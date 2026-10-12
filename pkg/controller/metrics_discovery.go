package controller

import (
	"encoding/json"
	"net"
	"net/http"
	"sort"
	"strconv"

	"go.uber.org/zap"
)

// NodeExporterDiscoveryPath is where the metrics listener serves Prometheus
// HTTP service discovery for node_exporter: one target per registered node, so
// a node added or removed in Haify is scraped or dropped without editing
// prometheus.yml.
const NodeExporterDiscoveryPath = "/discovery/node-exporter"

// sdGroup is one entry of Prometheus' http_sd_configs format.
type sdGroup struct {
	Targets []string          `json:"targets"`
	Labels  map[string]string `json:"labels"`
}

// nodeExporterTargets lists every registered node's node_exporter, sorted by
// node name, labelled with the node's Haify name and its labels, and with
// cluster when one is given (?cluster= on the discovery URL, so one
// Prometheus job can cover several clusters).
func (c *Controller) nodeExporterTargets(port int, cluster string) []sdGroup {
	log := c.traceLogger()
	nodes, err := c.nodes.ListNodes(c.ctx)
	if err != nil {
		log.Debug("nodeExporterTargets: listing nodes failed", zap.Error(err))
		return []sdGroup{}
	}
	groups := make([]sdGroup, 0, len(nodes))
	for _, n := range nodes {
		labels := map[string]string{"node": n.Name}
		if cluster != "" {
			labels["cluster"] = cluster
		}
		for k, v := range n.Labels {
			labels["__meta_haify_node_label_"+sanitizeLabel(k)] = v
		}
		target := net.JoinHostPort(n.Address, strconv.Itoa(port))
		log.Debug("nodeExporterTargets: node", zap.String("node", n.Name), zap.String("target", target))
		groups = append(groups, sdGroup{Targets: []string{target}, Labels: labels})
	}
	sort.Slice(groups, func(i, j int) bool { return groups[i].Labels["node"] < groups[j].Labels["node"] })
	log.Debug("nodeExporterTargets: done", zap.Int("count", len(groups)), zap.Int("port", port))
	return groups
}

// sanitizeLabel makes a Haify label key a valid Prometheus label name part.
func sanitizeLabel(key string) string {
	out := []byte(key)
	for i, ch := range out {
		if !(ch == '_' || ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9') {
			out[i] = '_'
		}
	}
	return string(out)
}

// metricsMux serves /metrics (and, as before, any other path) plus the
// node_exporter discovery document.
func (c *Controller) metricsMux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(NodeExporterDiscoveryPath, func(w http.ResponseWriter, r *http.Request) {
		cluster := r.URL.Query().Get("cluster")
		c.traceLogger().Debug("metricsMux: node_exporter discovery requested", zap.String("remote", r.RemoteAddr), zap.String("cluster", cluster))
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(c.nodeExporterTargets(c.config.Metrics.NodeExporterPort, cluster)); err != nil {
			c.traceLogger().Debug("metricsMux: writing discovery failed", zap.Error(err))
		}
	})
	mux.Handle("/", c.metrics.Handler())
	return mux
}
