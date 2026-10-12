package wanproxy

import (
	"fmt"
	"strings"

	"go.uber.org/zap"
)

// haify-proxy config defaults. These mirror config.example.toml so a generated
// config behaves identically to the documented defaults. See the haify-proxy repo
// (config.example.toml) for the authoritative schema.
const (
	// DefaultBulkCapBytes is the Bulk-lane backpressure cap (4 GiB).
	DefaultBulkCapBytes = 4294967296

	// DefaultZstdLevel selects zstd level 3 (0 disables compression).
	DefaultZstdLevel = 3

	// DefaultZstdMinSize is the minimum frame size that gets compressed.
	DefaultZstdMinSize = 64

	// DefaultOverflowGraceSecs applies only to on_congestion = "disconnect"; it
	// is emitted for schema completeness.
	DefaultOverflowGraceSecs = 30

	// DefaultAcceptorParkSecs is how long the acceptor holds a DRBD-B leg open
	// across a transient WAN drop (acceptor only).
	DefaultAcceptorParkSecs = 30

	// DefaultMetricsIntervalSecs is how often the proxy republishes its counters.
	// 5s keeps `resource status` reasonably fresh while costing one small file
	// write per interval.
	DefaultMetricsIntervalSecs = 5

	// CongestionPullAhead keeps the local DRBD connection alive under WAN
	// congestion so DRBD's own on-congestion pull-ahead engages. This is the WAN
	// default and requires `on-congestion pull-ahead;` in the DRBD net {} section
	// (which the WAN branch of generateDrbdConfig emits).
	CongestionPullAhead = "pull-ahead"

	// CertSAN is the SAN carried by the shared leaf certificate; both proxies
	// pin their peer to this name via the [tls] peer_name field.
	CertSAN = "haify-proxy"

	// roleDialer runs on the primary site; roleAcceptor runs on the DR site.
	roleDialer   = "dialer"
	roleAcceptor = "acceptor"
)

// loopback is the address DRBD and the proxy meet on; both are always on the
// same host so the routed traffic never leaves loopback.
const loopback = "127.0.0.1"

// RenderDialerConfig renders the primary-site (dialer) haify-proxy config for the
// resource in spec. The dialer binds the loopback DRBD port (P) and accepts the
// local DRBD-A connection, and dials the DR site's WAN endpoint over mTLS.
//
// wan_listen is unused by the dialer role but is still required by the schema,
// so it is emitted (bound to the WAN port for symmetry with the acceptor).
func RenderDialerConfig(spec ProxySpec) string {
	zap.L().Debug("RenderDialerConfig: rendering", zap.String("resource", spec.Resource),
		zap.String("dr_endpoint", spec.DRPublicEndpoint), zap.Int("wan_port", spec.WANPort), zap.Int("drbd_port", spec.DRBDPort))
	var b strings.Builder
	writeKV(&b, "role", roleDialer)
	// Bind here and accept the local DRBD-A connection (127.0.0.1:P).
	writeKV(&b, "drbd_listen", fmt.Sprintf("%s:%d", loopback, spec.DRBDPort))
	// The DR site's acceptor WAN address, dialed over mTLS.
	writeKV(&b, "peer", fmt.Sprintf("%s:%d", spec.DRPublicEndpoint, spec.WANPort))
	// Unused by the dialer, but required by the schema.
	writeKV(&b, "wan_listen", fmt.Sprintf("0.0.0.0:%d", spec.WANPort))
	writeInt(&b, "bulk_cap_bytes", DefaultBulkCapBytes)
	writeInt(&b, "zstd_level", DefaultZstdLevel)
	writeInt(&b, "zstd_min_size", DefaultZstdMinSize)
	writeKV(&b, "on_congestion", CongestionPullAhead)
	writeInt(&b, "overflow_grace_secs", DefaultOverflowGraceSecs)
	// Pin the egress source when asked, so replication leaves over the intended
	// interface. Port 0: the kernel picks an ephemeral port, which a reconnect
	// needs (a fixed port would hit EADDRINUSE during TIME_WAIT).
	if addr := strings.TrimSpace(spec.PrimaryEgressAddr); addr != "" {
		zap.L().Debug("RenderDialerConfig: pinning egress source", zap.String("resource", spec.Resource), zap.String("egress", addr))
		writeKV(&b, "bind_addr", fmt.Sprintf("%s:0", addr))
	} else {
		zap.L().Debug("RenderDialerConfig: no egress pin, routing decides", zap.String("resource", spec.Resource))
	}
	// Publish runtime counters where `Status` can read them back over SSH. The
	// backlog figure is the only way to answer how much data a protocol A
	// failover would lose, so it is always enabled.
	writeKV(&b, "metrics_path", NodeMetricsPath(spec.Resource))
	writeInt(&b, "metrics_interval_secs", DefaultMetricsIntervalSecs)
	writeTLS(&b)
	return b.String()
}

// RenderAcceptorConfig renders the DR-site (acceptor) haify-proxy config. The
// acceptor binds the WAN port and accepts the peer proxy's mTLS connection, and
// dials the local DRBD-B at the loopback DRBD port (P).
//
// peer is unused by the acceptor role (the dialer connects to us), but is still
// required by the schema, so a valid placeholder host:port is emitted.
func RenderAcceptorConfig(spec ProxySpec) string {
	zap.L().Debug("RenderAcceptorConfig: rendering", zap.String("resource", spec.Resource),
		zap.Int("wan_port", spec.WANPort), zap.Int("drbd_port", spec.DRBDPort))
	var b strings.Builder
	writeKV(&b, "role", roleAcceptor)
	// Dial the local DRBD-B endpoint here (DRBD-B listens on 127.0.0.1:P).
	writeKV(&b, "drbd_listen", fmt.Sprintf("%s:%d", loopback, spec.DRBDPort))
	// Unused by the acceptor (the peer proxy dials us); required by the schema.
	writeKV(&b, "peer", fmt.Sprintf("0.0.0.0:%d", spec.WANPort))
	// Bind here and accept the peer proxy's mTLS connection.
	writeKV(&b, "wan_listen", fmt.Sprintf("0.0.0.0:%d", spec.WANPort))
	writeInt(&b, "bulk_cap_bytes", DefaultBulkCapBytes)
	writeInt(&b, "zstd_level", DefaultZstdLevel)
	writeInt(&b, "zstd_min_size", DefaultZstdMinSize)
	writeKV(&b, "on_congestion", CongestionPullAhead)
	writeInt(&b, "overflow_grace_secs", DefaultOverflowGraceSecs)
	// Publish runtime counters where `Status` can read them back over SSH. The
	// backlog figure is the only way to answer how much data a protocol A
	// failover would lose, so it is always enabled.
	writeKV(&b, "metrics_path", NodeMetricsPath(spec.Resource))
	writeInt(&b, "metrics_interval_secs", DefaultMetricsIntervalSecs)
	// Acceptor-only knobs.
	writeInt(&b, "acceptor_park_secs", DefaultAcceptorParkSecs)
	writeBool(&b, "synthesize_ping_acks", true)
	writeTLS(&b)
	return b.String()
}

// writeTLS emits the [tls] table pointing at the deployed shared cert files and
// pinning the peer to the shared leaf's SAN.
func writeTLS(b *strings.Builder) {
	b.WriteString("\n[tls]\n")
	writeKV(b, "ca", NodeCAPath)
	writeKV(b, "cert", NodeCertPath)
	writeKV(b, "key", NodeKeyPath)
	writeKV(b, "peer_name", CertSAN)
}

// writeKV writes a `key = "value"` TOML line.
func writeKV(b *strings.Builder, key, value string) {
	fmt.Fprintf(b, "%s = %q\n", key, value)
}

// writeInt writes a `key = value` TOML line for an integer.
func writeInt(b *strings.Builder, key string, value int64) {
	fmt.Fprintf(b, "%s = %d\n", key, value)
}

// writeBool writes a `key = true/false` TOML line.
func writeBool(b *strings.Builder, key string, value bool) {
	fmt.Fprintf(b, "%s = %t\n", key, value)
}
