package main

import (
	"context"
	"flag"
	"strings"
	"time"

	"github.com/haify-project/haify/pkg/client"
	"github.com/haify-project/haify/pkg/csi"
	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/record"
)

// nodeDrainLeaseName is the Lease the controller plugins compete for to run
// the drain controller. It has one of its own so it moves between replicas
// independently of the health reporter and the HA controller.
const nodeDrainLeaseName = "haify-csi-node-drain"

// nodeDrainFlags are the drain controller's command-line settings.
type nodeDrainFlags struct {
	enabled  *bool
	onCordon *bool
	taints   *string
	delay    *time.Duration
	interval *time.Duration
	maxNodes *int
}

// registerNodeDrainFlags declares the drain controller's flags on fs; call it
// before fs.Parse.
func registerNodeDrainFlags(fs *flag.FlagSet) nodeDrainFlags {
	return nodeDrainFlags{
		enabled: fs.Bool("node-drain", true,
			"drain a node in Haify (as `haify node drain` does) when Kubernetes takes it down for maintenance, and undrain it afterwards: "+
				"a node annotated or labelled "+csi.DrainRequestKey+"=true, or carrying one of --node-drain-taints"),
		onCordon: fs.Bool("node-drain-on-cordon", false,
			"also drain any node that is cordoned (kubectl cordon, kubectl drain), without the annotation or a maintenance taint"),
		taints: fs.String("node-drain-taints", strings.Join(csi.DefaultDrainTaints, ","),
			"comma-separated taint keys that mark a node as under maintenance; empty for none"),
		delay: fs.Duration("node-drain-delay", 30*time.Second,
			"how long a node must keep asking to be drained before it is drained in Haify"),
		interval: fs.Duration("node-drain-interval", 10*time.Second,
			"how often the drain controller looks at the nodes"),
		maxNodes: fs.Int("node-drain-max-nodes", 1,
			"how many nodes may be drained in Haify at the same time; further ones wait"),
	}
}

// config turns the flags into the drain controller's configuration.
func (f nodeDrainFlags) config() csi.NodeDrainConfig {
	conf := csi.NodeDrainConfig{Interval: *f.interval, Delay: *f.delay, OnCordon: *f.onCordon, MaxNodes: *f.maxNodes}
	if conf.Delay == 0 {
		// The configuration reads zero as "default"; a flag set to 0 means no delay.
		conf.Delay = -1
	}
	for _, key := range strings.Split(*f.taints, ",") {
		if key = strings.TrimSpace(key); key != "" {
			conf.Taints = append(conf.Taints, key)
		}
	}
	return conf
}

// startNodeDrain runs the drain controller. Like the HA controller it needs
// the Kubernetes API, so outside a cluster it logs why it is off. With
// leaseNamespace set it runs only while this replica holds its Lease there.
func startNodeDrain(ctx context.Context, haify *client.HaifyClient, f nodeDrainFlags, leaseNamespace string, log *zap.Logger) {
	conf := f.config()
	log.Debug("startNodeDrain: loading in-cluster config", zap.Duration("interval", conf.Interval), zap.Duration("delay", *f.delay),
		zap.Bool("on_cordon", conf.OnCordon), zap.Strings("taints", conf.Taints), zap.Int("max_nodes", conf.MaxNodes),
		zap.String("lease_namespace", leaseNamespace))
	cfg, err := rest.InClusterConfig()
	if err != nil {
		log.Warn("node drain disabled: not running in a cluster", zap.Error(err))
		return
	}
	kube, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		log.Warn("node drain disabled: kubernetes client", zap.Error(err))
		return
	}
	broadcaster := record.NewBroadcaster()
	broadcaster.StartRecordingToSink(&typedcorev1.EventSinkImpl{Interface: kube.CoreV1().Events("")})
	recorder := broadcaster.NewRecorder(scheme.Scheme, corev1.EventSource{Component: csi.DriverName})
	go func() {
		<-ctx.Done()
		log.Debug("startNodeDrain: context done, shutting down event broadcaster")
		broadcaster.Shutdown()
	}()
	run := func(ctx context.Context) {
		// A fresh controller per term: the delays a previous term counted
		// start over, and what it did is read back from the Nodes.
		log.Debug("startNodeDrain: starting a term")
		csi.NewNodeDrain(haify, kube, recorder, conf, log).Run(ctx)
	}
	if leaseNamespace == "" {
		log.Debug("startNodeDrain: no leader election, running directly")
		go run(ctx)
	} else {
		log.Debug("startNodeDrain: running under leader election", zap.String("namespace", leaseNamespace))
		go runWhileLeaderOf(ctx, kube, nodeDrainLeaseName, leaseNamespace, leaseIdentity(), log, run)
	}
	log.Info("node drain enabled", zap.Duration("interval", conf.Interval), zap.Duration("delay", *f.delay),
		zap.Bool("on_cordon", conf.OnCordon), zap.Strings("taints", conf.Taints), zap.Bool("leader_election", leaseNamespace != ""))
}
