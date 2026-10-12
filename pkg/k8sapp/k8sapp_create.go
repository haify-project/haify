package k8sapp

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// storageClassFor picks the StorageClass: the one asked for, which must be
// served by the Haify driver, or else a Haify class that keeps data on the node
// running the database — a class allowing remote access would let the pod land
// on a node whose every read and write crosses the network.
func (m *Manager) storageClassFor(ctx context.Context, want string) (string, error) {
	log := zap.L()
	log.Debug("Manager.storageClassFor: choosing storage class", zap.String("storage_class", want))
	start := time.Now()
	list, err := m.kube.StorageV1().StorageClasses().List(ctx, metav1.ListOptions{})
	if err != nil {
		log.Debug("Manager.storageClassFor: listing storage classes failed", zap.Duration("took", time.Since(start)), zap.Error(err))
		return "", fmt.Errorf("list storage classes: %w", err)
	}
	log.Debug("Manager.storageClassFor: listed storage classes", zap.Int("count", len(list.Items)),
		zap.Duration("took", time.Since(start)))
	var local, remote []string
	for _, sc := range list.Items {
		if sc.Provisioner != haifyDriverName {
			if sc.Name == want {
				log.Debug("Manager.storageClassFor: rejected, requested class is not Haify's",
					zap.String("storage_class", want), zap.String("provisioner", sc.Provisioner))
				return "", fmt.Errorf("storage class %q is served by %s, not Haify", want, sc.Provisioner)
			}
			log.Debug("Manager.storageClassFor: skipping class of another driver",
				zap.String("storage_class", sc.Name), zap.String("provisioner", sc.Provisioner))
			continue
		}
		if sc.Name == want {
			log.Debug("Manager.storageClassFor: using the requested class", zap.String("storage_class", want))
			return want, nil
		}
		if sc.Parameters["allowRemoteVolumeAccess"] == "true" {
			log.Debug("Manager.storageClassFor: Haify class allows remote access", zap.String("storage_class", sc.Name))
			remote = append(remote, sc.Name)
		} else {
			log.Debug("Manager.storageClassFor: Haify class keeps data local", zap.String("storage_class", sc.Name))
			local = append(local, sc.Name)
		}
	}
	if want != "" {
		log.Debug("Manager.storageClassFor: requested class not found", zap.String("storage_class", want))
		return "", fmt.Errorf("storage class %q not found", want)
	}
	sort.Strings(local)
	sort.Strings(remote)
	if len(local) > 0 {
		log.Debug("Manager.storageClassFor: chose a local class", zap.String("storage_class", local[0]),
			zap.Strings("local", local), zap.Strings("remote", remote))
		return local[0], nil
	}
	if len(remote) > 0 {
		log.Debug("Manager.storageClassFor: no local class, chose a remote-access class",
			zap.String("storage_class", remote[0]), zap.Strings("remote", remote))
		return remote[0], nil
	}
	log.Debug("Manager.storageClassFor: no Haify storage class found")
	return "", errors.New("no StorageClass uses the Haify CSI driver; install deploy/k8s first")
}

// Created is what POST /ai/apps returns.
type Created struct {
	Message      string `json:"message"`
	Namespace    string `json:"namespace"`
	Name         string `json:"name"`
	Service      string `json:"service"`
	Secret       string `json:"secret"`
	StorageClass string `json:"storageClass"`
}

// create makes the app's objects. It never overwrites: an object that already
// exists is an error, because silently adopting someone's Deployment or
// replacing a Secret would lose a password or a workload.
func (m *Manager) Create(ctx context.Context, r Request) (*Created, error) {
	log := zap.L()
	log.Debug("Manager.Create: creating app", zap.String("template", r.Template), zap.String("name", r.Name),
		zap.String("namespace", r.Namespace), zap.String("size", r.Size), zap.String("storage_class", r.StorageClass),
		zap.String("image", r.Image))
	tpl, err := r.normalize()
	if err != nil {
		log.Debug("Manager.Create: invalid request", zap.Error(err))
		return nil, &InvalidError{err}
	}
	sc, err := m.storageClassFor(ctx, r.StorageClass)
	if err != nil {
		log.Debug("Manager.Create: no usable storage class", zap.Error(err))
		return nil, &InvalidError{err}
	}
	start := time.Now()
	_, err = m.kube.AppsV1().Deployments(r.Namespace).Get(ctx, r.Name, metav1.GetOptions{})
	log.Debug("Manager.Create: checked for an existing deployment", zap.String("namespace", r.Namespace),
		zap.String("name", r.Name), zap.Bool("found", err == nil), zap.Duration("took", time.Since(start)), zap.Error(err))
	if err == nil {
		log.Debug("Manager.Create: rejected, deployment already exists", zap.String("name", r.Name))
		return nil, &InvalidError{fmt.Errorf("a Deployment %s/%s already exists", r.Namespace, r.Name)}
	}
	start = time.Now()
	_, err = m.kube.CoreV1().Namespaces().Get(ctx, r.Namespace, metav1.GetOptions{})
	log.Debug("Manager.Create: checked namespace", zap.String("namespace", r.Namespace), zap.Bool("found", err == nil),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if apierrors.IsNotFound(err) {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: r.Namespace,
			Labels: map[string]string{managedByKey: managedByValue}}}
		start = time.Now()
		_, err := m.kube.CoreV1().Namespaces().Create(ctx, ns, metav1.CreateOptions{})
		log.Debug("Manager.Create: created namespace", zap.String("namespace", r.Namespace),
			zap.Duration("took", time.Since(start)), zap.Error(err))
		if err != nil && !apierrors.IsAlreadyExists(err) {
			return nil, fmt.Errorf("create namespace: %w", err)
		}
	}
	pw, err := randomPassword()
	if err != nil {
		log.Debug("Manager.Create: generating password failed", zap.Error(err))
		return nil, err
	}
	secret, pvc, dep, svc := objects(r, tpl, sc, pw)

	// A claim and secret kept by an earlier Delete of the same app are used
	// again: the data and the password that opens it belong together. Both are
	// checked before anything is created.
	core := m.kube.CoreV1()
	keepSecret, keepClaim := false, false
	start = time.Now()
	old, err := core.Secrets(r.Namespace).Get(ctx, secret.Name, metav1.GetOptions{})
	log.Debug("Manager.Create: checked for a kept secret", zap.String("secret", secret.Name), zap.Bool("found", err == nil),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err == nil {
		if keepSecret, err = keptObject("secret", secret.Name, r.Template, old.Labels, old.Annotations); err != nil {
			return nil, err
		}
	} else if !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("get secret: %w", err)
	}
	start = time.Now()
	oldClaim, err := core.PersistentVolumeClaims(r.Namespace).Get(ctx, pvc.Name, metav1.GetOptions{})
	log.Debug("Manager.Create: checked for a kept claim", zap.String("claim", pvc.Name), zap.Bool("found", err == nil),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err == nil {
		if keepClaim, err = keptObject("claim", pvc.Name, r.Template, oldClaim.Labels, oldClaim.Annotations); err != nil {
			return nil, err
		}
	} else if !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("get volume claim: %w", err)
	}
	log.Debug("Manager.Create: reuse decision", zap.Bool("keep_secret", keepSecret), zap.Bool("keep_claim", keepClaim))
	if keepClaim && !keepSecret {
		log.Debug("Manager.Create: rejected, kept claim has lost its secret", zap.String("claim", pvc.Name),
			zap.String("secret", secret.Name))
		return nil, &InvalidError{fmt.Errorf("claim %s is kept from an earlier %s but its secret %s is gone, so "+
			"its password is lost; delete the claim or restore the secret", pvc.Name, r.Name, secret.Name)}
	}

	if !keepSecret {
		start = time.Now()
		_, err := core.Secrets(r.Namespace).Create(ctx, secret, metav1.CreateOptions{})
		log.Debug("Manager.Create: created secret", zap.String("secret", secret.Name),
			zap.Duration("took", time.Since(start)), zap.Error(err))
		if err != nil {
			return nil, fmt.Errorf("create secret: %w", err)
		}
	}
	if !keepClaim {
		start = time.Now()
		_, err := core.PersistentVolumeClaims(r.Namespace).Create(ctx, pvc, metav1.CreateOptions{})
		log.Debug("Manager.Create: created claim", zap.String("claim", pvc.Name), zap.String("storage_class", sc),
			zap.Duration("took", time.Since(start)), zap.Error(err))
		if err != nil {
			return nil, fmt.Errorf("create volume claim: %w", err)
		}
	}
	start = time.Now()
	_, err = m.kube.CoreV1().Services(r.Namespace).Create(ctx, svc, metav1.CreateOptions{})
	log.Debug("Manager.Create: created service", zap.String("service", svc.Name),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return nil, fmt.Errorf("create service: %w", err)
	}
	start = time.Now()
	_, err = m.kube.AppsV1().Deployments(r.Namespace).Create(ctx, dep, metav1.CreateOptions{})
	log.Debug("Manager.Create: created deployment", zap.String("deployment", dep.Name),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return nil, fmt.Errorf("create deployment: %w", err)
	}
	host := fmt.Sprintf("%s.%s.svc:%d", r.Name, r.Namespace, tpl.port)
	msg := fmt.Sprintf("%s %s/%s created on %s (%s); connect at %s, password in secret %s",
		r.Template, r.Namespace, r.Name, sc, r.Size, host, secret.Name)
	if keepClaim {
		msg = fmt.Sprintf("%s %s/%s created on its kept claim %s; connect at %s, password in secret %s",
			r.Template, r.Namespace, r.Name, pvc.Name, host, secret.Name)
	}
	log.Debug("Manager.Create: app created", zap.String("namespace", r.Namespace), zap.String("name", r.Name),
		zap.String("service", host), zap.String("storage_class", sc), zap.Bool("kept_claim", keepClaim))
	return &Created{
		Message:   msg,
		Namespace: r.Namespace, Name: r.Name, Service: host, Secret: secret.Name, StorageClass: sc,
	}, nil
}
