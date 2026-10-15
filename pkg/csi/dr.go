package csi

import (
	"fmt"
	"net"
	"strconv"
	"strings"

	haifypb "github.com/haify-project/haify/api/proto/v1"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Cross-site disaster recovery for PersistentVolumes.
//
// A StorageClass with drNode creates each volume as a Haify WAN resource: the
// local replicas as usual, plus an asynchronous replica on drNode at another
// site, reached through a haify-proxy pair (CreateResourceRequest.wan). The DR
// replica never takes part in this cluster's scheduling: it is not in the
// volume's topology and never a local replica. Failing over to it is manual
// (`haify resource dr-failover`), because it can be behind.

// StorageClass parameters for the DR replica.
const (
	paramDRNode          = "drNode"          // the Haify node at the DR site
	paramDREndpoint      = "drEndpoint"      // the DR site's public address, without a port
	paramDRWANPort       = "drWanPort"       // the haify-proxy port; default a random one above 3000
	paramDREgressAddress = "drEgressAddress" // the primary site's source address for the WAN link
)

// drParams is the parsed DR part of a StorageClass.
type drParams struct {
	node     string // empty: no DR replica
	endpoint string
	wanPort  uint32
	egress   string
}

// parseDRParams validates the DR parameters. drNode and drEndpoint come
// together; the rest are optional.
func parseDRParams(p map[string]string) (drParams, error) {
	log := zap.L()
	out := drParams{
		node:     strings.TrimSpace(p[paramDRNode]),
		endpoint: strings.TrimSpace(p[paramDREndpoint]),
		egress:   strings.TrimSpace(p[paramDREgressAddress]),
	}
	log.Debug("parseDRParams: start", zap.String("dr_node", out.node), zap.String("dr_endpoint", out.endpoint),
		zap.String("egress", out.egress), zap.String("wan_port", p[paramDRWANPort]))
	if v := strings.TrimSpace(p[paramDRWANPort]); v != "" {
		n, err := strconv.ParseUint(v, 10, 16)
		if err != nil || n <= 3000 {
			log.Debug("parseDRParams: invalid WAN port", zap.String("wan_port", v))
			return out, fmt.Errorf("invalid %s %q (want a port above 3000)", paramDRWANPort, v)
		}
		out.wanPort = uint32(n)
	}
	switch {
	case out.node == "" && (out.endpoint != "" || out.egress != "" || out.wanPort != 0):
		log.Debug("parseDRParams: DR settings without a DR node")
		return out, fmt.Errorf("%s, %s and %s need %s", paramDREndpoint, paramDRWANPort, paramDREgressAddress, paramDRNode)
	case out.node != "" && out.endpoint == "":
		log.Debug("parseDRParams: DR node without endpoint", zap.String("dr_node", out.node))
		return out, fmt.Errorf("%s needs %s: the DR site's address the primary site connects to, without a port", paramDRNode, paramDREndpoint)
	case out.node != "" && net.ParseIP(out.endpoint) == nil && strings.ContainsAny(out.endpoint, ":/"):
		log.Debug("parseDRParams: endpoint has a port or scheme", zap.String("dr_endpoint", out.endpoint))
		return out, fmt.Errorf("%s %q: give an address or a host name, without a port or scheme (the port is %s)",
			paramDREndpoint, out.endpoint, paramDRWANPort)
	}
	return out, nil
}

// placeAwayFromDR checks that the DR node is a registered node holding the
// pool, and takes it out of the local candidates: a node cannot be both a
// synchronous local replica and the asynchronous remote one.
func placeAwayFromDR(dr drParams, nodes []*haifypb.NodeInfo, candidates []replicaCandidate, pinned []string, pool string) ([]replicaCandidate, error) {
	log := zap.L()
	log.Debug("placeAwayFromDR: start", zap.String("dr_node", dr.node), zap.Int("candidates", len(candidates)),
		zap.Strings("pinned", pinned))
	registered := false
	for _, n := range nodes {
		if n.GetName() == dr.node {
			registered = true
			break
		}
	}
	if !registered {
		log.Debug("placeAwayFromDR: DR node not registered", zap.String("dr_node", dr.node))
		return nil, status.Errorf(codes.InvalidArgument, "%s %q is not a registered Haify node", paramDRNode, dr.node)
	}
	for _, p := range pinned {
		if p == dr.node {
			log.Debug("placeAwayFromDR: DR node pinned as a local replica", zap.String("dr_node", dr.node))
			// ResourceExhausted makes the provisioner drop the node the
			// scheduler picked and choose again.
			return nil, status.Errorf(codes.ResourceExhausted,
				"node %q is the DR node of this StorageClass and cannot also hold a local replica", dr.node)
		}
	}
	out := make([]replicaCandidate, 0, len(candidates))
	hostsPool := false
	for _, c := range candidates {
		if c.node == dr.node {
			hostsPool = true
			continue
		}
		out = append(out, c)
	}
	if !hostsPool {
		log.Debug("placeAwayFromDR: DR node lacks the pool or is drained", zap.String("dr_node", dr.node), zap.String("pool", pool))
		return nil, status.Errorf(codes.InvalidArgument, "%s %q does not host pool %q (or is drained); the DR replica needs it too",
			paramDRNode, dr.node, pool)
	}
	log.Debug("placeAwayFromDR: DR node left out of local placement", zap.String("dr_node", dr.node), zap.Int("candidates", len(out)))
	return out, nil
}

// applyDR fills the WAN fields of a create request.
func applyDR(req *haifypb.CreateResourceRequest, dr drParams) {
	if dr.node == "" {
		return
	}
	req.Wan = true
	req.DrNode = dr.node
	req.DrEndpoint = dr.endpoint
	req.WanPort = dr.wanPort
	req.WanEgressAddress = dr.egress
}

// localReplicas is a volume's diskful nodes without its DR node, which serves
// the DR site and no Pod in this cluster.
func localReplicas(res *haifypb.ResourceInfo) []string {
	if !res.GetWanMode() || res.GetDrNode() == "" {
		return res.GetNodes()
	}
	out := make([]string, 0, len(res.GetNodes()))
	for _, n := range res.GetNodes() {
		if n != res.GetDrNode() {
			out = append(out, n)
		}
	}
	return out
}
