package gateway

import (
	"context"
	"fmt"
	"strings"
	"time"

	"go.uber.org/zap"
)

// Keeping a gateway's promoter on the nodes that hold the data when those
// nodes change.
//
// A gateway's config is written to the resource's diskful replicas when the
// gateway is created or edited, and at no other time. Adding a replica
// therefore produced a node holding the data with no promoter — it could never
// take the gateway over, though nothing said so — and removing one left its
// promoter behind on a node that no longer held a copy. SyncPlacement is run
// after every such change.

// gatewayKinds are the plugin-id prefixes of the three gateway types, with what
// a node needs to run each one.
var gatewayKinds = []struct {
	prefix  string
	prereqs func() gatewayPrereqs
}{
	{"haify-nfs-", nfsPrereqs},
	{"haify-iscsi-", iscsiPrereqs},
	{"haify-nvmeof-", nvmePrereqs},
	{"haify-smb-", smbPrereqs},
}

// SyncPlacement makes resource's gateway promoter config present on every
// diskful replica that may run it, and absent everywhere else.
//
// Nodes that already hold the config are not written to. On the node serving
// the gateway, a rewritten config would be a changed plugin to drbd-reactor,
// and reloading it stops the whole gateway; an edited config waiting there as
// .toml.pending (live_edit.go) is likewise left where it is. A node that lacks
// the config gets the copy the others agree on, as .toml.disabled when the
// gateway is stopped, and only after its prerequisites are checked: a promoter
// whose chain cannot start there would look like a standby and never be one.
//
// A resource without a gateway is not an error; its promoter is still retired
// from nodes that are no longer replicas, in case one was left behind.
func (m *Manager) SyncPlacement(ctx context.Context, resource string) error {
	log := m.log()
	begin := time.Now()
	log.Debug("SyncPlacement: start", zap.String("resource", resource))
	if m.resources == nil {
		log.Debug("SyncPlacement: no resource manager, nothing to place", zap.String("resource", resource))
		return nil // a manager without resources manages no gateways
	}
	res, err := m.resources.GetResource(ctx, resource)
	if err != nil || res == nil {
		log.Debug("SyncPlacement: resource lookup failed", zap.String("resource", resource), zap.Error(err))
		// Unlike promoterHosts, never fall back to "every host": that would
		// install a gateway on nodes with no copy of its data.
		return fmt.Errorf("look up %s to place its gateway: %v", resource, err)
	}
	run := gatewayNodes(res)
	if len(run) == 0 {
		log.Debug("SyncPlacement: no diskful replicas", zap.String("resource", resource))
		return fmt.Errorf("resource %s has no diskful replicas to place its gateway on", resource)
	}
	_, rest := m.promoterHosts(ctx, resource)
	log.Debug("SyncPlacement: placement", zap.String("resource", resource), zap.Strings("run", run), zap.Strings("rest", rest))

	var patterns []string
	for _, k := range gatewayKinds {
		path := gatewayConfigPath(k.prefix + resource)
		patterns = append(patterns, path, path+pendingSuffix, path+disabledSuffix)
	}
	byHost, err := m.dumpNodeConfigs(ctx, run, patterns...)
	if err != nil {
		log.Debug("SyncPlacement: reading configs failed", zap.String("resource", resource), zap.Error(err))
		return err
	}
	for _, h := range run {
		if _, ok := byHost[h]; !ok {
			m.logger.Warn("Replica did not answer; its gateway promoter is left as it is",
				zap.String("resource", resource), zap.String("host", h))
		}
	}

	for _, k := range gatewayKinds {
		pluginID := k.prefix + resource
		if err := m.placeConfig(ctx, pluginID, byHost, k.prereqs()); err != nil {
			log.Debug("SyncPlacement: placing failed", zap.String("plugin", pluginID), zap.Error(err))
			return err
		}
	}
	m.retirePromoter(ctx, rest, resource)
	log.Debug("SyncPlacement: done", zap.String("resource", resource), zap.Duration("took", time.Since(begin)))
	return nil
}

// placeConfig installs pluginID's config on the answering hosts that lack it.
func (m *Manager) placeConfig(ctx context.Context, pluginID string, byHost map[string]map[string]string,
	prereqs gatewayPrereqs) error {

	log := m.log()
	path := gatewayConfigPath(pluginID)
	var copies []nodeConfigCopy
	var missing []string
	for _, host := range sortedKeys(byHost) {
		files := byHost[host]
		switch {
		case has(files, path+pendingSuffix):
			log.Debug("placeConfig: host holds a pending copy", zap.String("plugin", pluginID), zap.String("host", host))
			copies = append(copies, nodeConfigCopy{host: host, content: files[path+pendingSuffix]})
		case has(files, path):
			log.Debug("placeConfig: host holds a live copy", zap.String("plugin", pluginID), zap.String("host", host))
			copies = append(copies, nodeConfigCopy{host: host, content: files[path]})
		case has(files, path+disabledSuffix):
			log.Debug("placeConfig: host holds a disabled copy", zap.String("plugin", pluginID), zap.String("host", host))
			copies = append(copies, nodeConfigCopy{host: host, disabled: true, content: files[path+disabledSuffix]})
		default:
			log.Debug("placeConfig: host lacks the config", zap.String("plugin", pluginID), zap.String("host", host))
			missing = append(missing, host)
		}
	}
	if len(copies) == 0 || len(missing) == 0 {
		log.Debug("placeConfig: nothing to place",
			zap.String("plugin", pluginID), zap.Int("copies", len(copies)), zap.Strings("missing", missing))
		return nil
	}
	chosen, from, _ := chooseConfigCopy(copies)
	log.Debug("placeConfig: placing",
		zap.String("plugin", pluginID), zap.Strings("hosts", missing), zap.Strings("from", from), zap.Bool("disabled", chosen.disabled))

	if err := m.checkGatewayPrereqs(ctx, missing, prereqs); err != nil {
		log.Debug("placeConfig: prerequisites missing", zap.String("plugin", pluginID), zap.Strings("hosts", missing), zap.Error(err))
		return fmt.Errorf("gateway %s cannot run on %s: %w", pluginID, strings.Join(missing, ", "), err)
	}
	if strings.HasPrefix(pluginID, "haify-nfs-") {
		m.prepareNFSNode(ctx, missing, "")
	}
	if strings.HasPrefix(pluginID, "haify-smb-") {
		if err := m.prepareSMBNode(ctx, missing); err != nil {
			log.Debug("placeConfig: preparing SMB nodes failed", zap.Strings("hosts", missing), zap.Error(err))
			return err
		}
	}
	if strings.HasPrefix(pluginID, "haify-nvmeof-") {
		if err := m.loadNVMeModulesFor(ctx, missing, chosen.content); err != nil {
			log.Debug("placeConfig: loading NVMe modules failed", zap.Strings("hosts", missing), zap.Error(err))
			return err
		}
	}

	dest := path
	if chosen.disabled {
		dest = path + disabledSuffix
	}
	start := time.Now()
	if err := m.deployment.DistributeConfig(ctx, missing, chosen.content, dest); err != nil {
		log.Debug("placeConfig: distributing failed",
			zap.Strings("hosts", missing), zap.String("path", dest), zap.Duration("took", time.Since(start)), zap.Error(err))
		return fmt.Errorf("install gateway %s on %s: %w", pluginID, strings.Join(missing, ", "), err)
	}
	log.Debug("placeConfig: config distributed",
		zap.Strings("hosts", missing), zap.String("path", dest), zap.Duration("took", time.Since(start)))
	if !chosen.disabled {
		start = time.Now()
		err := m.deployment.Exec(ctx, missing, reloadReactorCmd)
		log.Debug("placeConfig: reloaded drbd-reactor",
			zap.Strings("hosts", missing), zap.Duration("took", time.Since(start)), zap.Error(err))
		if err != nil {
			m.logger.Warn("Failed to reload drbd-reactor after placing a gateway",
				zap.String("gateway", pluginID), zap.Strings("hosts", missing), zap.Error(err))
		}
	} else {
		log.Debug("placeConfig: gateway stopped, no reload", zap.String("plugin", pluginID))
	}
	m.logger.Info("Gateway promoter placed on new replicas",
		zap.String("gateway", pluginID), zap.Strings("hosts", missing),
		zap.Strings("copied_from", from), zap.Bool("stopped", chosen.disabled))
	return nil
}

// loadNVMeModulesFor loads the kernel modules an NVMe-oF gateway's config
// needs, reading its transport from the nvmet-port entry.
func (m *Manager) loadNVMeModulesFor(ctx context.Context, hosts []string, content string) error {
	log := m.log()
	units, err := parsePromoterUnits(content)
	if err != nil {
		log.Debug("loadNVMeModulesFor: config unparsable", zap.Strings("hosts", hosts))
		return err
	}
	transport := "tcp"
	for _, u := range units.Units {
		if u.Agent == "heartbeat:nvmet-port" && u.Params["type"] != "" {
			transport = u.Params["type"]
		}
	}
	log.Debug("loadNVMeModulesFor: transport read", zap.Strings("hosts", hosts), zap.String("transport", transport))
	return NewNVMeManager(m).ensureNVMeModules(ctx, hosts, transport)
}

// CheckPlacementPrereqs reports whether hosts could run the gateway of the
// given type ("nfs", "iscsi", "nvmeof"). It has no side effects, so it can
// refuse a new replica before anything is provisioned for it.
func (m *Manager) CheckPlacementPrereqs(ctx context.Context, gatewayType string, hosts []string) error {
	m.log().Debug("CheckPlacementPrereqs: start", zap.String("type", gatewayType), zap.Strings("hosts", hosts))
	for _, k := range gatewayKinds {
		if k.prefix == "haify-"+gatewayType+"-" {
			return m.checkGatewayPrereqs(ctx, hosts, k.prereqs())
		}
	}
	m.log().Debug("CheckPlacementPrereqs: unknown gateway type", zap.String("type", gatewayType))
	return fmt.Errorf("unknown gateway type %q", gatewayType)
}

func has(files map[string]string, path string) bool {
	_, ok := files[path]
	return ok
}
