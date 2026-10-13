package wanproxy

import (
	"context"
	"fmt"
	"strings"
	"time"

	"go.uber.org/zap"
)

// MultiSpec describes WAN replication from a multi-replica primary site to a
// single DR node — the "两地三中心" shape: synchronous replicas in the
// production site, one asynchronous copy far away.
//
// The primary site keeps its normal LAN mesh (real IPs, protocol C). Only the
// legs that cross the WAN go through haify-proxy, one per primary-site node, each
// with its own WAN port and its own loopback pair so the tunnels do not collide
// on either end.
type MultiSpec struct {
	Resource string

	// PrimaryNodeAddrs are the reachable addresses of every primary-site node.
	// Each gets a dialer; each gets a matching acceptor on the DR.
	PrimaryNodeAddrs []string

	// PrimaryNodeKeys are the stable identities of those nodes — their
	// registered names — in the same order as PrimaryNodeAddrs. Leg names are
	// derived from these.
	//
	// Naming a leg after a node's address bakes a mutable fact into a systemd
	// instance name, a config filename and a metrics path. When the node is
	// renumbered (a DHCP lease that moves, a VM that returns on a different
	// address) every one of those names still refers to the old address, so the
	// running tunnel is orphaned: it keeps replicating, but nothing that
	// recomputes the name can find it, and deprovisioning silently misses it.
	//
	// A node's registered name does not change when its address does, which is
	// the property the name needs. Empty entries fall back to the address, so a
	// caller that has no names still gets the historic behaviour.
	PrimaryNodeKeys []string

	// DRNodeAddr is the DR-site node (runs every acceptor).
	DRNodeAddr string

	// DRPublicEndpoint is the DR's public WAN address the dialers connect to.
	DRPublicEndpoint string

	// BaseWANPort is the first mTLS WAN port; leg i uses BaseWANPort+i.
	BaseWANPort int

	// BaseDRBDPort is the first loopback DRBD port. Leg i uses BaseDRBDPort+i
	// on BOTH ends: the primary's dialer listens on it (the primary's DRBD
	// connects there) and the DR's acceptor dials it (the DR's DRBD binds
	// there). The two are on different machines, so they do not collide.
	BaseDRBDPort int

	PrimaryEgressAddr string
	BinaryPath        string

	// BinaryFor resolves the binary per node address; see ProxySpec.BinaryFor.
	// A fleet whose nodes do not all share one architecture must set this.
	BinaryFor func(nodeAddr string) string

	// Tuning applies to every leg; see ProxySpec.Tuning.
	Tuning Tuning

	SkipReachabilityCheck bool
}

// Legs expands a MultiSpec into the per-leg ProxySpecs, in node order.
func (m MultiSpec) Legs() []ProxySpec {
	single := len(m.PrimaryNodeAddrs) == 1
	zap.L().Debug("MultiSpec.Legs: expanding", zap.String("resource", m.Resource), zap.Int("count", len(m.PrimaryNodeAddrs)),
		zap.Bool("single", single), zap.Int("base_wan_port", m.BaseWANPort), zap.Int("base_drbd_port", m.BaseDRBDPort))
	specs := make([]ProxySpec, 0, len(m.PrimaryNodeAddrs))
	for i, primary := range m.PrimaryNodeAddrs {
		specs = append(specs, ProxySpec{
			Resource:              LegID(m.Resource, m.legKey(i), single),
			PrimaryNodeAddr:       primary,
			DRNodeAddr:            m.DRNodeAddr,
			DRPublicEndpoint:      m.DRPublicEndpoint,
			WANPort:               m.BaseWANPort + i,
			DRBDPort:              m.BaseDRBDPort + i,
			PrimaryEgressAddr:     m.PrimaryEgressAddr,
			BinaryPath:            m.BinaryPath,
			BinaryFor:             m.BinaryFor,
			Tuning:                m.Tuning,
			SkipReachabilityCheck: m.SkipReachabilityCheck,
		})
	}
	return specs
}

// legKey returns the stable identity used to name leg i, falling back to the
// node's address when no name was supplied.
func (m MultiSpec) legKey(i int) string {
	if i < len(m.PrimaryNodeKeys) {
		if k := strings.TrimSpace(m.PrimaryNodeKeys[i]); k != "" {
			zap.L().Debug("MultiSpec.legKey: using node name", zap.Int("index", i), zap.String("node", k))
			return k
		}
	}
	zap.L().Debug("MultiSpec.legKey: no node name, falling back to the address", zap.Int("index", i),
		zap.String("addr", m.PrimaryNodeAddrs[i]))
	return m.PrimaryNodeAddrs[i]
}

// Validate checks the multi-replica spec before anything is provisioned.
func (m MultiSpec) Validate() error {
	log := zap.L()
	log.Debug("MultiSpec.Validate: start", zap.String("resource", m.Resource), zap.Strings("primaries", m.PrimaryNodeAddrs),
		zap.String("dr", m.DRNodeAddr), zap.Int("base_wan_port", m.BaseWANPort), zap.Int("base_drbd_port", m.BaseDRBDPort))
	if strings.TrimSpace(m.Resource) == "" {
		log.Debug("MultiSpec.Validate: no resource name")
		return fmt.Errorf("wanproxy: resource name is required")
	}
	if len(m.PrimaryNodeAddrs) == 0 {
		log.Debug("MultiSpec.Validate: no primary-site nodes", zap.String("resource", m.Resource))
		return fmt.Errorf("wanproxy: at least one primary-site node is required")
	}
	if strings.TrimSpace(m.DRNodeAddr) == "" {
		log.Debug("MultiSpec.Validate: no DR address", zap.String("resource", m.Resource))
		return fmt.Errorf("wanproxy: DR node address is required")
	}
	seen := make(map[string]bool, len(m.PrimaryNodeAddrs))
	for _, p := range m.PrimaryNodeAddrs {
		if strings.TrimSpace(p) == "" {
			log.Debug("MultiSpec.Validate: empty primary address", zap.String("resource", m.Resource))
			return fmt.Errorf("wanproxy: empty primary-site node address")
		}
		if p == m.DRNodeAddr {
			log.Debug("MultiSpec.Validate: primary is also the DR node", zap.String("node", p))
			return fmt.Errorf("wanproxy: node %q cannot be both a primary-site replica and the DR site", p)
		}
		if seen[p] {
			log.Debug("MultiSpec.Validate: duplicate primary", zap.String("node", p))
			return fmt.Errorf("wanproxy: duplicate primary-site node %q", p)
		}
		seen[p] = true
	}
	// Each leg consumes one WAN port and two loopback ports; make sure the
	// ranges fit and cannot overlap.
	last := m.BaseWANPort + len(m.PrimaryNodeAddrs) - 1
	if m.BaseWANPort <= 0 || last > 65535 {
		log.Debug("MultiSpec.Validate: WAN port range out of range", zap.Int("base_wan_port", m.BaseWANPort), zap.Int("last", last))
		return fmt.Errorf("wanproxy: WAN port range %d-%d out of range", m.BaseWANPort, last)
	}
	lastDRBD := m.BaseDRBDPort + len(m.PrimaryNodeAddrs) - 1
	if m.BaseDRBDPort <= 0 || lastDRBD > 65535 {
		log.Debug("MultiSpec.Validate: DRBD port range out of range", zap.Int("base_drbd_port", m.BaseDRBDPort), zap.Int("last", lastDRBD))
		return fmt.Errorf("wanproxy: DRBD loopback port range %d-%d out of range", m.BaseDRBDPort, lastDRBD)
	}
	return nil
}

// ProvisionMulti sets up every WAN leg of a multi-replica primary site.
//
// Shared material (PKI, binary, unit template) is pushed once per node; the
// per-leg configs and systemd instances are then created leg by leg. A failure
// part-way leaves the already-provisioned legs running — Deprovision/
// DeprovisionMulti is the way back.
func ProvisionMulti(ctx context.Context, deploy DeploymentClient, spec MultiSpec) error {
	log := zap.L()
	start := time.Now()
	log.Debug("ProvisionMulti: start", zap.String("resource", spec.Resource), zap.Strings("primaries", spec.PrimaryNodeAddrs),
		zap.String("dr", spec.DRNodeAddr), zap.String("dr_endpoint", spec.DRPublicEndpoint),
		zap.Bool("skip_reachability", spec.SkipReachabilityCheck))
	if deploy == nil {
		log.Debug("ProvisionMulti: no deployment client", zap.String("resource", spec.Resource))
		return fmt.Errorf("wanproxy: deployment client is nil")
	}
	if err := spec.Validate(); err != nil {
		log.Debug("ProvisionMulti: spec invalid", zap.String("resource", spec.Resource), zap.Error(err))
		return err
	}

	legs := spec.Legs()
	allNodes := append(append([]string{}, spec.PrimaryNodeAddrs...), spec.DRNodeAddr)

	// Shared, idempotent material first — pushing it per leg would repeat the
	// same bytes N times to the DR node.
	pki, err := EnsurePKI(PKIDir)
	if err != nil {
		log.Debug("ProvisionMulti: PKI unavailable", zap.String("pki_dir", PKIDir), zap.Error(err))
		return fmt.Errorf("wanproxy: ensure PKI: %w", err)
	}
	if err := distribute(ctx, deploy, allNodes, unitTemplate, UnitTemplatePath, "install unit template"); err != nil {
		return err
	}
	if err := distribute(ctx, deploy, allNodes, string(pki.CAPEM), NodeCAPath, "distribute CA"); err != nil {
		return err
	}
	if err := distribute(ctx, deploy, allNodes, string(pki.CertPEM), NodeCertPath, "distribute cert"); err != nil {
		return err
	}
	if err := distribute(ctx, deploy, allNodes, string(pki.KeyPEM), NodeKeyPath, "distribute key"); err != nil {
		return err
	}
	// Per node, like Provision: BinaryFor used to be ignored here, so a
	// controller holding the right binary for each architecture pushed nothing.
	if err := ensureBinaries(ctx, deploy, allNodes,
		ProxySpec{BinaryPath: spec.BinaryPath, BinaryFor: spec.BinaryFor}); err != nil {
		return err
	}
	if err := requireBinaries(ctx, deploy, allNodes); err != nil {
		return err
	}
	if err := run(ctx, deploy, allNodes, "sudo systemctl daemon-reload", "systemd daemon-reload"); err != nil {
		return err
	}

	for _, leg := range legs {
		log.Debug("ProvisionMulti: provisioning leg", zap.String("leg", leg.Resource), zap.String("primary", leg.PrimaryNodeAddr),
			zap.Int("wan_port", leg.WANPort), zap.Int("drbd_port", leg.DRBDPort))
		// Both ends of a leg use the same leg port on their own loopback: the
		// dialer listens on it (the primary's DRBD connects there) and the
		// acceptor dials it (the DR's DRBD binds there). No offset — the two
		// numbers live on different machines.
		acceptor := leg

		if err := distribute(ctx, deploy, []string{leg.PrimaryNodeAddr},
			RenderDialerConfig(leg), NodeConfigPath(leg.Resource), "distribute dialer config"); err != nil {
			return err
		}
		if err := distribute(ctx, deploy, []string{spec.DRNodeAddr},
			RenderAcceptorConfig(acceptor), NodeConfigPath(leg.Resource), "distribute acceptor config"); err != nil {
			return err
		}
		both := []string{leg.PrimaryNodeAddr, spec.DRNodeAddr}
		// Restart, not `enable --now`: a running leg ignores the config and PKI
		// just written for it. See the same step in Provision for what that
		// looks like when it goes wrong.
		if err := run(ctx, deploy, both,
			fmt.Sprintf("sudo systemctl enable %s", UnitInstance(leg.Resource)), "enable proxy"); err != nil {
			return err
		}
		if err := run(ctx, deploy, both,
			fmt.Sprintf("sudo systemctl restart %s", UnitInstance(leg.Resource)), "restart proxy"); err != nil {
			return err
		}
		if !spec.SkipReachabilityCheck {
			if err := VerifyReachability(ctx, deploy, leg); err != nil {
				log.Debug("ProvisionMulti: leg unreachable", zap.String("leg", leg.Resource), zap.Error(err))
				return err
			}
		} else {
			log.Debug("ProvisionMulti: reachability check skipped", zap.String("leg", leg.Resource))
		}
	}
	log.Debug("ProvisionMulti: done", zap.String("resource", spec.Resource), zap.Int("legs", len(legs)),
		zap.Duration("took", time.Since(start)))
	return nil
}

// DeprovisionMulti tears down every WAN leg of a multi-replica primary site.
// It is best-effort per leg: one unreachable node must not strand the rest.
func DeprovisionMulti(ctx context.Context, deploy DeploymentClient, spec MultiSpec) error {
	log := zap.L()
	log.Debug("DeprovisionMulti: start", zap.String("resource", spec.Resource), zap.Strings("primaries", spec.PrimaryNodeAddrs),
		zap.String("dr", spec.DRNodeAddr))
	if deploy == nil {
		log.Debug("DeprovisionMulti: no deployment client", zap.String("resource", spec.Resource))
		return fmt.Errorf("wanproxy: deployment client is nil")
	}
	var firstErr error
	for _, leg := range spec.Legs() {
		err := Deprovision(ctx, deploy, leg.Resource, leg.PrimaryNodeAddr, leg.DRNodeAddr)
		log.Debug("DeprovisionMulti: leg deprovisioned", zap.String("leg", leg.Resource), zap.String("primary", leg.PrimaryNodeAddr),
			zap.Error(err))
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	log.Debug("DeprovisionMulti: done", zap.String("resource", spec.Resource), zap.Error(firstErr))
	return firstErr
}

// ValidateResourceNameForWAN rejects a WAN resource name that could be mistaken
// for another resource's leg. Leg instances are named "<resource>_<key>", so a
// resource literally named like one of those would make the two indistinguish-
// able — and FindStaleLegs would consider one resource's legs stale for the
// other. Refusing the name at creation is cheaper than disambiguating forever.
func ValidateResourceNameForWAN(name string, existing []string) error {
	zap.L().Debug("ValidateResourceNameForWAN: start", zap.String("resource", name), zap.Int("existing", len(existing)))
	for _, other := range existing {
		if other == name {
			continue
		}
		if strings.HasPrefix(name, other+"_") {
			zap.L().Debug("ValidateResourceNameForWAN: name collides with another resource's legs",
				zap.String("resource", name), zap.String("other", other))
			return fmt.Errorf(
				"wanproxy: resource name %q collides with WAN leg names of resource %q; choose a name that is not %q_<suffix>",
				name, other, other)
		}
	}
	return nil
}
