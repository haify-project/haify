package config

import (
	"fmt"
	"strings"

	"github.com/robfig/cron/v3"
	"github.com/spf13/viper"
	"go.uber.org/zap"
)

// ThinConfig is [storage.thin]: keeping thin pools from filling up.
//
// A thin pool fills in two ways the filesystems above it never see: a resync
// writes zeros into blocks nothing uses, and a filesystem frees blocks the
// pool still holds. Trimming returns the second kind; the resync defaults keep
// the first from happening. What is left — real growth — is met by leaving
// part of the volume group outside the pool and growing the pool into it
// before it is full, rather than after.
type ThinConfig struct {
	// TrimSchedule is a cron spec on which every mounted DRBD filesystem is
	// trimmed on the node serving it; the discards reach every replica's thin
	// pool. Empty turns it off.
	TrimSchedule string `mapstructure:"trim_schedule"`
	// ReservePercent of a volume group is left outside a new thin pool, as
	// room for AutoextendThreshold to grow into.
	ReservePercent int `mapstructure:"reserve_percent"`
	// AutoextendThreshold: once a thin pool's data or metadata use reaches
	// this percentage, the controller grows it by AutoextendPercent of its
	// size from the free space of its volume group. 0 turns it off.
	AutoextendThreshold int `mapstructure:"autoextend_threshold"`
	AutoextendPercent   int `mapstructure:"autoextend_percent"`
}

func setThinDefaults() {
	zap.L().Debug("setThinDefaults: registering thin pool defaults")
	viper.SetDefault("storage.thin.trim_schedule", "30 2 * * *")
	viper.SetDefault("storage.thin.reserve_percent", 10)
	viper.SetDefault("storage.thin.autoextend_threshold", 80)
	viper.SetDefault("storage.thin.autoextend_percent", 20)
}

func (t *ThinConfig) validate() error {
	log := zap.L()
	log.Debug("ThinConfig.validate: start", zap.String("trim_schedule", t.TrimSchedule),
		zap.Int("reserve_percent", t.ReservePercent), zap.Int("autoextend_threshold", t.AutoextendThreshold),
		zap.Int("autoextend_percent", t.AutoextendPercent))
	if spec := strings.TrimSpace(t.TrimSchedule); spec != "" {
		if _, err := cron.ParseStandard(spec); err != nil {
			log.Debug("ThinConfig.validate: trim schedule not a cron spec", zap.String("trim_schedule", spec), zap.Error(err))
			return fmt.Errorf("storage.thin.trim_schedule %q is not a cron spec: %w", spec, err)
		}
	}
	if t.ReservePercent < 0 || t.ReservePercent > 50 {
		log.Debug("ThinConfig.validate: reserve percent out of range", zap.Int("reserve_percent", t.ReservePercent))
		return fmt.Errorf("storage.thin.reserve_percent %d must be between 0 and 50", t.ReservePercent)
	}
	if t.AutoextendThreshold != 0 && (t.AutoextendThreshold < 50 || t.AutoextendThreshold > 95) {
		log.Debug("ThinConfig.validate: autoextend threshold out of range", zap.Int("autoextend_threshold", t.AutoextendThreshold))
		return fmt.Errorf("storage.thin.autoextend_threshold %d must be 0 (off) or between 50 and 95", t.AutoextendThreshold)
	}
	if t.AutoextendThreshold != 0 && (t.AutoextendPercent < 5 || t.AutoextendPercent > 100) {
		log.Debug("ThinConfig.validate: autoextend percent out of range", zap.Int("autoextend_percent", t.AutoextendPercent))
		return fmt.Errorf("storage.thin.autoextend_percent %d must be between 5 and 100", t.AutoextendPercent)
	}
	log.Debug("ThinConfig.validate: valid", zap.Bool("autoextend", t.AutoextendThreshold != 0))
	return nil
}
