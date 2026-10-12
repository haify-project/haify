package main

import (
	"context"
	"flag"
	"os"
	"os/signal"
	"syscall"

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
	flag.Parse()

	log := logging.Production()
	defer func() { _ = log.Sync() }()

	if *nodeName == "" || *nodeIP == "" {
		log.Fatal("node-name and node-ip are required (set via downward API NODE_NAME / NODE_IP)")
	}

	// With [auth] or [rbac] on the controller, every call needs a token:
	// HAIFY_TOKEN (the manifests fill it from the haify-api-token Secret),
	// else ~/.haify/token or /etc/haify/token.
	haify, err := client.NewHaifyClient(*haifyAddr, client.WithToken(client.ResolveToken("")))
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
	if _, err := haify.RegisterNode(context.Background(), *nodeName, *nodeIP); err != nil {
		log.Warn("register node (continuing)", zap.Error(err))
	}

	d := csi.NewDriver(*endpoint, log,
		csi.NewIdentityServer(),
		nil,
		csi.NewNodeServer(haify, csi.NewMounter(), *nodeName, log),
	)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := d.Run(ctx); err != nil {
		log.Error("driver exited", zap.Error(err))
		os.Exit(1)
	}
}
