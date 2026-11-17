package config

import (
	"fmt"

	"github.com/spf13/viper"
	"go.uber.org/zap"
)

// SelfHealConfig is [self_heal]: replacing the replicas of a node that stays
// unreachable (pkg/controller/self_heal_evict.go).
//
// AutoEvict is "off" (the default), "dry-run" (decide and announce what would
// be done, do nothing) or "on". A node is evicted only after AfterMinutes
// offline, confirmed by its peers' DRBD as well as by SSH, and only while at
// most MaxOfflinePercent of the nodes are offline at once — more than that
// looks like a network partition, in which the controller may be the one cut
// off.
//
// AutoDiskful takes the same three values and is about something else: a
// node that keeps using a volume as a diskless client gets a replica of its
// own, and an old replica goes, so the data ends up where it is used
// (pkg/controller/self_heal_follow.go). It is off by default because each
// such move is a full sync of the volume, and it acts only after
// AutoDiskfulAfterMinutes of uninterrupted use.
type SelfHealConfig struct {
	AutoEvict               string `mapstructure:"auto_evict"`
	AfterMinutes            int    `mapstructure:"after_minutes"`
	MaxOfflinePercent       int    `mapstructure:"max_offline_percent"`
	AutoDiskful             string `mapstructure:"auto_diskful"`
	AutoDiskfulAfterMinutes int    `mapstructure:"auto_diskful_after_minutes"`
}

// minAutoDiskfulMinutes is the shortest use that may move data. A pod
// restart, a live migration and a node reboot for an upgrade are all over
// well inside it.
const minAutoDiskfulMinutes = 10

func setSelfHealDefaults() {
	zap.L().Debug("setSelfHealDefaults: registering self-heal defaults")
	viper.SetDefault("self_heal.auto_evict", "off")
	viper.SetDefault("self_heal.after_minutes", 60)
	viper.SetDefault("self_heal.max_offline_percent", 34)
	viper.SetDefault("self_heal.auto_diskful", "off")
	viper.SetDefault("self_heal.auto_diskful_after_minutes", 60)
}

// Enabled reports whether auto-evict runs at all, dry run included.
func (s SelfHealConfig) Enabled() bool { return s.AutoEvict == "on" || s.AutoEvict == "dry-run" }

// DryRun reports whether it only announces.
func (s SelfHealConfig) DryRun() bool { return s.AutoEvict == "dry-run" }

// DiskfulEnabled reports whether auto-diskful runs at all, dry run included.
func (s SelfHealConfig) DiskfulEnabled() bool {
	return s.AutoDiskful == "on" || s.AutoDiskful == "dry-run"
}

// DiskfulDryRun reports whether auto-diskful only announces.
func (s SelfHealConfig) DiskfulDryRun() bool { return s.AutoDiskful == "dry-run" }

func (s *SelfHealConfig) validate() error {
	log := zap.L()
	log.Debug("SelfHealConfig.validate: start", zap.String("auto_evict", s.AutoEvict),
		zap.Int("after_minutes", s.AfterMinutes), zap.Int("max_offline_percent", s.MaxOfflinePercent))
	switch s.AutoEvict {
	case "", "off", "dry-run", "on":
	default:
		log.Debug("SelfHealConfig.validate: unknown auto_evict", zap.String("auto_evict", s.AutoEvict))
		return fmt.Errorf("self_heal.auto_evict must be off, dry-run or on, not %q", s.AutoEvict)
	}
	if s.Enabled() && s.AfterMinutes < 5 {
		log.Debug("SelfHealConfig.validate: after_minutes too short", zap.Int("after_minutes", s.AfterMinutes))
		return fmt.Errorf("self_heal.after_minutes must be at least 5: a reboot must not cost a node its replicas")
	}
	if s.MaxOfflinePercent < 0 || s.MaxOfflinePercent > 50 {
		log.Debug("SelfHealConfig.validate: max_offline_percent out of range", zap.Int("max_offline_percent", s.MaxOfflinePercent))
		return fmt.Errorf("self_heal.max_offline_percent must be between 0 and 50")
	}
	switch s.AutoDiskful {
	case "", "off", "dry-run", "on":
	default:
		log.Debug("SelfHealConfig.validate: unknown auto_diskful", zap.String("auto_diskful", s.AutoDiskful))
		return fmt.Errorf("self_heal.auto_diskful must be off, dry-run or on, not %q", s.AutoDiskful)
	}
	if s.DiskfulEnabled() && s.AutoDiskfulAfterMinutes < minAutoDiskfulMinutes {
		log.Debug("SelfHealConfig.validate: auto_diskful_after_minutes too short",
			zap.Int("auto_diskful_after_minutes", s.AutoDiskfulAfterMinutes))
		return fmt.Errorf("self_heal.auto_diskful_after_minutes must be at least %d: "+
			"a pod restart or a live migration must not cost a full sync", minAutoDiskfulMinutes)
	}
	return nil
}
