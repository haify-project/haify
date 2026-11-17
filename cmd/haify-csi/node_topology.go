package main

import (
	"context"
	"time"

	"github.com/haify-project/haify/pkg/client"
	"github.com/haify-project/haify/pkg/csi"
	"go.uber.org/zap"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// startZoneTopology makes the node plugin publish its node's Kubernetes zone
// and region and keep the Haify node's labels in step with them.
//
// Nothing here can stop the plugin from serving. Outside a cluster, or
// without permission to read the Node, it logs why and the plugin publishes
// the node alone, as it does on a cluster with no zones; the sync to Haify
// runs beside the driver and retries on its own.
func startZoneTopology(ctx context.Context, ns csiNodeServer, haify *client.HaifyClient, nodeName string,
	interval time.Duration, log *zap.Logger) {
	log.Debug("startZoneTopology: loading in-cluster config", zap.String("node", nodeName), zap.Duration("sync_interval", interval))
	cfg, err := rest.InClusterConfig()
	if err != nil {
		log.Warn("zone topology disabled: not running in a cluster", zap.Error(err))
		return
	}
	kube, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		log.Warn("zone topology disabled: kubernetes client", zap.Error(err))
		return
	}
	read := func(ctx context.Context) (map[string]string, error) {
		node, err := kube.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
		return node.Labels, nil
	}
	topology := csi.NewNodeTopology(nodeName, read, haify, log)
	csi.EnableNodeTopology(ns, topology)
	go topology.Run(ctx, interval)
	log.Info("zone topology enabled", zap.String("node", nodeName), zap.Duration("sync_interval", interval))
}
