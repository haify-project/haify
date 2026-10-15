package main

import (
	"context"
	"os"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/haify-project/haify/pkg/client"
	hcsi "github.com/haify-project/haify/pkg/csi"
	"go.uber.org/zap"
)

type csiNodeServer = csi.NodeServer

// startLocalAgent registers this node as a local-agent node and starts its
// agent. The controller keeps a node it already reaches over SSH that way, and
// the plugin then works the ordinary way. It returns nil only when ctx ends
// before the controller answered.
func startLocalAgent(ctx context.Context, haify *client.HaifyClient, nodeName, nodeIP, confDir string,
	interval time.Duration, log *zap.Logger) csiNodeServer {
	hostname, err := os.Hostname()
	if err != nil {
		log.Fatal("read this node's hostname", zap.Error(err))
	}
	log.Debug("startLocalAgent: registering", zap.String("node", nodeName), zap.String("node_ip", nodeIP),
		zap.String("hostname", hostname), zap.String("conf_dir", confDir))
	if fi, err := os.Stat(confDir); err != nil || !fi.IsDir() {
		log.Fatal("the host's DRBD configuration directory is not mounted", zap.String("dir", confDir), zap.Error(err))
	}
	// Which mode the plugin works in depends on the answer, so it waits for
	// one rather than guessing.
	for {
		start := time.Now()
		node, err := haify.RegisterLocalAgent(ctx, nodeName, nodeIP, hostname)
		log.Debug("startLocalAgent: registration returned", zap.Duration("took", time.Since(start)), zap.Error(err))
		if err == nil {
			if !node.GetLocalAgent() {
				log.Info("node is managed by the controller over SSH; not acting as its agent", zap.String("node", nodeName))
				return hcsi.NewNodeServer(haify, hcsi.NewMounter(), nodeName, log)
			}
			agent := hcsi.NewLocalAgent(haify, nodeName, confDir, log)
			go agent.Run(ctx, interval)
			log.Info("acting as this node's local agent", zap.String("node", nodeName), zap.String("hostname", hostname))
			return hcsi.NewLocalAgentNodeServer(haify, hcsi.NewMounter(), nodeName, agent, log)
		}
		log.Warn("register as local agent failed; retrying", zap.Error(err))
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(5 * time.Second):
		}
	}
}
