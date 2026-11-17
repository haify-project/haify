package csi

import (
	"fmt"
	"sort"
	"strings"

	"github.com/container-storage-interface/spec/lib/go/csi"
	haifypb "github.com/haify-project/haify/api/proto/v1"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// Zone topology.
//
// A node plugin publishes its node's Kubernetes zone and region beside the
// per-node key, under the well-known keys, so a StorageClass can name zones in
// allowedTopologies. It also mirrors both onto the Haify node as labels
// (node_topology.go), which is how the controller plugin, which sees Haify
// nodes and not Kubernetes ones, knows which node is in which zone.
const (
	// TopologyKeyZone and TopologyKeyRegion are the Kubernetes node labels a
	// node plugin reads, the topology keys it publishes them under, and the
	// labels it mirrors them into on the Haify node.
	TopologyKeyZone   = "topology.kubernetes.io/zone"
	TopologyKeyRegion = "topology.kubernetes.io/region"

	// HaifyZoneLabel and HaifyRegionLabel are the short Haify node labels that
	// follow the mirrored ones unless an operator set them to something else,
	// so a StorageClass can say faultDomainLabel: zone.
	HaifyZoneLabel   = "zone"
	HaifyRegionLabel = "region"
)

// zoneKeys are the topology keys, other than the node's own, the driver
// understands.
var zoneKeys = []string{TopologyKeyZone, TopologyKeyRegion}

// zoneOnlySegments returns the zone and region of a topology that names no
// node, or nil when it names one or carries neither.
//
// A topology with the node key is about that node and is handled as such
// everywhere: the zone beside it only repeats where the node is. What is left
// is a topology that names a zone alone, which external-provisioner sends for
// a StorageClass whose allowedTopologies list zones.
func zoneOnlySegments(t *csi.Topology) map[string]string {
	segs := t.GetSegments()
	if segs[TopologyKeyNode] != "" {
		return nil
	}
	out := map[string]string{}
	for _, k := range zoneKeys {
		if v := segs[k]; v != "" {
			out[k] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// zoneTerms extracts the zone-only topologies of a requirement, preferred ones
// first, each once.
func zoneTerms(req *csi.TopologyRequirement) []map[string]string {
	if req == nil {
		return nil
	}
	seen := map[string]bool{}
	var out []map[string]string
	add := func(tops []*csi.Topology) {
		for _, t := range tops {
			segs := zoneOnlySegments(t)
			if segs == nil {
				continue
			}
			if key := zoneTermString(segs); !seen[key] {
				seen[key] = true
				out = append(out, segs)
			}
		}
	}
	add(req.GetPreferred())
	add(req.GetRequisite())
	zap.L().Debug("zoneTerms: extracted", zap.Int("terms", len(out)))
	return out
}

// zoneTermString renders a term for messages and for telling terms apart.
func zoneTermString(term map[string]string) string {
	var parts []string
	for _, k := range zoneKeys {
		if v := term[k]; v != "" {
			parts = append(parts, strings.TrimPrefix(k, "topology.kubernetes.io/")+"="+v)
		}
	}
	return strings.Join(parts, ",")
}

// inZone reports whether a Haify node carrying labels lies in the zone (and
// region) a term names. Only the mirrored labels count: they are what
// Kubernetes says, where the short zone label may be an operator's own naming.
func inZone(labels, term map[string]string) bool {
	if len(term) == 0 {
		return false
	}
	for k, v := range term {
		if labels[k] != v {
			return false
		}
	}
	return true
}

// placeInZones applies the zone-only topologies of a CreateVolume to
// placement and returns the candidates and pins selectReplicaNodes then works
// from.
//
// The terms are a preference with one hard edge, for the same reason a
// requisite node is seated rather than filtered on (see requisiteNodes): a
// volume has several replicas, and spreading them over fault domains is the
// point of having them, so "every replica inside these zones" is not what a
// topology requirement can mean here. What it does mean:
//
//   - one replica is seated in a zone the CO asked for, the first preferred
//     one that has a node with room, so the volume is usable there. When
//     mustSeat is set and no such node exists, placement fails, and the caller
//     reports ResourceExhausted like every other placement failure;
//   - the remaining replicas take nodes inside the named zones before any
//     outside them.
//
// With no terms it returns its inputs untouched.
func placeInZones(candidates []replicaCandidate, pinned []string, terms []map[string]string, nodes []*haifypb.NodeInfo,
	replicas int, needBytes uint64, mustSeat bool) ([]replicaCandidate, []string, error) {
	log := zap.L()
	if len(terms) == 0 {
		return candidates, pinned, nil
	}
	labels := make(map[string]map[string]string, len(nodes))
	for _, n := range nodes {
		labels[n.GetName()] = n.GetLabels()
	}
	termOf := func(node string) int {
		for i, term := range terms {
			if inZone(labels[node], term) {
				return i
			}
		}
		return -1
	}

	out := make([]replicaCandidate, len(candidates))
	hosts := map[string]bool{}
	for i, c := range candidates {
		c.outside = termOf(c.node) < 0
		out[i] = c
		hosts[c.node] = true
		log.Debug("placeInZones: candidate", zap.String("node", c.node), zap.Bool("outside", c.outside))
	}

	// Pins already seat a replica each; one of them inside a named zone is all
	// the requirement asks for.
	seated := 0
	for _, p := range pinned {
		if !hosts[p] {
			continue
		}
		seated++
		if termOf(p) >= 0 && seated <= replicas {
			log.Debug("placeInZones: a pinned node is already inside", zap.String("node", p))
			return out, pinned, nil
		}
	}

	names := make([]string, 0, len(terms))
	for _, term := range terms {
		names = append(names, zoneTermString(term))
	}
	if seated >= replicas {
		log.Debug("placeInZones: no replica left to seat in a zone", zap.Int("pinned", seated), zap.Int("replicas", replicas))
		if mustSeat {
			return nil, nil, fmt.Errorf("the volume must be usable in %s, but its %d replica(s) are all tied to nodes outside (%s)",
				strings.Join(names, " or "), replicas, strings.Join(pinned, ", "))
		}
		return out, pinned, nil
	}
	for i := range terms {
		best := -1
		for j, c := range out {
			if termOf(c.node) != i || !c.fits(needBytes) {
				continue
			}
			if best < 0 || c.freeBytes > out[best].freeBytes || (c.freeBytes == out[best].freeBytes && c.node < out[best].node) {
				best = j
			}
		}
		if best >= 0 {
			log.Debug("placeInZones: seating a replica in a requested zone", zap.String("node", out[best].node),
				zap.String("zone", names[i]), zap.Uint64("free_bytes", out[best].freeBytes))
			return out, append(append([]string(nil), pinned...), out[best].node), nil
		}
		log.Debug("placeInZones: no node with room in zone", zap.String("zone", names[i]))
	}
	if mustSeat {
		return nil, nil, fmt.Errorf("no node in %s hosts the pool with %dGiB free", strings.Join(names, " or "), needBytes/giB)
	}
	log.Debug("placeInZones: nothing inside the requested zones, placing by free space")
	return out, pinned, nil
}

// zoneCapacity answers a GetCapacity that names a zone and no node: the room
// the pool has on the nodes of that zone.
//
// The largest volume is bounded twice. One replica has to fit on a node of the
// zone for the volume to be usable there, and the volume as a whole needs
// `replicas` nodes with that much room, which may lie in other zones.
func zoneCapacity(nodes []*haifypb.NodeInfo, capacity map[string]uint64, unknown []string, term map[string]string,
	params VolumeParams) (*csi.GetCapacityResponse, error) {
	log := zap.L()
	noFigure := map[string]bool{}
	for _, u := range unknown {
		noFigure[u] = true
	}
	var total, largest uint64
	known, unmeasured := 0, 0
	for _, n := range nodes {
		if !inZone(n.GetLabels(), term) {
			continue
		}
		free, ok := capacity[n.GetName()]
		switch {
		case ok:
			known++
			total += free
			if free > largest {
				largest = free
			}
		case noFigure[n.GetName()]:
			unmeasured++
		}
		log.Debug("zoneCapacity: node in zone", zap.String("node", n.GetName()), zap.Bool("hosts_pool", ok || noFigure[n.GetName()]),
			zap.Uint64("free_bytes", free))
	}
	zone := zoneTermString(term)
	if known == 0 {
		switch {
		case unmeasured > 0:
			// As for one node: 0 would read as "full" over a figure nobody
			// measured.
			log.Debug("zoneCapacity: zone reports no capacity figure", zap.String("zone", zone))
			return nil, status.Errorf(codes.Unavailable, "pool %q in %s reports no capacity figure", params.Pool, zone)
		case params.AllowRemoteVolumeAccess:
			log.Debug("zoneCapacity: zone lacks pool, remote access allowed, reporting pool-wide", zap.String("zone", zone))
			return poolWide(capacity, params.Replicas), nil
		}
		log.Debug("zoneCapacity: no node of the zone hosts the pool, reporting 0", zap.String("zone", zone))
		return &csi.GetCapacityResponse{AvailableCapacity: 0, MaximumVolumeSize: wrapperspb.Int64(0)}, nil
	}
	maxVol := uint64(poolWide(capacity, params.Replicas).GetMaximumVolumeSize().GetValue())
	if largest < maxVol {
		maxVol = largest
	}
	log.Debug("zoneCapacity: computed", zap.String("zone", zone), zap.Int("nodes", known), zap.Uint64("total_bytes", total),
		zap.Uint64("max_volume_bytes", maxVol))
	return &csi.GetCapacityResponse{
		AvailableCapacity: int64(total),
		MaximumVolumeSize: wrapperspb.Int64(int64(maxVol)),
	}, nil
}

// topologyLabelConflict is a short label an operator set by hand to something
// other than what Kubernetes reports, which the sync left alone.
type topologyLabelConflict struct {
	label      string // "zone" or "region"
	kept       string // the operator's value, still on the node
	kubernetes string // what Kubernetes says
}

// topologyLabelChanges works out what to write on a Haify node so its labels
// follow the zone and region Kubernetes reports ("" for none), given the
// labels the node has now. An empty value in the result deletes that label,
// which is how the controller's label merge reads it.
//
// Two labels are kept per value, and the rule between them is what keeps an
// operator's own labels safe:
//
//   - the mirrored label (topology.kubernetes.io/zone) is the driver's. It
//     always ends up equal to Kubernetes's, and so also records what the
//     driver last wrote;
//   - the short label (zone) follows it only while it is absent or still
//     holds what the driver wrote, that is, equals the mirrored label. Any
//     other value was set by hand: it stays, and is reported as a conflict.
func topologyLabelChanges(current map[string]string, zone, region string) (map[string]string, []topologyLabelConflict) {
	set := map[string]string{}
	var conflicts []topologyLabelConflict
	for _, p := range []struct{ mirror, short, want string }{
		{TopologyKeyZone, HaifyZoneLabel, zone},
		{TopologyKeyRegion, HaifyRegionLabel, region},
	} {
		wrote, short := current[p.mirror], current[p.short]
		if wrote != p.want {
			set[p.mirror] = p.want
		}
		switch {
		case short == p.want:
		case short == "" || short == wrote:
			set[p.short] = p.want
		case p.want != "":
			conflicts = append(conflicts, topologyLabelConflict{label: p.short, kept: short, kubernetes: p.want})
		}
	}
	return set, conflicts
}

// sortedLabelKeys lists a label set's keys in order, for stable log lines.
func sortedLabelKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
