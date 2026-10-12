package controller

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/haify-project/haify/pkg/config"
	"github.com/haify-project/haify/pkg/metrics"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestNodeExporterDiscovery(t *testing.T) {
	ctrl := newBasicTestController(&fakeDeploymentClient{})
	ctrl.ctx = context.Background()
	ctrl.config = &config.Config{Metrics: config.MetricsConfig{NodeExporterPort: 39117}}
	m, err := metrics.New(zap.NewNop())
	require.NoError(t, err)
	ctrl.metrics = m
	ctrl.nodes.nodes["10.0.0.2"] = &NodeInfo{Name: "node-b", Address: "10.0.0.2", Labels: map[string]string{"rack.id": "r2"}}
	ctrl.nodes.nodes["10.0.0.1"] = &NodeInfo{Name: "node-a", Address: "10.0.0.1"}
	ctrl.nodes.nodes["fd00::3"] = &NodeInfo{Name: "node-c", Address: "fd00::3"}

	rec := httptest.NewRecorder()
	ctrl.metricsMux().ServeHTTP(rec, httptest.NewRequest("GET", NodeExporterDiscoveryPath+"?cluster=lab", nil))
	assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))
	var groups []sdGroup
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &groups))
	require.Len(t, groups, 3)
	assert.Equal(t, []string{"10.0.0.1:39117"}, groups[0].Targets)
	assert.Equal(t, "node-a", groups[0].Labels["node"])
	assert.Equal(t, "lab", groups[0].Labels["cluster"])
	assert.Equal(t, "r2", groups[1].Labels["__meta_haify_node_label_rack_id"])
	assert.Equal(t, []string{"[fd00::3]:39117"}, groups[2].Targets, "IPv6 addresses are bracketed")

	rec = httptest.NewRecorder()
	ctrl.metricsMux().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	assert.Equal(t, 200, rec.Code)
	assert.Contains(t, rec.Body.String(), "haify_controller_up")
}
