package config

import "go.uber.org/zap"

// Sections whose types, defaults and validation live in files of their own
// (audit.go, approval.go, write_anomaly.go, self_heal.go, quota.go, thin.go), wired
// into Load and Validate here so config.go does not grow with each one.

func setSectionDefaults() {
	zap.L().Debug("setSectionDefaults: registering section defaults")
	setAuditDefaults()
	setApprovalDefaults()
	setWriteAnomalyDefaults()
	setSelfHealDefaults()
	setQuotaDefaults()
	setThinDefaults()
}

func (c *Config) validateSections() error {
	for i, validate := range []func() error{
		c.Audit.validate,
		func() error { return c.RBAC.Approval.validate(c.RBAC.Enabled) },
		c.Alert.WriteAnomaly.validate,
		c.SelfHeal.validate,
		c.Quota.validate,
		c.Storage.Thin.validate,
	} {
		if err := validate(); err != nil {
			zap.L().Debug("Config.validateSections: section invalid", zap.Int("index", i), zap.Error(err))
			return err
		}
	}
	zap.L().Debug("Config.validateSections: all sections valid")
	return nil
}
