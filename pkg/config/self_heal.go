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
type SelfHealConfig struct {
	AutoEvict         string `mapstructure:"auto_evict"`
	AfterMinutes      int    `mapstructure:"after_minutes"`
	MaxOfflinePercent int    `mapstructure:"max_offline_percent"`
}

func setSelfHealDefaults() {
	zap.L().Debug("setSelfHealDefaults: registering self-heal defaults")
	viper.SetDefault("self_heal.auto_evict", "off")
	viper.SetDefault("self_heal.after_minutes", 60)
	viper.SetDefault("self_heal.max_offline_percent", 34)
}

// Enabled reports whether auto-evict runs at all, dry run included.
func (s SelfHealConfig) Enabled() bool { return s.AutoEvict == "on" || s.AutoEvict == "dry-run" }

// DryRun reports whether it only announces.
func (s SelfHealConfig) DryRun() bool { return s.AutoEvict == "dry-run" }

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
	return nil
}
