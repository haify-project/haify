package k8sapp

import (
	"context"
	"fmt"
	"strings"
	"time"

	"go.uber.org/zap"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// DeleteRequest names an app to delete. Without DeleteData the claim holding
// the data and the secret holding its password are kept, so creating the app
// again with the same name picks both up.
type DeleteRequest struct {
	Namespace  string `json:"namespace"`
	Name       string `json:"name"`
	DeleteData bool   `json:"deleteData"`
}

// Deleted is what Delete reports.
type Deleted struct {
	Message string   `json:"message"`
	Removed []string `json:"removed"`
	Kept    []string `json:"kept,omitempty"`
}

// managedBy reports whether an object carries the label Create puts on
// everything it makes.
func managedBy(labels map[string]string) bool { return labels[managedByKey] == managedByValue }

// Delete removes an app Create made. It deletes only objects carrying Create's
// label, and refuses outright when the Deployment does not: a name that
// happens to match must not take someone else's workload or volume with it.
//
// The Deployment goes first, so its pod releases the volume before the claim
// (with DeleteData) is deleted; the Haify StorageClass's reclaim policy then
// decides what happens to the DRBD resource behind it.
func (m *Manager) Delete(ctx context.Context, r DeleteRequest) (*Deleted, error) {
	log := zap.L()
	log.Debug("Manager.Delete: deleting app", zap.String("namespace", r.Namespace), zap.String("name", r.Name),
		zap.Bool("delete_data", r.DeleteData))
	r.Namespace = strings.TrimSpace(r.Namespace)
	if r.Namespace == "" {
		log.Debug("Manager.Delete: no namespace given, using the default", zap.String("namespace", defaultNS))
		r.Namespace = defaultNS
	}
	r.Name = strings.TrimSpace(r.Name)
	if !dnsLabel.MatchString(r.Name) || !dnsLabel.MatchString(r.Namespace) {
		log.Debug("Manager.Delete: rejected, name or namespace is not a DNS label",
			zap.String("namespace", r.Namespace), zap.String("name", r.Name))
		return nil, &InvalidError{fmt.Errorf("%q in namespace %q is not a valid app name", r.Name, r.Namespace)}
	}
	start := time.Now()
	dep, err := m.kube.AppsV1().Deployments(r.Namespace).Get(ctx, r.Name, metav1.GetOptions{})
	log.Debug("Manager.Delete: got deployment", zap.String("namespace", r.Namespace), zap.String("name", r.Name),
		zap.Bool("found", err == nil), zap.Duration("took", time.Since(start)), zap.Error(err))
	if apierrors.IsNotFound(err) {
		log.Debug("Manager.Delete: rejected, no such app", zap.String("namespace", r.Namespace), zap.String("name", r.Name))
		return nil, &InvalidError{fmt.Errorf("no app %s/%s", r.Namespace, r.Name)}
	}
	if err != nil {
		log.Debug("Manager.Delete: get deployment failed", zap.String("name", r.Name), zap.Error(err))
		return nil, fmt.Errorf("get deployment: %w", err)
	}
	if !managedBy(dep.Labels) {
		log.Debug("Manager.Delete: refused, deployment lacks the managed-by label",
			zap.String("namespace", r.Namespace), zap.String("name", r.Name), zap.String("managed_by", dep.Labels[managedByKey]))
		return nil, &InvalidError{fmt.Errorf("deployment %s/%s was not created by haify_k8s_app_create; refusing to delete it",
			r.Namespace, r.Name)}
	}

	out := &Deleted{}
	del := func(kind, name string, get func() (map[string]string, error), remove func() error) error {
		start := time.Now()
		labels, err := get()
		log.Debug("Manager.Delete: got object", zap.String("kind", kind), zap.String("object", name),
			zap.Bool("found", err == nil), zap.Duration("took", time.Since(start)), zap.Error(err))
		if apierrors.IsNotFound(err) {
			log.Debug("Manager.Delete: object already gone, skipping", zap.String("kind", kind), zap.String("object", name))
			return nil
		}
		if err != nil {
			log.Debug("Manager.Delete: get object failed", zap.String("kind", kind), zap.String("object", name), zap.Error(err))
			return fmt.Errorf("get %s %s: %w", kind, name, err)
		}
		if !managedBy(labels) {
			log.Debug("Manager.Delete: keeping object not created by Haify", zap.String("kind", kind), zap.String("object", name))
			out.Kept = append(out.Kept, fmt.Sprintf("%s %s (not created by Haify)", kind, name))
			return nil
		}
		start = time.Now()
		err = remove()
		log.Debug("Manager.Delete: deleted object", zap.String("kind", kind), zap.String("object", name),
			zap.Duration("took", time.Since(start)), zap.Error(err))
		if err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete %s %s: %w", kind, name, err)
		}
		out.Removed = append(out.Removed, kind+" "+name)
		return nil
	}
	opts := metav1.DeleteOptions{}
	apps, core := m.kube.AppsV1(), m.kube.CoreV1()
	ns, name := r.Namespace, r.Name
	start = time.Now()
	err = apps.Deployments(ns).Delete(ctx, name, opts)
	log.Debug("Manager.Delete: deleted deployment", zap.String("namespace", ns), zap.String("name", name),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil && !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("delete deployment: %w", err)
	}
	out.Removed = append(out.Removed, "deployment "+name)
	if err := del("service", name, func() (map[string]string, error) {
		s, err := core.Services(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
		return s.Labels, nil
	}, func() error { return core.Services(ns).Delete(ctx, name, opts) }); err != nil {
		log.Debug("Manager.Delete: service removal failed", zap.String("name", name), zap.Error(err))
		return nil, err
	}

	claim, secret := name+"-data", name+"-auth"
	if !r.DeleteData {
		log.Debug("Manager.Delete: keeping claim and secret, delete_data not set",
			zap.String("claim", claim), zap.String("secret", secret))
		out.Kept = append(out.Kept, "claim "+claim, "secret "+secret)
		out.Message = fmt.Sprintf("app %s/%s deleted; its data (claim %s) and password (secret %s) are kept, and "+
			"creating the app again with the same name and template uses them", ns, name, claim, secret)
		log.Debug("Manager.Delete: done", zap.Strings("removed", out.Removed), zap.Strings("kept", out.Kept))
		return out, nil
	}
	if err := del("claim", claim, func() (map[string]string, error) {
		p, err := core.PersistentVolumeClaims(ns).Get(ctx, claim, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
		return p.Labels, nil
	}, func() error { return core.PersistentVolumeClaims(ns).Delete(ctx, claim, opts) }); err != nil {
		log.Debug("Manager.Delete: claim removal failed", zap.String("claim", claim), zap.Error(err))
		return nil, err
	}
	if err := del("secret", secret, func() (map[string]string, error) {
		s, err := core.Secrets(ns).Get(ctx, secret, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
		return s.Labels, nil
	}, func() error { return core.Secrets(ns).Delete(ctx, secret, opts) }); err != nil {
		log.Debug("Manager.Delete: secret removal failed", zap.String("secret", secret), zap.Error(err))
		return nil, err
	}
	out.Message = fmt.Sprintf("app %s/%s and its data deleted", ns, name)
	log.Debug("Manager.Delete: done", zap.Strings("removed", out.Removed), zap.Strings("kept", out.Kept))
	return out, nil
}

// keptObject reports whether an object Create is about to make was kept by
// an earlier Delete of the same app: it carries Create's label and was made
// for the same template. Anything else of that name is in the way.
func keptObject(kind, name, template string, labels, annotations map[string]string) (bool, error) {
	if managedBy(labels) && annotations[templateKey] == template {
		zap.L().Debug("keptObject: reusing object kept by an earlier delete", zap.String("kind", kind),
			zap.String("object", name), zap.String("template", template))
		return true, nil
	}
	zap.L().Debug("keptObject: object of that name is in the way", zap.String("kind", kind), zap.String("object", name),
		zap.String("template", template), zap.Bool("managed", managedBy(labels)),
		zap.String("object_template", annotations[templateKey]))
	return false, &InvalidError{fmt.Errorf("a %s %s already exists and is not a kept %s app's; choose another name", kind, name, template)}
}
