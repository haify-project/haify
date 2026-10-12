package main

import (
	"context"
	"os"
	"strings"
	"time"

	"go.uber.org/zap"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
)

// healthLeaseName is the Lease the controller plugins of one cluster compete
// for. Only its holder runs the health reporter: every replica of the
// Deployment sweeping on its own would post each VolumeDegraded event once per
// replica.
const healthLeaseName = "haify-csi-health-reporter"

// serviceAccountNamespaceFile is where the kubelet puts the pod's namespace.
const serviceAccountNamespaceFile = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"

// leaseNamespace is the namespace of the health reporter's Lease: the flag when
// given, else the pod's own namespace.
func leaseNamespace(flagValue string) string {
	if flagValue != "" {
		return flagValue
	}
	if ns := os.Getenv("NAMESPACE"); ns != "" {
		return ns
	}
	if b, err := os.ReadFile(serviceAccountNamespaceFile); err == nil {
		if ns := strings.TrimSpace(string(b)); ns != "" {
			return ns
		}
	}
	return "default"
}

// leaseIdentity names this replica in the Lease. The pod name is unique where
// the hostname is not: the controller runs on the host network, so two replicas
// on one node would share a hostname.
func leaseIdentity() string {
	if name := os.Getenv("POD_NAME"); name != "" {
		return name
	}
	host, _ := os.Hostname()
	return host
}

// runWhileLeader calls run with a context that lives as long as this replica,
// named id, holds the Lease, and competes for it again each time it is lost,
// until ctx is cancelled. A standby replica takes over within the lease
// duration of the leader stopping; on an orderly shutdown the Lease is released
// at once.
func runWhileLeader(ctx context.Context, kube kubernetes.Interface, namespace, id string, log *zap.Logger, run func(context.Context)) {
	lock := &resourcelock.LeaseLock{
		LeaseMeta:  metav1.ObjectMeta{Name: healthLeaseName, Namespace: namespace},
		Client:     kube.CoordinationV1(),
		LockConfig: resourcelock.ResourceLockConfig{Identity: id},
	}
	for ctx.Err() == nil {
		leaderelection.RunOrDie(ctx, leaderelection.LeaderElectionConfig{
			Lock:            lock,
			Name:            healthLeaseName,
			LeaseDuration:   15 * time.Second,
			RenewDeadline:   10 * time.Second,
			RetryPeriod:     2 * time.Second,
			ReleaseOnCancel: true,
			Callbacks: leaderelection.LeaderCallbacks{
				OnStartedLeading: func(leading context.Context) {
					log.Info("health reporter: leading", zap.String("identity", id))
					run(leading)
				},
				OnStoppedLeading: func() {
					log.Info("health reporter: not leading", zap.String("identity", id))
				},
			},
		})
	}
}
