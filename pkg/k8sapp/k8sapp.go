// Package k8sapp creates databases on Kubernetes whose data lives on a Haify
// volume. haify-mcp exposes it as the haify_k8s_app_create, haify_k8s_app_list and
// haify_k8s_app_delete tools.
package k8sapp

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"go.uber.org/zap"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// "I want an HA MySQL" becomes one tool call: a database on Kubernetes whose
// data lives on a Haify volume.
//
// High availability here comes from the storage, not from database
// replication. The volume is a DRBD resource with a replica on two nodes and a
// tiebreaker on a third; the database runs as a single pod. When its node
// dies, Kubernetes evicts the pod after failoverSeconds, schedules the
// replacement on the other replica node (the PV's node affinity allows nothing
// else), and the CSI driver promotes DRBD there. The committed data is already
// on that node — DRBD protocol C acknowledged every write on both. A node that
// is only cut off, not dead, loses quorum and freezes its I/O, so the two pods
// cannot both write.
//
// The shape of the workload follows from that:
//   - a Deployment with strategy Recreate, not a StatefulSet. A StatefulSet
//     never replaces a pod stuck on an unreachable node (it guarantees at most
//     one pod per identity, and cannot prove the old one is gone), so the
//     database would stay down until someone force-deleted it. DRBD quorum is
//     the fence that makes replacing it safe.
//   - ReadWriteOnce, not ReadWriteOncePod. The pod left on a dead node stays
//     Terminating and still counts as the one pod RWOP allows, which would
//     block the replacement exactly when it is needed.
//   - tolerations that evict after failoverSeconds instead of the default five
//     minutes.

const (
	managedByKey    = "app.kubernetes.io/managed-by"
	managedByValue  = "haify"
	templateKey     = "haify.io/app-template"
	haifyDriverName = "haify.csi.liliang-cn.com"
	failoverSeconds = int64(30)
	defaultSize     = "5Gi"
	defaultNS       = "default"
)

// template describes one application that can be created.
type template struct {
	image     string
	port      int32
	dataDir   string
	secretEnv string // env var the generated password is passed in
	extraEnv  []corev1.EnvVar
	probe     []string
}

var templates = map[string]template{
	"mysql": {
		image:     "mysql:8.4",
		port:      3306,
		dataDir:   "/var/lib/mysql",
		secretEnv: "MYSQL_ROOT_PASSWORD",
		probe:     []string{"mysqladmin", "ping", "-h", "127.0.0.1", "--silent"},
	},
	"postgres": {
		image:     "postgres:17",
		port:      5432,
		dataDir:   "/var/lib/postgresql/data",
		secretEnv: "POSTGRES_PASSWORD",
		// The image refuses a data directory that is a mount point with
		// lost+found in it; PGDATA one level down avoids that.
		extraEnv: []corev1.EnvVar{{Name: "PGDATA", Value: "/var/lib/postgresql/data/pgdata"}},
		probe:    []string{"pg_isready", "-h", "127.0.0.1"},
	},
}

// Request asks for one app. Only Template is required.
type Request struct {
	Template     string `json:"template"`
	Name         string `json:"name"`
	Namespace    string `json:"namespace"`
	Size         string `json:"size"`
	StorageClass string `json:"storageClass"`
	Image        string `json:"image"`
}

var dnsLabel = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)

// normalize fills defaults and rejects anything Kubernetes would refuse later,
// so a bad request fails before any object exists.
func (r *Request) normalize() (template, error) {
	log := zap.L()
	log.Debug("Request.normalize: validating app request", zap.String("template", r.Template),
		zap.String("name", r.Name), zap.String("namespace", r.Namespace), zap.String("size", r.Size),
		zap.String("storage_class", r.StorageClass), zap.String("image", r.Image))
	r.Template = strings.ToLower(strings.TrimSpace(r.Template))
	tpl, ok := templates[r.Template]
	if !ok {
		log.Debug("Request.normalize: rejected, unknown template", zap.String("template", r.Template))
		return tpl, fmt.Errorf("unknown template %q (available: %s)", r.Template, strings.Join(Templates(), ", "))
	}
	r.Name = strings.TrimSpace(r.Name)
	if r.Name == "" {
		log.Debug("Request.normalize: no name given, using the template name", zap.String("name", r.Template))
		r.Name = r.Template
	}
	if !dnsLabel.MatchString(r.Name) || len(r.Name) > 50 {
		log.Debug("Request.normalize: rejected, name is not a DNS label of at most 50 characters",
			zap.String("name", r.Name), zap.Int("length", len(r.Name)))
		return tpl, fmt.Errorf("name %q must be a lowercase DNS label of at most 50 characters", r.Name)
	}
	r.Namespace = strings.TrimSpace(r.Namespace)
	if r.Namespace == "" {
		log.Debug("Request.normalize: no namespace given, using the default", zap.String("namespace", defaultNS))
		r.Namespace = defaultNS
	}
	if !dnsLabel.MatchString(r.Namespace) {
		log.Debug("Request.normalize: rejected, namespace is not a DNS label", zap.String("namespace", r.Namespace))
		return tpl, fmt.Errorf("namespace %q is not a valid DNS label", r.Namespace)
	}
	if strings.TrimSpace(r.Size) == "" {
		log.Debug("Request.normalize: no size given, using the default", zap.String("size", defaultSize))
		r.Size = defaultSize
	}
	q, err := resource.ParseQuantity(r.Size)
	if err != nil || q.Sign() <= 0 {
		log.Debug("Request.normalize: rejected, size is not a positive quantity", zap.String("size", r.Size), zap.Error(err))
		return tpl, fmt.Errorf("size %q is not a positive quantity such as 5Gi", r.Size)
	}
	if r.Image == "" {
		log.Debug("Request.normalize: no image given, using the template image", zap.String("image", tpl.image))
		r.Image = tpl.image
	}
	log.Debug("Request.normalize: request accepted", zap.String("template", r.Template), zap.String("name", r.Name),
		zap.String("namespace", r.Namespace), zap.String("size", r.Size), zap.String("image", r.Image))
	return tpl, nil
}

func Templates() []string {
	names := make([]string, 0, len(templates))
	for n := range templates {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// objects renders the four objects of an app. The password is passed in so
// rendering stays deterministic and testable.
func objects(r Request, tpl template, storageClass, password string) (*corev1.Secret, *corev1.PersistentVolumeClaim, *appsv1.Deployment, *corev1.Service) {
	zap.L().Debug("objects: rendering app objects", zap.String("template", r.Template), zap.String("name", r.Name),
		zap.String("namespace", r.Namespace), zap.String("size", r.Size), zap.String("storage_class", storageClass),
		zap.String("image", r.Image), zap.Bool("has_password", password != ""))
	labels := map[string]string{
		"app.kubernetes.io/name":     r.Template,
		"app.kubernetes.io/instance": r.Name,
		managedByKey:                 managedByValue,
	}
	selector := map[string]string{"app.kubernetes.io/instance": r.Name, managedByKey: managedByValue}
	meta := func(name string) metav1.ObjectMeta {
		return metav1.ObjectMeta{Name: name, Namespace: r.Namespace, Labels: labels,
			Annotations: map[string]string{templateKey: r.Template}}
	}
	secretName, pvcName := r.Name+"-auth", r.Name+"-data"

	secret := &corev1.Secret{ObjectMeta: meta(secretName), Type: corev1.SecretTypeOpaque,
		StringData: map[string]string{"password": password}}

	sc := storageClass
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: meta(pvcName),
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			StorageClassName: &sc,
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(r.Size)},
			},
		},
	}

	env := append([]corev1.EnvVar{{
		Name: tpl.secretEnv,
		ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: secretName}, Key: "password"}},
	}}, tpl.extraEnv...)

	one := int32(1)
	evictAfter := failoverSeconds
	dep := &appsv1.Deployment{
		ObjectMeta: meta(r.Name),
		Spec: appsv1.DeploymentSpec{
			Replicas: &one,
			Strategy: appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType},
			Selector: &metav1.LabelSelector{MatchLabels: selector},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					Tolerations: []corev1.Toleration{
						{Key: "node.kubernetes.io/not-ready", Operator: corev1.TolerationOpExists,
							Effect: corev1.TaintEffectNoExecute, TolerationSeconds: &evictAfter},
						{Key: "node.kubernetes.io/unreachable", Operator: corev1.TolerationOpExists,
							Effect: corev1.TaintEffectNoExecute, TolerationSeconds: &evictAfter},
					},
					Containers: []corev1.Container{{
						Name:  r.Template,
						Image: r.Image,
						Env:   env,
						Ports: []corev1.ContainerPort{{Name: "db", ContainerPort: tpl.port}},
						VolumeMounts: []corev1.VolumeMount{{Name: "data", MountPath: tpl.dataDir,
							// A fresh ext4 volume carries lost+found, which both
							// images refuse as a data directory.
							SubPath: subPathFor(tpl)}},
						ReadinessProbe: &corev1.Probe{
							ProbeHandler:        corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: tpl.probe}},
							InitialDelaySeconds: 5, PeriodSeconds: 5,
						},
					}},
					Volumes: []corev1.Volume{{Name: "data", VolumeSource: corev1.VolumeSource{
						PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: pvcName}}}},
				},
			},
		},
	}

	svc := &corev1.Service{
		ObjectMeta: meta(r.Name),
		Spec: corev1.ServiceSpec{
			Selector: selector,
			Ports: []corev1.ServicePort{{Name: "db", Port: tpl.port,
				TargetPort: intstr.FromString("db")}},
		},
	}
	return secret, pvc, dep, svc
}

// subPathFor mounts MySQL one directory into the volume. Postgres does the
// same through PGDATA, so it mounts the volume root.
func subPathFor(tpl template) string {
	if tpl.secretEnv == "MYSQL_ROOT_PASSWORD" {
		zap.L().Debug("subPathFor: mysql data mounted one directory down", zap.String("sub_path", "mysql"))
		return "mysql"
	}
	zap.L().Debug("subPathFor: data mounted at the volume root", zap.String("image", tpl.image))
	return ""
}

func randomPassword() (string, error) {
	b := make([]byte, 18)
	if _, err := rand.Read(b); err != nil {
		zap.L().Debug("randomPassword: reading random bytes failed", zap.Error(err))
		return "", err
	}
	zap.L().Debug("randomPassword: generated password", zap.Int("random_bytes", len(b)))
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// Manager creates and reports apps on one Kubernetes cluster.
type Manager struct {
	kube kubernetes.Interface
}

// NewManagerFor wraps an existing client; tests pass a fake clientset.
func NewManagerFor(kube kubernetes.Interface) *Manager { return &Manager{kube: kube} }

// NewManager connects to Kubernetes with the kubeconfig at path, or from
// inside a pod when path is empty. It returns nil, nil when neither is
// available, so a caller can run without the app tools.
func NewManager(path string) (*Manager, error) {
	log := zap.L()
	log.Debug("NewManager: connecting to Kubernetes", zap.String("kubeconfig", path))
	var cfg *rest.Config
	var err error
	start := time.Now()
	if path != "" {
		log.Debug("NewManager: using the kubeconfig file", zap.String("kubeconfig", path))
		cfg, err = clientcmd.BuildConfigFromFlags("", path)
	} else if os.Getenv("KUBERNETES_SERVICE_HOST") != "" {
		log.Debug("NewManager: no kubeconfig, using the in-cluster config")
		cfg, err = rest.InClusterConfig()
	} else {
		log.Debug("NewManager: no kubeconfig and not in a pod, app tools unavailable")
		return nil, nil
	}
	if err != nil {
		log.Debug("NewManager: loading config failed", zap.Duration("took", time.Since(start)), zap.Error(err))
		return nil, fmt.Errorf("kubernetes config: %w", err)
	}
	cfg.Timeout = 20 * time.Second
	kube, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		log.Debug("NewManager: building the clientset failed", zap.String("api_host", cfg.Host), zap.Error(err))
		return nil, err
	}
	log.Debug("NewManager: connected", zap.String("api_host", cfg.Host), zap.Duration("timeout", cfg.Timeout),
		zap.Duration("took", time.Since(start)))
	return &Manager{kube: kube}, nil
}

// InvalidError is a request that failed validation before anything was
// created: an unknown template, a bad name, a StorageClass that is not Haify, an
// app that already exists.
type InvalidError struct{ err error }

func (e *InvalidError) Error() string { return e.err.Error() }
func (e *InvalidError) Unwrap() error { return e.err }
