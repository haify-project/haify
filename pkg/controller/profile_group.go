package controller

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/haify-project/haify/pkg/database"
	"go.uber.org/zap"
)

// A resource profile is a group, not only a creation template: a resource
// created from it — or attached to it later — stays its member, and what is
// set on the profile is carried to every member. Without that a profile named
// what a new resource would look like and nothing more; changing one DRBD
// option on thirty database volumes meant thirty set-options runs, and the
// ones someone missed drifted silently.
//
// Traces count DRBD options rather than listing them: shared-secret is one.

// ProfileMemberResult is what one profile operation did to one member.
type ProfileMemberResult struct {
	Resource string
	OK       bool
	Message  string
}

// profileMembers lists the resources that belong to a profile, by name.
func (rm *ResourceManager) profileMembers(ctx context.Context, profile string) ([]*database.Resource, error) {
	log := rm.controller.traceLogger()
	start := time.Now()
	all, err := rm.controller.db.ListResources(ctx)
	log.Debug("profileMembers: listed resources", zap.String("profile", profile), zap.Int("count", len(all)),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return nil, fmt.Errorf("list resources: %w", err)
	}
	var members []*database.Resource
	for _, r := range all {
		if r.Profile == profile {
			members = append(members, r)
		}
	}
	sort.Slice(members, func(i, j int) bool { return members[i].Name < members[j].Name })
	log.Debug("profileMembers: members", zap.String("profile", profile), zap.Int("count", len(members)))
	return members, nil
}

func (rm *ResourceManager) requireProfile(ctx context.Context, name string) (*database.ResourceProfile, error) {
	log := rm.controller.traceLogger()
	if rm.controller.db == nil {
		log.Debug("requireProfile: no database", zap.String("profile", name))
		return nil, fmt.Errorf("database not available")
	}
	start := time.Now()
	p, err := rm.controller.db.GetResourceProfile(ctx, name)
	log.Debug("requireProfile: read profile", zap.String("profile", name), zap.Bool("found", err == nil && p != nil),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil || p == nil {
		return nil, fmt.Errorf("resource profile %q not found", name)
	}
	return p, nil
}

// SetProfileOptions records DRBD options on a profile and applies them to
// every member. The profile is saved first, so a member that fails keeps its
// old value while every later AdjustProfile tries again.
func (rm *ResourceManager) SetProfileOptions(ctx context.Context, name string, options map[string]string) (*database.ResourceProfile, []ProfileMemberResult, error) {
	log := rm.controller.traceLogger()
	log.Debug("SetProfileOptions: setting", zap.String("profile", name), zap.Int("options", len(options)))
	if len(options) == 0 {
		log.Debug("SetProfileOptions: no options given")
		return nil, nil, fmt.Errorf("no options provided")
	}
	p, err := rm.requireProfile(ctx, name)
	if err != nil {
		return nil, nil, err
	}
	if p.DRBDOptions == nil {
		p.DRBDOptions = map[string]string{}
	}
	for k, v := range options {
		p.DRBDOptions[k] = v
	}
	start := time.Now()
	err = rm.controller.db.SaveResourceProfile(ctx, p)
	log.Debug("SetProfileOptions: saved profile", zap.String("profile", name), zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return nil, nil, fmt.Errorf("save profile: %w", err)
	}
	members, err := rm.profileMembers(ctx, name)
	if err != nil {
		log.Debug("SetProfileOptions: members unknown", zap.String("profile", name), zap.Error(err))
		return p, nil, err
	}
	results := make([]ProfileMemberResult, 0, len(members))
	for _, m := range members {
		res := ProfileMemberResult{Resource: m.Name, OK: true, Message: "options applied"}
		start := time.Now()
		err := rm.SetOptions(ctx, m.Name, options)
		log.Debug("SetProfileOptions: applied to member", zap.String("profile", name), zap.String("resource", m.Name),
			zap.Duration("took", time.Since(start)), zap.Error(err))
		if err != nil {
			res.OK, res.Message = false, err.Error()
		}
		results = append(results, res)
	}
	rm.controller.logger.Info("Profile options set",
		zap.String("profile", name), zap.Strings("options", optionKeys(options)), zap.Int("members", len(members)))
	return p, results, nil
}

// AssignProfile makes resource a member of profile, applying the profile's
// DRBD options to it, or — with an empty profile — takes it out of the one it
// is in. Replica count and placement are left to AdjustProfile, which says
// what it would change before it changes it.
func (rm *ResourceManager) AssignProfile(ctx context.Context, resource, profile string) error {
	log := rm.controller.traceLogger()
	log.Debug("AssignProfile: assigning", zap.String("resource", resource), zap.String("profile", profile))
	if rm.controller.db == nil {
		log.Debug("AssignProfile: no database")
		return fmt.Errorf("database not available")
	}
	start := time.Now()
	r, err := rm.controller.db.GetResource(ctx, resource)
	log.Debug("AssignProfile: read resource", zap.String("resource", resource), zap.Bool("found", err == nil && r != nil),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil || r == nil {
		return fmt.Errorf("resource %q not found", resource)
	}
	if profile != "" {
		p, err := rm.requireProfile(ctx, profile)
		if err != nil {
			return err
		}
		if len(p.DRBDOptions) > 0 {
			start := time.Now()
			err := rm.SetOptions(ctx, resource, p.DRBDOptions)
			log.Debug("AssignProfile: applied profile options", zap.String("resource", resource), zap.String("profile", profile),
				zap.Int("options", len(p.DRBDOptions)), zap.Duration("took", time.Since(start)), zap.Error(err))
			if err != nil {
				return fmt.Errorf("apply the options of profile %q: %w", profile, err)
			}
		} else {
			log.Debug("AssignProfile: profile has no DRBD options", zap.String("profile", profile))
		}
	} else {
		log.Debug("AssignProfile: leaving profile", zap.String("resource", resource), zap.String("previous", r.Profile))
	}
	r.Profile = profile
	start = time.Now()
	err = rm.controller.db.SaveResource(ctx, r)
	log.Debug("AssignProfile: saved resource", zap.String("resource", resource), zap.String("profile", profile),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	return err
}

// AdjustProfile brings every member into line with its profile: the profile's
// DRBD options are applied, and a member with fewer diskful replicas than the
// profile asks for gets new ones, placed by the profile's pool and label
// constraints. A member with more replicas is reported and left alone —
// removing a copy of someone's data is not something to do as a side effect.
// With dryRun nothing is changed and each result says what would be.
func (rm *ResourceManager) AdjustProfile(ctx context.Context, name string, dryRun bool) ([]ProfileMemberResult, error) {
	log := rm.controller.traceLogger()
	log.Debug("AdjustProfile: adjusting", zap.String("profile", name), zap.Bool("dry_run", dryRun))
	p, err := rm.requireProfile(ctx, name)
	if err != nil {
		return nil, err
	}
	members, err := rm.profileMembers(ctx, name)
	if err != nil {
		log.Debug("AdjustProfile: members unknown", zap.String("profile", name), zap.Error(err))
		return nil, err
	}
	results := make([]ProfileMemberResult, 0, len(members))
	for _, m := range members {
		res := rm.adjustMember(ctx, p, m, dryRun)
		log.Debug("AdjustProfile: member", zap.String("profile", name), zap.String("resource", m.Name),
			zap.Bool("ok", res.OK), zap.String("message", res.Message))
		results = append(results, res)
	}
	return results, nil
}

func (rm *ResourceManager) adjustMember(ctx context.Context, p *database.ResourceProfile, m *database.Resource, dryRun bool) ProfileMemberResult {
	log := rm.controller.traceLogger()
	log.Debug("adjustMember: adjusting", zap.String("profile", p.Name), zap.String("resource", m.Name),
		zap.Bool("dry_run", dryRun), zap.Int("replicas_wanted", p.Replicas))
	res := ProfileMemberResult{Resource: m.Name, OK: true}
	var did []string

	if len(p.DRBDOptions) > 0 {
		if dryRun {
			did = append(did, "would apply the profile's DRBD options")
		} else {
			start := time.Now()
			err := rm.SetOptions(ctx, m.Name, p.DRBDOptions)
			log.Debug("adjustMember: applied DRBD options", zap.String("resource", m.Name),
				zap.Int("options", len(p.DRBDOptions)), zap.Duration("took", time.Since(start)), zap.Error(err))
			if err != nil {
				res.OK, res.Message = false, "apply options: "+err.Error()
				return res
			}
			did = append(did, "DRBD options applied")
		}
	}

	nodes := splitCSV(m.Nodes)
	if m.WANMode && m.DRNode != "" {
		nodes = without(nodes, m.DRNode) // the DR copy is not a local replica
	}
	switch {
	case p.Replicas <= 0 || len(nodes) == p.Replicas:
		log.Debug("adjustMember: replica count matches", zap.String("resource", m.Name), zap.Int("replicas", len(nodes)))
		did = append(did, fmt.Sprintf("%d replica(s)", len(nodes)))
	case len(nodes) > p.Replicas:
		log.Debug("adjustMember: more replicas than the profile, leaving them", zap.String("resource", m.Name),
			zap.Int("replicas", len(nodes)), zap.Int("replicas_wanted", p.Replicas))
		did = append(did, fmt.Sprintf("%d replicas, more than the profile's %d; none removed", len(nodes), p.Replicas))
	case m.WANMode:
		log.Debug("adjustMember: WAN resource, not adding replicas", zap.String("resource", m.Name),
			zap.Int("replicas", len(nodes)), zap.Int("replicas_wanted", p.Replicas))
		did = append(did, fmt.Sprintf("%d of %d replicas; WAN resources are not adjusted — add replicas with resource add-replica", len(nodes), p.Replicas))
	default:
		missing := p.Replicas - len(nodes)
		pool, sizeGB := rm.memberPoolAndSize(ctx, m.Name, p.Pool)
		picked, err := rm.selectAdditionalReplicas(ctx, pool, sizeGB, missing, nodes, nil, p.OnDifferent, p.OnSame)
		log.Debug("adjustMember: selected nodes for missing replicas", zap.String("resource", m.Name),
			zap.Int("missing", missing), zap.String("pool", pool), zap.Uint64("size_gb", sizeGB),
			zap.Strings("picked", picked), zap.Error(err))
		if err != nil {
			res.OK, res.Message = false, strings.Join(append(did, "no room for more replicas: "+err.Error()), "; ")
			return res
		}
		tiebreakers := splitCSV(m.DisklessNodes)
		for _, n := range picked {
			isTiebreaker := containsString(tiebreakers, n)
			if dryRun {
				log.Debug("adjustMember: dry run, would add replica", zap.String("resource", m.Name), zap.String("node", n),
					zap.Bool("tiebreaker", isTiebreaker))
				if isTiebreaker {
					did = append(did, "would turn tiebreaker "+n+" into a replica")
				} else {
					did = append(did, "would add a replica on "+n)
				}
				continue
			}
			var err error
			start := time.Now()
			if isTiebreaker {
				err = rm.tiebreakerToReplica(ctx, m.Name, n)
			} else {
				err = rm.AddReplica(ctx, m.Name, n)
			}
			log.Debug("adjustMember: added replica", zap.String("resource", m.Name), zap.String("node", n),
				zap.Bool("tiebreaker", isTiebreaker), zap.Duration("took", time.Since(start)), zap.Error(err))
			if err != nil {
				res.OK = false
				did = append(did, fmt.Sprintf("add a replica on %s: %v", n, err))
				break
			}
			if isTiebreaker {
				did = append(did, "turned tiebreaker "+n+" into a replica")
			} else {
				did = append(did, "added a replica on "+n)
			}
		}
	}
	res.Message = strings.Join(did, "; ")
	return res
}

// tiebreakerToReplica gives a resource's diskless tiebreaker a replica of its
// own. On three nodes the only place for a third copy of a two-replica
// resource is its tiebreaker, and AddReplica refuses a tiebreaker: it would be
// a data copy and a quorum-only member at once. The tiebreaker is taken out
// first, then the node is added as a replica; with three copies the resource
// no longer needs one. If the replica cannot be added, the tiebreaker is put
// back, so a failed adjust does not leave two copies with no third vote.
func (rm *ResourceManager) tiebreakerToReplica(ctx context.Context, resource, node string) error {
	log := rm.controller.traceLogger()
	log.Debug("tiebreakerToReplica: removing tiebreaker", zap.String("resource", resource), zap.String("node", node))
	if err := rm.SetTiebreaker(ctx, resource, ""); err != nil {
		log.Debug("tiebreakerToReplica: removing tiebreaker failed", zap.String("resource", resource), zap.Error(err))
		return fmt.Errorf("take tiebreaker %s out: %w", node, err)
	}
	if err := rm.AddReplica(ctx, resource, node); err != nil {
		log.Debug("tiebreakerToReplica: adding replica failed, restoring tiebreaker", zap.String("resource", resource),
			zap.String("node", node), zap.Error(err))
		if rerr := rm.SetTiebreaker(ctx, resource, node); rerr != nil {
			log.Debug("tiebreakerToReplica: restoring tiebreaker failed", zap.String("resource", resource), zap.Error(rerr))
			return fmt.Errorf("%w; putting the tiebreaker back also failed: %v", err, rerr)
		}
		return err
	}
	log.Debug("tiebreakerToReplica: tiebreaker is now a replica", zap.String("resource", resource), zap.String("node", node))
	return nil
}

// memberPoolAndSize is the pool a member's new replicas go into — its own
// volumes', else the profile's — and the size one must hold.
func (rm *ResourceManager) memberPoolAndSize(ctx context.Context, resource, profilePool string) (string, uint64) {
	log := rm.controller.traceLogger()
	start := time.Now()
	vols, err := rm.controller.db.ListVolumes(ctx, resource)
	log.Debug("memberPoolAndSize: listed volumes", zap.String("resource", resource), zap.Int("count", len(vols)),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	var pool string
	var size uint64
	if err == nil {
		for _, v := range vols {
			if pool == "" {
				pool = v.Pool
			}
			size += uint64(v.SizeGB)
		}
	}
	if pool == "" {
		log.Debug("memberPoolAndSize: no volume pool, using the profile's", zap.String("pool", profilePool))
		pool = profilePool
	}
	log.Debug("memberPoolAndSize: result", zap.String("resource", resource), zap.String("pool", pool), zap.Uint64("size_gb", size))
	return pool, size
}

// ProfileMaxSize is the largest volume a new member of the profile could get
// now: the biggest size for which the profile's pool still has room on enough
// nodes to satisfy its replica count and label constraints. On a thin pool the
// figure is what fits without overcommitting; thin reports that.
func (rm *ResourceManager) ProfileMaxSize(ctx context.Context, name string) (sizeGB uint64, nodes []string, thin bool, err error) {
	log := rm.controller.traceLogger()
	log.Debug("ProfileMaxSize: computing", zap.String("profile", name))
	p, err := rm.requireProfile(ctx, name)
	if err != nil {
		return 0, nil, false, err
	}
	if p.Pool == "" {
		log.Debug("ProfileMaxSize: profile names no pool", zap.String("profile", name))
		return 0, nil, false, fmt.Errorf("profile %q names no pool", name)
	}
	replicas := p.Replicas
	if replicas <= 0 {
		log.Debug("ProfileMaxSize: replicas unset, assuming 2", zap.String("profile", name))
		replicas = 2
	}
	cands, err := rm.placementCandidates(ctx, normalizeManagedName(p.Pool), 0, nil)
	log.Debug("ProfileMaxSize: placement candidates", zap.String("pool", p.Pool), zap.Int("count", len(cands)), zap.Error(err))
	if err != nil {
		return 0, nil, false, err
	}
	size, picked := maxPlaceableSize(cands, replicas, placementConstraints{onDifferent: p.OnDifferent, onSame: p.OnSame})
	if picked == nil {
		log.Debug("ProfileMaxSize: cannot place under the constraints", zap.String("pool", p.Pool), zap.Int("replicas", replicas))
		return 0, nil, false, fmt.Errorf("pool %q cannot place %d replica(s) under the profile's constraints", p.Pool, replicas)
	}
	for _, c := range cands {
		for _, n := range picked {
			if c.node == n && c.thin {
				thin = true
			}
		}
	}
	log.Debug("ProfileMaxSize: result", zap.String("profile", name), zap.Uint64("size_gb", size),
		zap.Strings("nodes", picked), zap.Bool("thin", thin))
	return size, picked, thin, nil
}

// maxPlaceableSize tries every candidate's free space as the size, largest
// first, and returns the first that still leaves enough nodes to place on, in
// whole GiB rounded down: a size that does not fit is worse than no answer.
func maxPlaceableSize(cands []placementNode, replicas int, c placementConstraints) (uint64, []string) {
	log := zap.L()
	log.Debug("maxPlaceableSize: trying sizes", zap.Int("candidates", len(cands)), zap.Int("replicas", replicas))
	sizes := make([]uint64, 0, len(cands))
	for _, n := range cands {
		sizes = append(sizes, n.freeBytes)
	}
	sort.Slice(sizes, func(i, j int) bool { return sizes[i] > sizes[j] })
	for _, s := range sizes {
		var fit []placementNode
		for _, n := range cands {
			if n.freeBytes >= s {
				fit = append(fit, n)
			}
		}
		picked, err := selectConstrained(fit, replicas, c)
		log.Debug("maxPlaceableSize: size tried", zap.Uint64("size_bytes", s), zap.Int("fit", len(fit)),
			zap.Strings("picked", picked), zap.Error(err))
		if err == nil {
			return s / (1 << 30), picked
		}
	}
	log.Debug("maxPlaceableSize: no size places")
	return 0, nil
}

// optionKeys lists option names without their values, for logs: one DRBD
// option is shared-secret.
func optionKeys(options map[string]string) []string {
	keys := make([]string, 0, len(options))
	for k := range options {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
