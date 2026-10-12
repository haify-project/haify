package config

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
	"time"

	"go.uber.org/zap"
)

// TLSConfig controls transport security for the gRPC API (and, through the
// in-process REST gateway, for everything the web UI calls).
//
// This section used to be pure decoration: nothing in the tree read it, so
// `enabled = true` produced a plaintext listener while the startup log happily
// reported TLS as on — the worst kind of failure, a security switch that
// claims to be closed. Every field below is now read at startup and every
// unusable combination is rejected there, not at the first connection.
//
// The fields are named from the CONTROLLER's point of view, because that is
// who reads them: CertFile/KeyFile are the certificate this server presents.
// The old names (ca_cert / client_cert / client_key) described a *client*, and
// a config still carrying them is rejected rather than reinterpreted — see
// Validate. The client-side spellings live on in pkg/client, where they are
// correct.
type TLSConfig struct {
	Enabled bool `mapstructure:"enabled"`
	// CertFile and KeyFile are the server certificate chain and its private
	// key, PEM encoded. Both are required when Enabled.
	CertFile string `mapstructure:"cert_file"`
	KeyFile  string `mapstructure:"key_file"`
	// ClientCAFile is the CA bundle that signs client certificates. Setting it
	// is what turns on mutual TLS: the server then requires and verifies a
	// client certificate on every connection. There is deliberately no
	// separate "require_client_cert" switch — a CA configured but not enforced
	// is exactly the kind of security setting that looks on while being off.
	ClientCAFile string `mapstructure:"client_ca_file"`
	// REST also serves the REST gateway (rest_port) over TLS, with the same
	// certificate. Off by default because REST clients written against plain
	// HTTP stop working. Server-side only: REST callers authenticate with a
	// bearer token, and browsers behind the web UI have no client certificate.
	REST bool `mapstructure:"rest"`

	// Legacy client-viewpoint names. They are still decoded so that a config
	// which sets them fails loudly at startup instead of being silently
	// ignored (which, for three releases, is what happened to the whole
	// section). They are never used for anything else.
	LegacyCACert     string `mapstructure:"ca_cert,omitempty"`
	LegacyClientCert string `mapstructure:"client_cert,omitempty"`
	LegacyClientKey  string `mapstructure:"client_key,omitempty"`
}

// MutualTLS reports whether client certificates are required and verified.
func (t TLSConfig) MutualTLS() bool { return t.Enabled && t.ClientCAFile != "" }

// Validate rejects a [tls] section that cannot work, at startup rather than at
// the first connection. A controller that boots "with TLS" and only discovers
// at the first handshake that its key does not match its certificate has
// already told the operator it is secure.
func (t TLSConfig) Validate() error {
	log := zap.L()
	log.Debug("TLSConfig.Validate: start", zap.Bool("enabled", t.Enabled), zap.Bool("rest", t.REST),
		zap.String("cert_file", t.CertFile), zap.String("key_file", t.KeyFile),
		zap.String("client_ca_file", t.ClientCAFile))
	if t.LegacyCACert != "" || t.LegacyClientCert != "" || t.LegacyClientKey != "" {
		log.Debug("TLSConfig.Validate: legacy client-side fields set, rejecting")
		return fmt.Errorf("tls: ca_cert/client_cert/client_key name a *client's* material and are no longer read; " +
			"the controller needs its own certificate — set tls.cert_file and tls.key_file, " +
			"and tls.client_ca_file to require client certificates. " +
			"(haify keeps --tls-ca/--tls-cert/--tls-key for the client side.)")
	}
	if !t.Enabled {
		if t.REST {
			log.Debug("TLSConfig.Validate: rest TLS asked for without tls enabled")
			return fmt.Errorf("tls.rest needs tls.enabled: the REST gateway is served with tls.cert_file")
		}
		log.Debug("TLSConfig.Validate: tls disabled, nothing to check")
		return nil
	}
	if t.CertFile == "" || t.KeyFile == "" {
		log.Debug("TLSConfig.Validate: cert or key file missing",
			zap.Bool("has_cert_file", t.CertFile != ""), zap.Bool("has_key_file", t.KeyFile != ""))
		return fmt.Errorf("tls.cert_file and tls.key_file are required when tls.enabled is true")
	}
	// LoadX509KeyPair is the check, not a formality: it catches a missing or
	// unreadable file, a PEM block that is not a certificate, and — the one an
	// existence check would wave through — a key that does not belong to the
	// certificate.
	start := time.Now()
	if _, err := tls.LoadX509KeyPair(t.CertFile, t.KeyFile); err != nil {
		log.Debug("TLSConfig.Validate: loading key pair failed", zap.String("cert_file", t.CertFile),
			zap.String("key_file", t.KeyFile), zap.Error(err), zap.Duration("took", time.Since(start)))
		return fmt.Errorf("tls: cannot load cert_file %q with key_file %q: %w", t.CertFile, t.KeyFile, err)
	}
	log.Debug("TLSConfig.Validate: key pair loaded", zap.String("cert_file", t.CertFile),
		zap.Duration("took", time.Since(start)))
	if t.ClientCAFile != "" {
		start = time.Now()
		pem, err := os.ReadFile(t.ClientCAFile)
		log.Debug("TLSConfig.Validate: read client CA file", zap.String("client_ca_file", t.ClientCAFile),
			zap.Int("bytes", len(pem)), zap.Error(err), zap.Duration("took", time.Since(start)))
		if err != nil {
			return fmt.Errorf("tls: cannot read client_ca_file %q: %w", t.ClientCAFile, err)
		}
		// An unparsable bundle would otherwise produce an empty pool, and an
		// empty ClientCAs pool rejects every client — mutual TLS that locks
		// out the whole cluster instead of failing to start.
		if !x509.NewCertPool().AppendCertsFromPEM(pem) {
			log.Debug("TLSConfig.Validate: client CA file holds no certificate", zap.String("client_ca_file", t.ClientCAFile))
			return fmt.Errorf("tls: client_ca_file %q contains no PEM certificate", t.ClientCAFile)
		}
		log.Debug("TLSConfig.Validate: mutual TLS on", zap.String("client_ca_file", t.ClientCAFile))
	} else {
		log.Debug("TLSConfig.Validate: no client CA, server-only TLS")
	}
	return nil
}
