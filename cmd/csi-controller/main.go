package main

import (
	"context"
	"flag"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/haify-project/haify/pkg/logging"

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

func main() {
	endpoint := flag.String("endpoint", "unix:///csi/csi.sock", "CSI gRPC endpoint")
	haifyAddr := flag.String("haify-controller", "haify-controller:3374", "haify-controller gRPC address")
	healthInterval := flag.Duration("health-interval", time.Minute,
		"how often to check each volume's replicas and post VolumeDegraded/VolumeRecovered events on its PVC and the Pods using it; 0 disables")
	leaderElection := flag.Bool("leader-election", false,
		"run the health reporter only on the replica holding the "+healthLeaseName+" Lease, and the HA controller on the one holding "+
			haFailoverLeaseName+"; set it when the Deployment has more than one replica")
	leaderElectionNamespace := flag.String("leader-election-namespace", "",
		"namespace of the Lease (default: the pod's own namespace)")
	haFailover := registerHAFailoverFlags()
	flag.Parse()

	log := logging.Production()
	defer func() { _ = log.Sync() }()
	log.Debug("main: csi-controller starting", zap.String("endpoint", *endpoint), zap.String("haify_controller", *haifyAddr),
		zap.Duration("health_interval", *healthInterval), zap.Bool("leader_election", *leaderElection),
		zap.String("leader_election_namespace", *leaderElectionNamespace))

	// With [auth] or [rbac] on the controller, every call needs a token:
	// HAIFY_TOKEN (the manifests fill it from the haify-api-token Secret),
	// else ~/.haify/token or /etc/haify/token.
	token := client.ResolveToken("")
	log.Debug("main: connecting to haify-controller", zap.String("addr", *haifyAddr), zap.Bool("has_token", token != ""))
	haify, err := client.NewHaifyClient(*haifyAddr, client.WithToken(token))
	if err != nil {
		log.Fatal("connect haify-controller", zap.Error(err))
	}
	// Runs on an orderly shutdown, once the driver has stopped serving. All this
	// can report is that tearing down an idle gRPC transport was untidy, on a
	// process that is about to exit and has nothing left to retry, so the error
	// is dropped rather than logged as a shutdown failure.
	defer func() { _ = haify.Close() }()

	d := csi.NewDriver(*endpoint, log,
		csi.NewIdentityServer(),
		csi.NewControllerServer(haify, log),
		nil,
	)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if *healthInterval > 0 {
		lease := ""
		if *leaderElection {
			lease = leaseNamespace(*leaderElectionNamespace)
		}
		log.Debug("main: starting health reporter", zap.Duration("interval", *healthInterval), zap.String("lease_namespace", lease))
		startHealthReporter(ctx, haify, *healthInterval, lease, log)
	} else {
		log.Debug("main: health reporter disabled by --health-interval=0")
	}

	if *haFailover.enabled {
		lease := ""
		if *leaderElection {
			lease = leaseNamespace(*leaderElectionNamespace)
		}
		startHAFailover(ctx, haify, haFailover, lease, log)
	} else {
		log.Debug("main: HA failover disabled by --ha-failover=false")
	}

	if err := d.Run(ctx); err != nil {
		log.Error("driver exited", zap.Error(err))
		os.Exit(1)
	}
}

// startHealthReporter posts replica health on PVCs and their Pods. It needs the
// Kubernetes API, so outside a cluster it logs why it is off and the driver runs without it —
// provisioning does not depend on it. With leaseNamespace set it runs only while
// this replica holds the Lease there.
func startHealthReporter(ctx context.Context, haify csi.HaifyBackend, interval time.Duration, leaseNamespace string, log *zap.Logger) {
	log.Debug("startHealthReporter: loading in-cluster config", zap.Duration("interval", interval),
		zap.String("lease_namespace", leaseNamespace))
	cfg, err := rest.InClusterConfig()
	if err != nil {
		log.Warn("volume health reporting disabled: not running in a cluster", zap.Error(err))
		return
	}
	kube, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		log.Warn("volume health reporting disabled: kubernetes client", zap.Error(err))
		return
	}
	broadcaster := record.NewBroadcaster()
	broadcaster.StartRecordingToSink(&typedcorev1.EventSinkImpl{Interface: kube.CoreV1().Events("")})
	recorder := broadcaster.NewRecorder(scheme.Scheme, corev1.EventSource{Component: csi.DriverName})
	go func() {
		<-ctx.Done()
		log.Debug("startHealthReporter: context done, shutting down event broadcaster")
		broadcaster.Shutdown()
	}()
	run := func(ctx context.Context) {
		// A fresh reporter per term: what a previous term saw may be stale by
		// the time this replica leads again.
		log.Debug("startHealthReporter: starting a reporter term")
		csi.NewHealthReporter(haify, kube, recorder, interval, log).Run(ctx)
	}
	if leaseNamespace == "" {
		log.Debug("startHealthReporter: no leader election, running directly")
		go run(ctx)
	} else {
		log.Debug("startHealthReporter: running under leader election", zap.String("namespace", leaseNamespace))
		go runWhileLeader(ctx, kube, leaseNamespace, leaseIdentity(), log, run)
	}
	log.Info("volume health reporting enabled", zap.Duration("interval", interval),
		zap.Bool("leader_election", leaseNamespace != ""))
}
