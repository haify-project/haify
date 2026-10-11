// haify-docker is the Docker volume plugin for Haify: `docker volume create
// -d haify` gives containers replicated volumes that follow them between hosts.
// It runs on every Docker host, which must be a registered Haify node.
package main

import (
	"context"
	"flag"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/haify-project/haify/pkg/client"
	"github.com/haify-project/haify/pkg/csi"
	"github.com/haify-project/haify/pkg/dockervol"
	"github.com/haify-project/haify/pkg/util"
	"go.uber.org/zap"
)

func main() {
	host, _ := os.Hostname()
	controller := flag.String("controller", "127.0.0.1:3374", "the Haify controller's gRPC address (its floating IP with Self-HA)")
	node := flag.String("node", strings.Split(host, ".")[0], "this host's Haify node name")
	socket := flag.String("socket", "/run/docker/plugins/haify.sock", "the socket Docker finds the plugin on")
	root := flag.String("root", "/var/lib/haify-docker/volumes", "where volumes are mounted")
	pool := flag.String("pool", "", "pool for volumes created without -o pool")
	size := flag.String("size", "10G", "size of volumes created without -o size")
	replicas := flag.Uint("replicas", 2, "copies of volumes created without -o replicas")
	fs := flag.String("fs", "ext4", "filesystem of volumes created without -o fs (ext4, xfs)")
	flag.Parse()

	log, _ := zap.NewProduction()
	defer func() { _ = log.Sync() }()

	sizeBytes, err := util.ParseSize(*size)
	if err != nil {
		log.Fatal("--size", zap.Error(err))
	}
	// HAIFY_TOKEN, else ~/.haify/token or /etc/haify/token.
	haify, err := client.NewHaifyClient(*controller, client.WithToken(client.ResolveToken("")))
	if err != nil {
		log.Fatal("connect to the controller", zap.Error(err))
	}
	defer func() { _ = haify.Close() }()

	d := dockervol.New(haify, csi.NewMounter(), *node, *root, dockervol.Defaults{
		SizeBytes: sizeBytes, Pool: *pool, Replicas: uint32(*replicas), FS: *fs,
	})
	ln, err := dockervol.Listen(*socket)
	if err != nil {
		log.Fatal("listen", zap.String("socket", *socket), zap.Error(err))
	}
	srv := &http.Server{Handler: dockervol.Handler(d), ReadHeaderTimeout: 10 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shut, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shut)
	}()
	log.Info("serving Docker volumes", zap.String("socket", *socket), zap.String("node", *node))
	if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		log.Fatal("serve", zap.Error(err))
	}
}
