package controller

import (
	"context"
	"strings"

	"go.uber.org/zap"

	"github.com/haify-project/haify/pkg/alert"
)

// haServiceLister gives the alert monitor the services of every HA chain.
//
// It reads what `haify ha status` reads, once per poll for all of them:
// GetHaStatus probes each node a chain may run on a single time and answers
// for every resource it carries, so the cost is one SSH session per node
// however many chains there are.
type haServiceLister struct {
	rm *ResourceManager
}

// GetServiceStatusList implements alert.ServiceLister.
func (l haServiceLister) GetServiceStatusList(ctx context.Context) ([]alert.ServiceStatusInfo, error) {
	log := zap.L()
	if l.rm == nil {
		log.Debug("haServiceLister.GetServiceStatusList: no resource manager")
		return nil, nil
	}
	promoters, err := l.rm.GetHaStatus(ctx, "")
	log.Debug("haServiceLister.GetServiceStatusList: read HA status",
		zap.Int("promoters", len(promoters)), zap.Error(err))
	if err != nil {
		return nil, err
	}
	infos := serviceInfosFromHaStatus(promoters)
	log.Debug("haServiceLister.GetServiceStatusList: services listed", zap.Int("services", len(infos)))
	return infos, nil
}

// serviceInfosFromHaStatus lists the services of the given chains: the
// systemd units that do the chain's work, not its mounts, floating IPs or
// storage-protocol agents. A gateway's targets and exports are reported by the
// gateway events, and the mount and the IP by the resource checks; a service of
// `haify ha create` is what nothing else reads.
//
// A chain that runs on no node still lists its services, with no Node, so the
// monitor can tell a chain that is down from a unit that was removed.
func serviceInfosFromHaStatus(promoters []*HaPromoterStatus) []alert.ServiceStatusInfo {
	var infos []alert.ServiceStatusInfo
	for _, p := range promoters {
		if p == nil || p.DRBDResource == "" {
			continue
		}
		for _, unit := range p.Deps {
			if unit.Kind != haKindService {
				continue
			}
			infos = append(infos, alert.ServiceStatusInfo{
				Resource: p.DRBDResource,
				Unit:     unit.Name,
				Label:    unit.Label,
				Node:     p.ActiveNode,
				Active:   serviceRunning(unit),
				SubState: unit.SubState,
				Restarts: unit.Restarts,
			})
		}
	}
	return infos
}

// serviceRunning is whether a unit is doing its work. The reactor calling it
// active is not enough: a unit crash-looping under Restart=on-failure is
// "active" to the reactor for as long as systemd has not given up on it, so
// systemd's own finer state decides. "exited" is a unit that finished by
// design; no finer state at all means the probe had nothing more to say.
func serviceRunning(unit HaServiceStatus) bool {
	if !strings.EqualFold(unit.Status, "active") {
		return false
	}
	switch unit.SubState {
	case "", "running", "exited":
		return true
	}
	return false
}
