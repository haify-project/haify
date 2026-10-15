package main

import (
	"context"
	"flag"
	"time"

	"github.com/haify-project/haify/pkg/csi"
	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/record"
)

// haFailoverLeaseName is the Lease the controller plugins compete for to run
// the HA controller. It is separate from the health reporter's so either can
// move to another replica without the other.
const haFailoverLeaseName = "haify-csi-ha-failover"

// haFailoverFlags are the HA controller's command-line settings.
type haFailoverFlags struct {
	enabled         *bool
	interval        *time.Duration
	grace           *time.Duration
	maxLostFraction *float64
}

// registerHAFailoverFlags declares the HA controller's flags on fs; call it
// before fs.Parse.
func registerHAFailoverFlags(fs *flag.FlagSet) haFailoverFlags {
	return haFailoverFlags{
		enabled: fs.Bool("ha-failover", true,
			"move Pods off a node that has dropped out of its volumes' DRBD resources while the other replicas keep quorum: "+
				"taint the node "+csi.FailoverTaintKey+":NoSchedule and force-delete the Pods so their controller recreates them elsewhere"),
		interval: fs.Duration("ha-failover-interval", 5*time.Second,
			"how often the HA controller checks the volumes Pods are using"),
		grace: fs.Duration("ha-failover-grace", 10*time.Second,
			"how long a node must look lost to Haify before the HA controller acts while Kubernetes still reports the node Ready; "+
				"a node Kubernetes reports NotReady or Unknown is acted on at once"),
		maxLostFraction: fs.Float64("ha-failover-max-lost-fraction", 0.5,
			"the HA controller does nothing while more than this fraction of the cluster's nodes look lost, treating it as a partition"),
	}
}

// startHAFailover runs the HA controller. Like the health reporter it needs
// the Kubernetes API, so outside a cluster it logs why it is off. With
// leaseNamespace set it runs only while this replica holds its Lease there.
func startHAFailover(ctx context.Context, haify csi.HaifyBackend, f haFailoverFlags, leaseNamespace string, log *zap.Logger) {
	log.Debug("startHAFailover: loading in-cluster config", zap.Duration("interval", *f.interval),
		zap.Duration("grace", *f.grace), zap.Float64("max_lost_fraction", *f.maxLostFraction),
		zap.String("lease_namespace", leaseNamespace))
	cfg, err := rest.InClusterConfig()
	if err != nil {
		log.Warn("HA failover disabled: not running in a cluster", zap.Error(err))
		return
	}
	kube, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		log.Warn("HA failover disabled: kubernetes client", zap.Error(err))
		return
	}
	broadcaster := record.NewBroadcaster()
	broadcaster.StartRecordingToSink(&typedcorev1.EventSinkImpl{Interface: kube.CoreV1().Events("")})
	recorder := broadcaster.NewRecorder(scheme.Scheme, corev1.EventSource{Component: csi.DriverName})
	go func() {
		<-ctx.Done()
		log.Debug("startHAFailover: context done, shutting down event broadcaster")
		broadcaster.Shutdown()
	}()
	conf := csi.HAFailoverConfig{Interval: *f.interval, Grace: *f.grace, MaxLostFraction: *f.maxLostFraction}
	run := func(ctx context.Context) {
		// A fresh controller per term: grace timers a previous term started
		// may be stale by the time this replica leads again.
		log.Debug("startHAFailover: starting a term")
		csi.NewHAFailover(haify, kube, recorder, conf, log).Run(ctx)
	}
	if leaseNamespace == "" {
		log.Debug("startHAFailover: no leader election, running directly")
		go run(ctx)
	} else {
		log.Debug("startHAFailover: running under leader election", zap.String("namespace", leaseNamespace))
		go runWhileLeaderOf(ctx, kube, haFailoverLeaseName, leaseNamespace, leaseIdentity(), log, run)
	}
	log.Info("HA failover enabled", zap.Duration("interval", *f.interval), zap.Duration("grace", *f.grace),
		zap.Bool("leader_election", leaseNamespace != ""))
}
