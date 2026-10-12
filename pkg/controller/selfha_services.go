package controller

import (
	"context"
	"slices"

	"go.uber.org/zap"
)

// reconcileSelfHaServices brings the stored Self-HA config's service list in
// line with [self_ha] extra_services. Those are read when Self-HA is enabled;
// a cluster that adds the AI Copilot or the remote MCP server afterwards puts
// them in the promoter by hand, and without this the stored config keeps
// naming the controller alone, so the HA page warns that saving would drop
// them, and a sync from the stored config would. Nothing configured, nothing
// changes.
func (rm *ResourceManager) reconcileSelfHaServices(ctx context.Context) {
	log := rm.resourcesLog()
	extras := rm.selfHaExtraServices()
	if len(extras) == 0 || rm.controller == nil || rm.controller.db == nil {
		log.Debug("reconcileSelfHaServices: nothing to reconcile", zap.Int("extra_services", len(extras)))
		return
	}
	cfg, err := rm.controller.db.GetHaConfig(ctx, SelfHaResource)
	if err != nil || cfg == nil {
		log.Debug("reconcileSelfHaServices: no Self-HA config", zap.Bool("found", cfg != nil), zap.Error(err))
		return
	}
	want := append([]string{selfHaControllerSvc}, extras...)
	if slices.Equal(cfg.Services, want) {
		log.Debug("reconcileSelfHaServices: services already match", zap.Strings("services", want))
		return
	}
	was := cfg.Services
	cfg.Services = want
	if err := rm.controller.db.SaveHaConfig(ctx, cfg); err != nil {
		rm.controller.logger.Warn("Could not update the Self-HA services from [self_ha] extra_services", zap.Error(err))
		return
	}
	rm.controller.logger.Info("Self-HA services updated from [self_ha] extra_services",
		zap.Strings("was", was), zap.Strings("now", want))
}
