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
)

func main() {
	endpoint := flag.String("endpoint", "unix:///csi/csi.sock", "CSI gRPC endpoint")
	haifyAddr := flag.String("haify-controller", "haify-controller:3374", "haify-controller gRPC address")
	nodeName := flag.String("node-name", os.Getenv("NODE_NAME"), "Kubernetes node name")
	nodeIP := flag.String("node-ip", os.Getenv("NODE_IP"), "this node's storage IP, registered with the controller at startup")
	qosInterval := flag.Duration("qos-interval", 30*time.Second,
		"how often to re-read the I/O limits of published volumes (changed through a VolumeAttributesClass) and rewrite io.max; 0 applies them only at publish")
	qosStateDir := flag.String("qos-state-dir", "/csi/qos",
		"where the published volumes whose I/O limits are kept current are recorded, so a restarted plugin still knows them; empty keeps them in memory")
	flag.Parse()

	log := logging.Production()
	maybeRunDRBDModule(log)
	defer func() { _ = log.Sync() }()
	log.Debug("main: csi-node starting", zap.String("endpoint", *endpoint), zap.String("haify_controller", *haifyAddr),
		zap.String("node", *nodeName), zap.String("node_ip", *nodeIP))

	if *nodeName == "" || *nodeIP == "" {
		log.Fatal("node-name and node-ip are required (set via downward API NODE_NAME / NODE_IP)")
	}

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

	// Auto-register this node into haify (idempotent on the controller side):
	// maps the k8s node name to its storage IP so topology and SSH line up.
	start := time.Now()
	if _, err := haify.RegisterNode(context.Background(), *nodeName, *nodeIP); err != nil {
		log.Warn("register node (continuing)", zap.Error(err))
	} else {
		log.Debug("main: registered node", zap.String("node", *nodeName), zap.String("node_ip", *nodeIP),
			zap.Duration("took", time.Since(start)))
	}

	ns := csi.NewNodeServer(haify, csi.NewMounter(), *nodeName, log)
	d := csi.NewDriver(*endpoint, log,
		csi.NewIdentityServer(),
		nil,
		ns,
	)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	log.Debug("main: enabling I/O limit upkeep", zap.Duration("interval", *qosInterval), zap.String("state_dir", *qosStateDir))
	csi.EnableQoSReconcile(ctx, ns, *qosStateDir, *qosInterval)
	if err := d.Run(ctx); err != nil {
		log.Error("driver exited", zap.Error(err))
		os.Exit(1)
	}
}
