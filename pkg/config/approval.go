package config

import (
	"fmt"
	"strings"

	"github.com/spf13/viper"
	"go.uber.org/zap"
)

// ApprovalConfig is [rbac.approval]: two-person approval.
//
// RBAC decides what a role may do; it cannot stop one stolen admin token from
// doing all of it. With approval on, the calls in Methods — the ones that
// destroy data, or weaken what keeps it — run only after a different user with
// the approve right (admin, or the security-officer role) has approved that
// exact call. Creating users and changing roles are on the list, so the one
// token cannot mint itself a second approver.
type ApprovalConfig struct {
	Enabled bool `mapstructure:"enabled"`
	// Methods are API method names, e.g. "DeleteBackupTarget". Empty means
	// DefaultApprovalMethods.
	Methods []string `mapstructure:"methods"`
	// TTLMinutes is how long a request waits for approval, and how long an
	// approved call then has to be made.
	TTLMinutes int `mapstructure:"ttl_minutes"`
}

// DefaultApprovalMethods destroy data or weaken what protects it.
var DefaultApprovalMethods = []string{
	// Data.
	"DeletePool", "DeleteZFSPool", "DeleteResource", "RemoveVolume", "DeleteZFSDataset",
	"DeleteSnapshot", "DeleteLvmSnapshot", "DeleteZFSSnapshot",
	"RestoreSnapshot", "RestoreLvmSnapshot", "RestoreZFSSnapshot", "RestoreBackup",
	"RollbackResourceSnapshot", "DeleteResourceSnapshot",
	// DeleteApp stops a database and, with delete_data, deletes its resource
	// from inside the controller, where DeleteResource's own approval would
	// never be asked for.
	"DeleteApp",
	// MoveVolume deletes the volume's snapshots in the old pool.
	"MoveVolume",
	// What protects it.
	"DeleteSnapshotSchedule", "UnfreezeSnapshotSchedule", "AddBackupTarget", "DeleteBackupTarget", "DeleteBackup", "DeleteBackupSchedule",
	// Only when they replace a schedule with one that keeps less: its next
	// run would prune what the old one kept.
	"CreateSnapshotSchedule", "CreateBackupSchedule",
	"MarkNodeLost", "RestoreNode",
	// Who may do any of it.
	"CreateRbacUser", "DeleteRbacUser", "SetRbacUserRole",
}

func setApprovalDefaults() {
	zap.L().Debug("setApprovalDefaults: registering approval defaults")
	viper.SetDefault("rbac.approval.enabled", false)
	viper.SetDefault("rbac.approval.ttl_minutes", 60)
}

// ApprovalMethods is the effective list.
func (a ApprovalConfig) ApprovalMethods() []string {
	if len(a.Methods) == 0 {
		zap.L().Debug("ApprovalConfig.ApprovalMethods: none configured, using defaults", zap.Int("count", len(DefaultApprovalMethods)))
		return DefaultApprovalMethods
	}
	zap.L().Debug("ApprovalConfig.ApprovalMethods: using configured list", zap.Int("count", len(a.Methods)))
	return a.Methods
}

func (a *ApprovalConfig) validate(rbacEnabled bool) error {
	log := zap.L()
	log.Debug("ApprovalConfig.validate: start", zap.Bool("enabled", a.Enabled), zap.Bool("rbac_enabled", rbacEnabled),
		zap.Int("methods", len(a.Methods)), zap.Int("ttl_minutes", a.TTLMinutes))
	if !a.Enabled {
		log.Debug("ApprovalConfig.validate: disabled, nothing to check")
		return nil
	}
	if !rbacEnabled {
		log.Debug("ApprovalConfig.validate: approval without rbac")
		return fmt.Errorf("rbac.approval needs [rbac] enabled: approval is a second user, and without RBAC there are no users")
	}
	if a.TTLMinutes <= 0 {
		log.Debug("ApprovalConfig.validate: ttl unset, defaulting", zap.Int("ttl_minutes", 60))
		a.TTLMinutes = 60
	}
	for _, m := range a.Methods {
		if strings.TrimSpace(m) == "" || strings.ContainsAny(m, "/. ") {
			log.Debug("ApprovalConfig.validate: not a method name", zap.String("method", m))
			return fmt.Errorf("rbac.approval.methods: %q is not an API method name (e.g. DeleteBackupTarget)", m)
		}
	}
	return nil
}
