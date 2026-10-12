package inspect

import (
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"time"

	"go.uber.org/zap"
)

// Certificate expiry thresholds.
const (
	certWarn = 30 * 24 * time.Hour
	certFail = 7 * 24 * time.Hour
)

// checkTLS judges certificate expiry: the API server certificate, the
// replication CA, and every node's replication certificate.
func checkTLS(in *Input) []Check {
	log := zap.L()
	t := in.TLS
	log.Debug("checkTLS: start", zap.Bool("has_api_cert", t.APICert != nil), zap.String("api_cert_path", t.APICertPath),
		zap.Bool("api_cert_error", t.APICertErr != ""), zap.Bool("has_replication_ca", t.ReplicationCA != nil))
	var out []Check
	seen := 0
	if t.APICertErr != "" {
		out = append(out, Check{ID: "tls.api_cert", Area: AreaTLS, Subject: t.APICertPath, Status: StatusError,
			Message: "the API server certificate could not be read: " + t.APICertErr})
	}
	if t.APICert != nil {
		seen++
		if c, ok := expiry(in, "tls.api_cert", t.APICertPath, "API server certificate", t.APICert,
			"replace "+t.APICertPath+" and restart haify-controller"); ok {
			out = append(out, c)
		}
	}
	if t.ReplicationCA != nil {
		seen++
		if c, ok := expiry(in, "tls.replication_ca", t.ReplicationCAPath, "replication CA", t.ReplicationCA,
			"haify replication-tls setup"); ok {
			out = append(out, c)
		}
	}
	for _, node := range sortedKeys(in.Probes) {
		raw := in.Probes[node].TLSCert
		if len(raw) == 0 {
			log.Debug("checkTLS: node has no replication certificate", zap.String("node", node))
			continue
		}
		seen++
		cert, err := parsePEMCert(raw)
		if err != nil {
			log.Debug("checkTLS: node certificate unreadable", zap.String("node", node), zap.Error(err))
			out = append(out, Check{ID: "tls.replication_cert", Area: AreaTLS, Subject: node, Status: StatusFail,
				Message: "/etc/haify/drbd-tls/node.crt is unreadable: " + err.Error(), Fix: "haify replication-tls setup"})
			continue
		}
		if c, ok := expiry(in, "tls.replication_cert", node, "replication certificate", cert, "haify replication-tls setup"); ok {
			out = append(out, c)
		}
	}
	log.Debug("checkTLS: done", zap.Int("certificates", seen), zap.Int("findings", len(out)))
	if len(out) == 0 && seen == 0 {
		out = append(out, pass("tls.expiry", AreaTLS, "no TLS certificates in use"))
	}
	if len(out) == 0 {
		out = append(out, pass("tls.expiry", AreaTLS, "%s valid for more than %d days",
			plural(seen, "certificate", "certificates"), int(certWarn.Hours()/24)))
	}
	return out
}

func expiry(in *Input, id, subject, what string, cert *x509.Certificate, fix string) (Check, bool) {
	left := cert.NotAfter.Sub(in.Now)
	zap.L().Debug("expiry: certificate", zap.String("id", id), zap.String("subject", subject),
		zap.Time("not_after", cert.NotAfter), zap.Duration("left", left), zap.Bool("ok", left >= certWarn))
	if left >= certWarn {
		return Check{}, false
	}
	c := Check{ID: id, Area: AreaTLS, Subject: subject, Status: StatusWarn, Fix: fix,
		Evidence: []string{"not after " + cert.NotAfter.UTC().Format(time.RFC3339), "subject " + cert.Subject.String()}}
	switch {
	case left <= 0:
		c.Status = StatusFail
		c.Message = fmt.Sprintf("%s expired %s ago", what, (-left).Round(time.Hour))
	case left < certFail:
		c.Status = StatusFail
		c.Message = fmt.Sprintf("%s expires in %s", what, left.Round(time.Hour))
	default:
		c.Message = fmt.Sprintf("%s expires in %d days", what, int(left.Hours()/24))
	}
	return c, true
}

// parsePEMCert reads the first certificate of a PEM file.
func parsePEMCert(raw []byte) (*x509.Certificate, error) {
	block, _ := pem.Decode(raw)
	if block == nil {
		zap.L().Debug("parsePEMCert: no PEM block", zap.Int("bytes", len(raw)))
		return nil, fmt.Errorf("no PEM block")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	zap.L().Debug("parsePEMCert: parsed", zap.Error(err))
	return cert, err
}

// ParseCertFile reads the first certificate of PEM data; exported for the
// controller, which reads the API and CA certificates locally.
func ParseCertFile(raw []byte) (*x509.Certificate, error) { return parsePEMCert(raw) }
