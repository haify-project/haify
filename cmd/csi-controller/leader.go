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
	log := zap.L()
	if flagValue != "" {
		log.Debug("leaseNamespace: from flag", zap.String("namespace", flagValue))
		return flagValue
	}
	if ns := os.Getenv("NAMESPACE"); ns != "" {
		log.Debug("leaseNamespace: from NAMESPACE", zap.String("namespace", ns))
		return ns
	}
	b, err := os.ReadFile(serviceAccountNamespaceFile)
	log.Debug("leaseNamespace: read service account namespace file", zap.String("path", serviceAccountNamespaceFile),
		zap.Int("size", len(b)), zap.Error(err))
	if err == nil {
		if ns := strings.TrimSpace(string(b)); ns != "" {
			return ns
		}
	}
	log.Debug("leaseNamespace: falling back to default")
	return "default"
}

// leaseIdentity names this replica in the Lease. The pod name is unique where
// the hostname is not: the controller runs on the host network, so two replicas
// on one node would share a hostname.
func leaseIdentity() string {
	if name := os.Getenv("POD_NAME"); name != "" {
		zap.L().Debug("leaseIdentity: from POD_NAME", zap.String("identity", name))
		return name
	}
	host, err := os.Hostname()
	zap.L().Debug("leaseIdentity: from hostname", zap.String("identity", host), zap.Error(err))
	return host
}

// runWhileLeader calls run with a context that lives as long as this replica,
// named id, holds the health reporter's Lease, and competes for it again each
// time it is lost, until ctx is cancelled. A standby replica takes over within
// the lease duration of the leader stopping; on an orderly shutdown the Lease
// is released at once.
func runWhileLeader(ctx context.Context, kube kubernetes.Interface, namespace, id string, log *zap.Logger, run func(context.Context)) {
	runWhileLeaderOf(ctx, kube, healthLeaseName, namespace, id, log, run)
}

// runWhileLeaderOf is runWhileLeader for the Lease named lease. Each job that
// runs on one replica only has a Lease of its own, so the jobs move between
// replicas independently.
func runWhileLeaderOf(ctx context.Context, kube kubernetes.Interface, lease, namespace, id string, log *zap.Logger, run func(context.Context)) {
	lock := &resourcelock.LeaseLock{
		LeaseMeta:  metav1.ObjectMeta{Name: lease, Namespace: namespace},
		Client:     kube.CoordinationV1(),
		LockConfig: resourcelock.ResourceLockConfig{Identity: id},
	}
	log.Debug("runWhileLeaderOf: competing for lease", zap.String("lease", lease), zap.String("namespace", namespace),
		zap.String("identity", id))
	for ctx.Err() == nil {
		log.Debug("runWhileLeaderOf: starting an election round", zap.String("lease", lease), zap.String("identity", id))
		leaderelection.RunOrDie(ctx, leaderelection.LeaderElectionConfig{
			Lock:            lock,
			Name:            lease,
			LeaseDuration:   15 * time.Second,
			RenewDeadline:   10 * time.Second,
			RetryPeriod:     2 * time.Second,
			ReleaseOnCancel: true,
			Callbacks: leaderelection.LeaderCallbacks{
				OnStartedLeading: func(leading context.Context) {
					log.Info("leading", zap.String("lease", lease), zap.String("identity", id))
					run(leading)
				},
				OnStoppedLeading: func() {
					log.Info("not leading", zap.String("lease", lease), zap.String("identity", id))
				},
				OnNewLeader: func(identity string) {
					log.Debug("runWhileLeaderOf: observed leader", zap.String("lease", lease), zap.String("leader", identity),
						zap.String("identity", id))
				},
			},
		})
	}
	log.Debug("runWhileLeaderOf: context done, stopped competing", zap.String("lease", lease), zap.String("identity", id))
}
