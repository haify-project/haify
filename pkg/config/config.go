// Package config provides configuration management
package config

import "go.uber.org/zap"

// Config represents the application configuration
type Config struct {
	Server   ServerConfig   `mapstructure:"server"`
	Database DatabaseConfig `mapstructure:"database"`
	Dispatch DispatchConfig `mapstructure:"dispatch"`
	WAN      WANConfig      `mapstructure:"wan"`
	Auth     AuthConfig     `mapstructure:"auth"`
	TLS      TLSConfig      `mapstructure:"tls"`
	Log      LogConfig      `mapstructure:"log"`
	Storage  StorageConfig  `mapstructure:"storage"`
	Metrics  MetricsConfig  `mapstructure:"metrics"`
	UI       UIConfig       `mapstructure:"ui"`
	Audit    AuditConfig    `mapstructure:"audit"`
	RBAC     RBACConfig     `mapstructure:"rbac"`
	Gateway  GatewayConfig  `mapstructure:"gateway"`
	Resource ResourceConfig `mapstructure:"resource"`
	Schedule ScheduleConfig `mapstructure:"schedule"`
	SelfHA   SelfHAConfig   `mapstructure:"self_ha"`
	Alert    AlertConfig    `mapstructure:"alert"`
	Inspect  InspectConfig  `mapstructure:"inspect"`
	SelfHeal SelfHealConfig `mapstructure:"self_heal"`
	Quota    QuotaConfig    `mapstructure:"quota"`
}

// WANConfig tunes opt-in WAN replication.
//
// PKIDir is where the controller caches the shared CA + leaf used for the
// haify-proxy mTLS link. It defaults to /var/lib/haify/wanproxy-pki, which only a
// root controller can create; point it somewhere writable when the controller
// runs as an ordinary user (development, a test harness, or a packaged service
// with its own state directory).
type WANConfig struct {
	PKIDir string `mapstructure:"pki_dir"`
}

// DispatchConfig points the controller at the dispatch SSH configuration it
// uses to reach storage nodes.
//
// ConfigPath matters whenever the controller does NOT run as a user whose
// `~/.dispatch/config.toml` is the intended one — a systemd unit with a
// different HOME, a non-root operator, or a test harness keeping its config out
// of the home directory. Leave it empty to keep the dispatch default
// (`~/.dispatch/config.toml`, falling back to `~/.ssh/config`).
//
// Parallel caps how many nodes a single dispatch call fans out to; 0 keeps the
// deployment client's built-in default.
type DispatchConfig struct {
	ConfigPath string `mapstructure:"config_path"`
	Parallel   int    `mapstructure:"parallel"`
}

// AlertConfig controls background health polling and how the resulting
// notifications are delivered.
//
// Enabling this starts the detector and the event bus. Delivery is separate and
// optional: with no Webhook configured the events are still readable over
// `GET /v1/events`, streamed over `GET /v1/events/watch`, and pushed to browsers
// over `GET /v1/events/stream`.
type AlertConfig struct {
	Enabled bool `mapstructure:"enabled"`
	// WebhookURL is the single-receiver shorthand. Equivalent to one entry in
	// Webhooks; both may be set, and both receive events.
	WebhookURL string `mapstructure:"webhook_url"`
	// WebhookMinSeverity filters WebhookURL: "info" (default), "warning" or
	// "critical".
	WebhookMinSeverity string `mapstructure:"webhook_min_severity"`
	// Webhooks are additional receivers, each with its own severity threshold —
	// a pager on "critical", a chat channel on "info".
	Webhooks         []WebhookReceiver `mapstructure:"webhooks"`
	CheckIntervalSec int               `mapstructure:"check_interval_sec"`
	// CheckNodes enables SSH reachability probing of every registered node on
	// each poll. It is what produces node.unreachable events, and it is the only
	// check that costs a round trip per node, so it has its own switch.
	CheckNodes bool `mapstructure:"check_nodes"`
	// WarningHoldSec is how long a warning condition must last before it is
	// raised, so a link that drops and reconnects within it is not reported.
	// Critical conditions are never held. Zero raises warnings at once.
	WarningHoldSec int `mapstructure:"warning_hold_sec"`
	// HistorySize is how many past events are retained for late-joining clients.
	// Zero uses the event package default.
	HistorySize int `mapstructure:"history_size"`
	// CheckPools enables thin pool capacity alerting. It rides on the pool
	// listing the controller already serves, so unlike CheckNodes it costs no
	// extra round trip per subject — but it is still a switch, because a
	// cluster that has deliberately overcommitted its pools does not want to be
	// told so every poll.
	CheckPools bool `mapstructure:"check_pools"`
	// PoolNearFullPercent and PoolFullPercent are the thin pool utilisation
	// thresholds, applied to data and metadata alike. Zero uses the alert
	// package defaults (85 and 95).
	PoolNearFullPercent float64 `mapstructure:"pool_near_full_percent"`
	PoolFullPercent     float64 `mapstructure:"pool_full_percent"`
	// WatchDRBDEvents keeps a `drbdsetup events2` stream open to every node,
	// so a DRBD state change is checked within seconds instead of at the next
	// poll, and a healthy cluster is polled every IdleIntervalSec instead of
	// every CheckIntervalSec. Each poll is several SSH sessions per node.
	WatchDRBDEvents bool `mapstructure:"watch_drbd_events"`
	// IdleIntervalSec is the poll interval while every node's events arrive
	// and nothing is degraded or resyncing. It bounds how late what DRBD does
	// not report — a thin pool filling — is noticed.
	IdleIntervalSec int `mapstructure:"idle_interval_sec"`
	// WriteAnomaly is [alert.write_anomaly] (write_anomaly.go).
	WriteAnomaly WriteAnomalyConfig `mapstructure:"write_anomaly"`
}

// WebhookReceiver is one HTTP notification target.
type WebhookReceiver struct {
	URL string `mapstructure:"url"`
	// MinSeverity drops anything less severe: "info" (default), "warning",
	// "critical".
	MinSeverity string `mapstructure:"min_severity"`
	// Headers are sent with every request — an auth token, a routing key.
	Headers map[string]string `mapstructure:"headers"`
}

// Receivers returns every configured Webhook, folding the WebhookURL shorthand
// in as the first entry. Receivers without a URL are skipped rather than
// producing a receiver that fails on every delivery.
func (a AlertConfig) Receivers() []WebhookReceiver {
	var out []WebhookReceiver
	if a.WebhookURL != "" {
		out = append(out, WebhookReceiver{
			URL:         a.WebhookURL,
			MinSeverity: a.WebhookMinSeverity,
		})
	}
	for i, w := range a.Webhooks {
		if w.URL != "" {
			out = append(out, w)
		} else {
			zap.L().Debug("AlertConfig.Receivers: skipping webhook without a URL", zap.Int("index", i))
		}
	}
	zap.L().Debug("AlertConfig.Receivers: built receivers", zap.Bool("has_webhook_url", a.WebhookURL != ""),
		zap.Int("webhooks", len(a.Webhooks)), zap.Int("count", len(out)))
	return out
}

// SelfHAConfig configures controller Self-HA.
type SelfHAConfig struct {
	// ExtraServices are additional systemd units started/stopped alongside the
	// controller on the active node (appended to the Self-HA promoter start
	// list), so they follow the controller across failover. Empty by default;
	// e.g. ["haify-ai.service"] to make the AI Copilot follow the controller.
	ExtraServices []string `mapstructure:"extra_services"`
}

// ScheduleConfig is the master switch for the snapshot scheduler. When enabled,
// the active controller runs cron-driven snapshot schedules and prunes old
// snapshots per each schedule's GFS retention policy. Disabling it stops all
// scheduled snapshots without deleting the schedule definitions.
type ScheduleConfig struct {
	Enabled bool `mapstructure:"enabled"`
}

// ResourceConfig controls DRBD resource provisioning behavior. A 2-node
// resource under quorum=majority cannot keep serving I/O when either node
// fails (the survivor has no majority). When AutoTiebreaker is set, the
// controller automatically adds a third diskless node — one that only votes
// in quorum and stores no data — so the cluster keeps a majority through any
// single-node failure, matching LINSTOR's auto-quorum-tiebreaker behavior.
type ResourceConfig struct {
	AutoTiebreaker bool `mapstructure:"auto_tiebreaker"`
	// FaultDomainLabel is the node label naming what fails together, e.g. the
	// physical host a VM runs on. Automatic placement spreads replicas and the
	// tiebreaker across its values. A node without the label is its own domain.
	FaultDomainLabel string `mapstructure:"fault_domain_label"`
}

// GatewayConfig controls gateway provisioning behavior. Every gateway needs a
// small cluster-private "state" volume (volume 0) in addition to the data
// volume so the OCF agents can carry failover state. When AutoStateVolume is
// set, the controller provisions that volume automatically during gateway
// creation instead of failing when the resource has only one volume.
type GatewayConfig struct {
	AutoStateVolume   bool   `mapstructure:"auto_state_volume"`
	StateVolumeSizeGB uint32 `mapstructure:"state_volume_size_gb"`
}

// RBACConfig controls Casbin-backed role authorization. When enabled, callers
// authenticate with a per-user bearer token and every API call is checked
// against the caller's role. Users and any extra policies are declared here, so
// RBAC needs no extra service or database — it stays inside the controller
// binary. When disabled, the single-token [auth] model applies.
type RBACConfig struct {
	Enabled  bool         `mapstructure:"enabled"`
	Users    []RBACUser   `mapstructure:"users"`
	Policies []RBACPolicy `mapstructure:"policies"`
	// Approval puts listed calls behind a second person (approval.go).
	Approval ApprovalConfig `mapstructure:"approval"`
}

// RBACUser is an identity with a bearer token and a role. Built-in roles are
// admin, operator and viewer; custom role names require matching policies.
type RBACUser struct {
	Name  string `mapstructure:"name"`
	Token string `mapstructure:"token"`
	Role  string `mapstructure:"role"`
}

// RBACPolicy grants a role an action on an object (both may be "*"). These are
// additive on top of the built-in role defaults.
type RBACPolicy struct {
	Role   string `mapstructure:"role"`
	Object string `mapstructure:"object"`
	Action string `mapstructure:"action"`
}

// AuthConfig controls API authentication. When enabled, every gRPC and REST
// request must carry "Authorization: Bearer <token>"; only gRPC health
// checks stay open for liveness probes.
type AuthConfig struct {
	Enabled bool   `mapstructure:"enabled"`
	Token   string `mapstructure:"token"`
}

// ServerConfig represents server configuration
type ServerConfig struct {
	ListenAddress string `mapstructure:"listen_address"`
	Port          int    `mapstructure:"port"`
	// RESTPort is where the grpc-gateway REST API listens, on ListenAddress.
	// The web UI proxies /v1/ to it on loopback.
	RESTPort int `mapstructure:"rest_port"`
}

// DatabaseConfig represents database configuration
type DatabaseConfig struct {
	Path string `mapstructure:"path"` // Database file path (default: /var/lib/haify/haify.db)
}

// LogConfig represents logging configuration
type LogConfig struct {
	Level  string `mapstructure:"level"`
	Format string `mapstructure:"format"` // json or text
}

// StorageConfig represents storage configuration
type StorageConfig struct {
	DefaultPoolType       string `mapstructure:"default_pool_type"`
	DefaultSnapshotSuffix string `mapstructure:"default_snapshot_suffix"`
	// VerifySchedule is a cron spec on which every resource's replicas are
	// compared block by block (DRBD online verify), one resource at a time.
	// Differences raise resource.out_of_sync. Empty turns it off.
	VerifySchedule string `mapstructure:"verify_schedule"`
	// Thin is [storage.thin] (thin.go).
	Thin ThinConfig `mapstructure:"thin"`
}

// MetricsConfig represents metrics configuration
type MetricsConfig struct {
	Enabled       bool   `mapstructure:"enabled"`
	ListenAddress string `mapstructure:"listen_address"`
	Port          int    `mapstructure:"port"`
	// NodeExporterPort is the port node_exporter listens on on every node,
	// as scripts/install-node-exporter.sh sets it up; the listener's
	// /discovery/node-exporter hands Prometheus one such target per node.
	NodeExporterPort int `mapstructure:"node_exporter_port"`
	// PrometheusURL is the Prometheus server that scrapes the nodes'
	// node_exporter, e.g. http://prometheus:9090. The dashboard's node
	// statistics are read from it; empty leaves them out.
	PrometheusURL string `mapstructure:"prometheus_url"`
}

// UIConfig controls the embedded web UI server. It was documented and written
// into deployed controller.toml files, but nothing read it: the listener was
// hardcoded, so a config asking for a different port bound the default instead
// and said nothing.
type UIConfig struct {
	// Enabled defaults to true — the UI has always been served, and a config
	// that omits the section must keep getting it.
	Enabled       *bool  `mapstructure:"enabled"`
	ListenAddress string `mapstructure:"listen_address"`
	Port          int    `mapstructure:"port"`
}

// UIEnabled reports whether the embedded UI should be served.
func (u UIConfig) UIEnabled() bool { return u.Enabled == nil || *u.Enabled }

// UIAddress returns the address the UI should bind, falling back to the API
// listen address and the historic port.
func (u UIConfig) UIAddress(serverListen string) (string, int) {
	addr, port := u.ListenAddress, u.Port
	if addr == "" {
		addr = serverListen
	}
	if port == 0 {
		port = DefaultUIPort
	}
	zap.L().Debug("UIConfig.UIAddress: resolved", zap.String("listen_address", u.ListenAddress),
		zap.Int("port", u.Port), zap.String("server_listen", serverListen),
		zap.String("addr", addr), zap.Int("resolved_port", port))
	return addr, port
}

// DefaultUIPort is the port the UI bound before [ui] was honoured.
const DefaultUIPort = 3376
