package config

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/go-viper/mapstructure/v2"
	"github.com/robfig/cron/v3"
	"github.com/spf13/viper"
	"go.uber.org/zap"
)

// Load loads configuration from file
func Load(configPath string) (*Config, error) {
	log := zap.L()
	log.Debug("Load: start", zap.String("config_path", configPath))
	// Set defaults
	setDefaults()

	// Read config file
	if configPath != "" {
		log.Debug("Load: using the given config file", zap.String("config_path", configPath))
		viper.SetConfigFile(configPath)
	} else {
		log.Debug("Load: no config file given, searching for controller.toml",
			zap.Strings("search_paths", []string{"/etc/haify/", "./configs/", "."}))
		viper.SetConfigName("controller")
		viper.AddConfigPath("/etc/haify/")
		viper.AddConfigPath("./configs/")
		viper.AddConfigPath(".")
	}

	// Enable environment variable override
	viper.SetEnvPrefix("HAIFY")
	viper.AutomaticEnv()

	start := time.Now()
	if err := viper.ReadInConfig(); err != nil {
		log.Debug("Load: reading config failed", zap.String("config_path", configPath),
			zap.Error(err), zap.Duration("took", time.Since(start)))
		return nil, fmt.Errorf("failed to read config: %w", err)
	}
	log.Debug("Load: config file read", zap.String("file", viper.ConfigFileUsed()),
		zap.Duration("took", time.Since(start)))

	var config Config
	if err := viper.Unmarshal(&config); err != nil {
		log.Debug("Load: unmarshalling config failed", zap.String("file", viper.ConfigFileUsed()), zap.Error(err))
		return nil, fmt.Errorf("failed to unmarshal config: %w", err)
	}

	// Validate config
	if err := config.Validate(); err != nil {
		log.Debug("Load: config invalid", zap.String("file", viper.ConfigFileUsed()), zap.Error(err))
		return nil, fmt.Errorf("invalid config: %w", err)
	}

	config.traceLoaded(log)
	return &config, nil
}

// FileUsed is the config file the last Load read, empty before one has.
func FileUsed() string { return viper.ConfigFileUsed() }

// traceLoaded logs the loaded file and the settings that decide what the
// controller does, at Debug. Secrets (the API token, RBAC user tokens,
// webhook URLs and headers, the audit webhook token) are reported only as
// present or not.
func (c *Config) traceLoaded(log *zap.Logger) {
	ce := log.Check(zap.DebugLevel, "Config.traceLoaded: config loaded")
	if ce == nil {
		return
	}
	ce.Write(
		zap.String("file", viper.ConfigFileUsed()),
		zap.String("listen_address", c.Server.ListenAddress),
		zap.Int("port", c.Server.Port),
		zap.Int("rest_port", c.Server.RESTPort),
		zap.String("database_path", c.Database.Path),
		zap.String("dispatch_config_path", c.Dispatch.ConfigPath),
		zap.Int("dispatch_parallel", c.Dispatch.Parallel),
		zap.String("log_level", c.Log.Level),
		zap.String("log_format", c.Log.Format),
		zap.Bool("auth_enabled", c.Auth.Enabled),
		zap.Bool("has_auth_token", c.Auth.Token != ""),
		zap.Bool("tls_enabled", c.TLS.Enabled),
		zap.Bool("tls_mutual", c.TLS.MutualTLS()),
		zap.Bool("tls_rest", c.TLS.REST),
		zap.Bool("rbac_enabled", c.RBAC.Enabled),
		zap.Int("rbac_users", len(c.RBAC.Users)),
		zap.Int("rbac_policies", len(c.RBAC.Policies)),
		zap.Bool("approval_enabled", c.RBAC.Approval.Enabled),
		zap.String("default_pool_type", c.Storage.DefaultPoolType),
		zap.String("verify_schedule", c.Storage.VerifySchedule),
		zap.String("trim_schedule", c.Storage.Thin.TrimSchedule),
		zap.Bool("metrics_enabled", c.Metrics.Enabled),
		zap.Int("metrics_port", c.Metrics.Port),
		zap.Bool("ui_enabled", c.UI.UIEnabled()),
		zap.Int("ui_port", c.UI.Port),
		zap.Bool("audit_enabled", c.Audit.Enabled),
		zap.Bool("audit_has_syslog", c.Audit.Syslog != ""),
		zap.Bool("audit_has_webhook", c.Audit.WebhookURL != ""),
		zap.Bool("audit_has_webhook_token", c.Audit.WebhookToken != ""),
		zap.Bool("alert_enabled", c.Alert.Enabled),
		zap.Int("alert_check_interval_sec", c.Alert.CheckIntervalSec),
		zap.Bool("alert_watch_drbd_events", c.Alert.WatchDRBDEvents),
		zap.Int("alert_webhooks", len(c.Alert.Webhooks)),
		zap.Bool("alert_has_webhook_url", c.Alert.WebhookURL != ""),
		zap.Bool("schedule_enabled", c.Schedule.Enabled),
		zap.Bool("inspect_enabled", c.Inspect.Enabled),
		zap.String("inspect_schedule", c.Inspect.Schedule),
		zap.String("self_heal_auto_evict", c.SelfHeal.AutoEvict),
		zap.Bool("auto_tiebreaker", c.Resource.AutoTiebreaker),
		zap.Bool("auto_state_volume", c.Gateway.AutoStateVolume),
		zap.String("wan_pki_dir", c.WAN.PKIDir),
		zap.Strings("self_ha_extra_services", c.SelfHA.ExtraServices),
	)
}

// Validate validates the configuration
// validPoolTypeDefaults are the values storage.default_pool_type may take.
//
// ZFS is deliberately absent even though `haify pool create --type zfs` works:
// a zpool is built from vdevs by a separate RPC, so an unspecified type never
// resolves to one. Accepting "zfs" here would produce a controller that starts
// cleanly and then fails every pool creation that omits a type, with an error
// about LVM that names a setting the operator wrote on purpose.
var validPoolTypeDefaults = []string{"vg", "lvm", "lvm-thin", "thin-pool", "thin_pool", "lvm-thin-vdo", "thin_vdo"}

func (c *Config) Validate() error {
	log := zap.L()
	log.Debug("Config.Validate: start", zap.String("listen_address", c.Server.ListenAddress),
		zap.Int("port", c.Server.Port), zap.Int("rest_port", c.Server.RESTPort))
	if c.Server.ListenAddress == "" {
		log.Debug("Config.Validate: listen address unset, defaulting", zap.String("listen_address", "0.0.0.0"))
		c.Server.ListenAddress = "0.0.0.0"
	}
	if c.Server.Port == 0 {
		log.Debug("Config.Validate: port unset, defaulting", zap.Int("port", 3374))
		c.Server.Port = 3374
	}
	if c.Server.RESTPort == 0 {
		log.Debug("Config.Validate: rest port unset, defaulting", zap.Int("rest_port", 3375))
		c.Server.RESTPort = 3375
	}
	if c.Server.RESTPort < 1 || c.Server.RESTPort > 65535 {
		log.Debug("Config.Validate: rest port out of range", zap.Int("rest_port", c.Server.RESTPort))
		return fmt.Errorf("server.rest_port %d is not a TCP port", c.Server.RESTPort)
	}
	if c.Server.RESTPort == c.Server.Port {
		log.Debug("Config.Validate: rest port equals grpc port", zap.Int("port", c.Server.Port))
		return fmt.Errorf("server.rest_port and server.port are both %d; the REST gateway needs a port of its own", c.Server.Port)
	}
	if c.Log.Level == "" {
		log.Debug("Config.Validate: log level unset, defaulting", zap.String("log_level", "info"))
		c.Log.Level = "info"
	}
	if t := strings.ToLower(strings.TrimSpace(c.Storage.DefaultPoolType)); t != "" {
		if !slices.Contains(validPoolTypeDefaults, t) {
			log.Debug("Config.Validate: default pool type not creatable", zap.String("default_pool_type", t))
			return fmt.Errorf("storage.default_pool_type %q is not a pool type this controller can create (want one of %s)",
				c.Storage.DefaultPoolType, strings.Join(validPoolTypeDefaults, ", "))
		}
	}

	if spec := strings.TrimSpace(c.Storage.VerifySchedule); spec != "" {
		if _, err := cron.ParseStandard(spec); err != nil {
			log.Debug("Config.Validate: verify schedule not a cron spec", zap.String("verify_schedule", spec), zap.Error(err))
			return fmt.Errorf("storage.verify_schedule %q is not a cron spec: %w", spec, err)
		}
	} else {
		log.Debug("Config.Validate: verify schedule empty, online verify off")
	}

	if err := c.validateSections(); err != nil {
		log.Debug("Config.Validate: a section is invalid", zap.Error(err))
		return err
	}
	if c.Auth.Enabled {
		log.Debug("Config.Validate: auth enabled, checking token length", zap.Bool("has_auth_token", c.Auth.Token != ""))
		if len(c.Auth.Token) < 16 {
			log.Debug("Config.Validate: auth token too short")
			return fmt.Errorf("auth.token must be at least 16 characters when auth is enabled")
		}
	}
	if c.RBAC.Enabled {
		log.Debug("Config.Validate: rbac enabled, checking users", zap.Int("count", len(c.RBAC.Users)))
		if len(c.RBAC.Users) == 0 {
			log.Debug("Config.Validate: rbac enabled with no users")
			return fmt.Errorf("rbac.users must declare at least one user when rbac is enabled")
		}
		seen := make(map[string]struct{}, len(c.RBAC.Users))
		for i, u := range c.RBAC.Users {
			log.Debug("Config.Validate: checking rbac user", zap.Int("index", i), zap.String("user", u.Name),
				zap.String("role", u.Role), zap.Bool("has_token", u.Token != ""))
			if u.Name == "" {
				log.Debug("Config.Validate: rbac user has no name", zap.Int("index", i))
				return fmt.Errorf("rbac.users[%d].name is required", i)
			}
			if u.Role == "" {
				log.Debug("Config.Validate: rbac user has no role", zap.String("user", u.Name))
				return fmt.Errorf("rbac.users[%q].role is required", u.Name)
			}
			if len(u.Token) < 16 {
				log.Debug("Config.Validate: rbac user token too short", zap.String("user", u.Name))
				return fmt.Errorf("rbac.users[%q].token must be at least 16 characters", u.Name)
			}
			if _, dup := seen[u.Token]; dup {
				log.Debug("Config.Validate: rbac user reuses a token", zap.String("user", u.Name))
				return fmt.Errorf("rbac.users[%q] reuses a token assigned to another user", u.Name)
			}
			seen[u.Token] = struct{}{}
		}
	}
	if err := c.TLS.Validate(); err != nil {
		log.Debug("Config.Validate: tls section invalid", zap.Error(err))
		return err
	}
	err := c.Inspect.Validate()
	log.Debug("Config.Validate: done", zap.Error(err))
	return err
}

func setDefaults() {
	zap.L().Debug("setDefaults: registering config defaults")
	viper.SetDefault("server.listen_address", "0.0.0.0")
	viper.SetDefault("server.port", 3374)
	viper.SetDefault("server.rest_port", 3375)
	viper.SetDefault("database.path", "/var/lib/haify/haify.db")
	viper.SetDefault("auth.enabled", false)
	viper.SetDefault("tls.enabled", false)
	viper.SetDefault("log.level", "info")
	viper.SetDefault("log.format", "json")
	// Thin by default. A thick pool cannot hold a snapshot history: LVM makes
	// every snapshot reserve a fixed COW area up front (Haify reserves 20% of the
	// origin), so a 10 GiB pool holding a 6 GiB volume fits two snapshots —
	// which is not a retention policy. Thin snapshots cost only the blocks that
	// diverge. Set "vg" here for the thick behaviour on every client at once.
	viper.SetDefault("storage.default_pool_type", "thin_pool")
	viper.SetDefault("storage.default_snapshot_suffix", "_snap")
	// Monthly, 03:00 on the 1st. Two replicas that disagree both report
	// UpToDate; nothing but reading both finds it.
	viper.SetDefault("storage.verify_schedule", "0 3 1 * *")
	viper.SetDefault("metrics.enabled", true)
	viper.SetDefault("metrics.listen_address", "0.0.0.0")
	viper.SetDefault("metrics.port", 9433)
	setSectionDefaults()
	viper.SetDefault("rbac.enabled", false)
	viper.SetDefault("gateway.auto_state_volume", true)
	viper.SetDefault("gateway.state_volume_size_gb", 1)
	viper.SetDefault("resource.auto_tiebreaker", true)
	viper.SetDefault("resource.fault_domain_label", "host")
	viper.SetDefault("schedule.enabled", true)
	viper.SetDefault("alert.enabled", true)
	viper.SetDefault("alert.warning_hold_sec", 30)
	viper.SetDefault("alert.check_interval_sec", 30)
	viper.SetDefault("alert.check_nodes", true)
	viper.SetDefault("alert.watch_drbd_events", true)
	viper.SetDefault("alert.idle_interval_sec", 300)
	viper.SetDefault("alert.history_size", 500)
	// On by default: a thin pool filling up is silent until it is fatal, and it
	// costs nothing extra to watch.
	viper.SetDefault("alert.check_pools", true)
	viper.SetDefault("alert.pool_near_full_percent", 85.0)
	viper.SetDefault("alert.pool_full_percent", 95.0)
	inspectDefaults()
}

// Save saves configuration to file
// Save writes the configuration back out.
//
// Sections go through mapstructure rather than straight into viper.Set: viper
// serialises a struct under its Go field names ("DefaultPoolType"), while Load
// reads the mapstructure tags ("default_pool_type"). Setting the struct
// directly produces a file that looks right and silently loses every value on
// the next Load — every section, not just one. mapstructure.Decode turns the
// struct into a map keyed by those same tags, so what is written is what is
// read back.
func (c *Config) Save(path string) error {
	log := zap.L()
	log.Debug("Config.Save: start", zap.String("path", path))
	config := viper.New()
	sections := []struct {
		name string
		v    any
	}{
		{"server", c.Server}, {"database", c.Database}, {"auth", c.Auth},
		{"tls", c.TLS}, {"log", c.Log}, {"storage", c.Storage},
		{"metrics", c.Metrics}, {"audit", c.Audit}, {"rbac", c.RBAC},
		{"gateway", c.Gateway}, {"resource", c.Resource},
		{"schedule", c.Schedule}, {"alert", c.Alert}, {"inspect", c.Inspect}, {"self_heal", c.SelfHeal}, {"quota", c.Quota},
	}
	for _, s := range sections {
		var m map[string]any
		if err := mapstructure.Decode(s.v, &m); err != nil {
			log.Debug("Config.Save: encoding section failed", zap.String("section", s.name), zap.Error(err))
			return fmt.Errorf("encode %s section: %w", s.name, err)
		}
		log.Debug("Config.Save: section encoded", zap.String("section", s.name), zap.Int("keys", len(m)))
		config.Set(s.name, m)
	}

	start := time.Now()
	err := config.WriteConfigAs(path)
	log.Debug("Config.Save: file written", zap.String("path", path), zap.Error(err), zap.Duration("took", time.Since(start)))
	return err
}
