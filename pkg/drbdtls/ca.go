// Package drbdtls is the certificate authority behind encrypted DRBD
// replication. DRBD hands the TLS handshake to the kernel, which hands it to
// tlshd on each node; tlshd presents the node's certificate and accepts a peer
// whose certificate chains to a CA in the system trust store. This CA signs one
// certificate per node, from a request the node makes with a key that never
// leaves it.
package drbdtls

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"

	"go.uber.org/zap"
)

const (
	caCertFile = "ca.pem"
	caKeyFile  = "ca-key.pem"

	caValidity   = 20 * 365 * 24 * time.Hour
	nodeValidity = 5 * 365 * 24 * time.Hour
)

// CA signs node certificates for DRBD replication.
type CA struct {
	cert    *x509.Certificate
	key     *ecdsa.PrivateKey
	CertPEM []byte
}

// Ensure loads the CA kept in dir, creating it on first use. The key is
// written 0600 in a 0700 directory; it is the one secret that lets a machine
// join replication, so it stays with the controller.
func Ensure(dir string) (*CA, error) {
	log := zap.L()
	log.Debug("Ensure: start", zap.String("dir", dir))
	start := time.Now()
	certPEM, certErr := os.ReadFile(filepath.Join(dir, caCertFile))
	keyPEM, keyErr := os.ReadFile(filepath.Join(dir, caKeyFile))
	log.Debug("Ensure: read CA files", zap.String("dir", dir), zap.NamedError("cert_error", certErr),
		zap.NamedError("key_error", keyErr), zap.Duration("took", time.Since(start)))
	if certErr == nil && keyErr == nil {
		log.Debug("Ensure: both CA files present, loading", zap.String("dir", dir))
		return parseCA(certPEM, keyPEM)
	}
	if !errors.Is(certErr, os.ErrNotExist) || !errors.Is(keyErr, os.ErrNotExist) {
		log.Debug("Ensure: CA incomplete or unreadable", zap.String("dir", dir),
			zap.Bool("cert_present", certErr == nil), zap.Bool("key_present", keyErr == nil))
		return nil, fmt.Errorf("replication CA in %s is incomplete; restore both %s and %s, or remove both to start over",
			dir, caCertFile, caKeyFile)
	}
	log.Debug("Ensure: no CA yet, creating one", zap.String("dir", dir))
	ca, keyPEM, err := newCA()
	if err != nil {
		log.Debug("Ensure: creating CA failed", zap.Error(err))
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		log.Debug("Ensure: creating CA dir failed", zap.String("dir", dir), zap.Error(err))
		return nil, fmt.Errorf("create %s: %w", dir, err)
	}
	start = time.Now()
	if err := os.WriteFile(filepath.Join(dir, caKeyFile), keyPEM, 0o600); err != nil {
		log.Debug("Ensure: writing CA key failed", zap.String("path", filepath.Join(dir, caKeyFile)), zap.Error(err))
		return nil, fmt.Errorf("write CA key: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, caCertFile), ca.CertPEM, 0o644); err != nil {
		log.Debug("Ensure: writing CA certificate failed", zap.String("path", filepath.Join(dir, caCertFile)), zap.Error(err))
		return nil, fmt.Errorf("write CA certificate: %w", err)
	}
	log.Debug("Ensure: CA created and written", zap.String("dir", dir), zap.String("subject", ca.cert.Subject.CommonName),
		zap.Time("not_after", ca.cert.NotAfter), zap.Duration("took", time.Since(start)))
	return ca, nil
}

func newCA() (*CA, []byte, error) {
	log := zap.L()
	log.Debug("newCA: generating CA key and certificate", zap.Duration("validity", caValidity))
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		log.Debug("newCA: generating key failed", zap.Error(err))
		return nil, nil, fmt.Errorf("generate CA key: %w", err)
	}
	serial, err := serialNumber()
	if err != nil {
		log.Debug("newCA: serial number failed", zap.Error(err))
		return nil, nil, err
	}
	now := time.Now().Add(-time.Hour)
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "Haify DRBD replication CA"},
		NotBefore:             now,
		NotAfter:              now.Add(caValidity),
		IsCA:                  true,
		BasicConstraintsValid: true,
		MaxPathLenZero:        true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		log.Debug("newCA: creating certificate failed", zap.Error(err))
		return nil, nil, fmt.Errorf("create CA certificate: %w", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		log.Debug("newCA: marshalling key failed", zap.Error(err))
		return nil, nil, err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	ca, err := parseCA(certPEM, keyPEM)
	log.Debug("newCA: done", zap.String("subject", tmpl.Subject.CommonName), zap.Time("not_after", tmpl.NotAfter), zap.Error(err))
	return ca, keyPEM, err
}

func parseCA(certPEM, keyPEM []byte) (*CA, error) {
	log := zap.L()
	log.Debug("parseCA: start", zap.Int("cert_bytes", len(certPEM)), zap.Bool("has_key", len(keyPEM) > 0))
	cert, err := parseCert(certPEM)
	if err != nil {
		log.Debug("parseCA: certificate does not parse", zap.Error(err))
		return nil, fmt.Errorf("replication CA certificate: %w", err)
	}
	block, _ := pem.Decode(keyPEM)
	if block == nil {
		log.Debug("parseCA: key has no PEM block")
		return nil, fmt.Errorf("replication CA key: no PEM block")
	}
	key, err := x509.ParseECPrivateKey(block.Bytes)
	if err != nil {
		log.Debug("parseCA: key does not parse", zap.Error(err))
		return nil, fmt.Errorf("replication CA key: %w", err)
	}
	if !key.PublicKey.Equal(cert.PublicKey) {
		log.Debug("parseCA: key does not match certificate", zap.String("subject", cert.Subject.CommonName))
		return nil, fmt.Errorf("replication CA key does not belong to its certificate")
	}
	log.Debug("parseCA: loaded", zap.String("subject", cert.Subject.CommonName), zap.Time("not_after", cert.NotAfter))
	return &CA{cert: cert, key: key, CertPEM: certPEM}, nil
}

// Sign issues a certificate for node from its certificate request. The request
// must be signed by the key it carries and name the node as its CN; the
// certificate names the node and its addresses, and serves both ends of a
// connection, since a DRBD peer is the client one time and the server the next.
func (ca *CA) Sign(csrPEM []byte, node string, addrs []string) ([]byte, error) {
	log := zap.L()
	log.Debug("CA.Sign: start", zap.String("node", node), zap.Strings("addrs", addrs), zap.Int("csr_bytes", len(csrPEM)))
	block, _ := pem.Decode(csrPEM)
	if block == nil || block.Type != "CERTIFICATE REQUEST" {
		log.Debug("CA.Sign: no certificate request in PEM", zap.String("node", node), zap.Bool("has_block", block != nil))
		return nil, fmt.Errorf("node %s sent no certificate request", node)
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		log.Debug("CA.Sign: request does not parse", zap.String("node", node), zap.Error(err))
		return nil, fmt.Errorf("node %s: %w", node, err)
	}
	if err := csr.CheckSignature(); err != nil {
		log.Debug("CA.Sign: request signature invalid", zap.String("node", node), zap.Error(err))
		return nil, fmt.Errorf("node %s: certificate request signature: %w", node, err)
	}
	if csr.Subject.CommonName != node {
		log.Debug("CA.Sign: request names another node", zap.String("node", node), zap.String("subject", csr.Subject.CommonName))
		return nil, fmt.Errorf("node %s requested a certificate for %q", node, csr.Subject.CommonName)
	}
	serial, err := serialNumber()
	if err != nil {
		log.Debug("CA.Sign: serial number failed", zap.String("node", node), zap.Error(err))
		return nil, err
	}
	now := time.Now().Add(-time.Hour)
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: node},
		DNSNames:     []string{node},
		NotBefore:    now,
		NotAfter:     now.Add(nodeValidity),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}
	for _, a := range addrs {
		if ip := net.ParseIP(a); ip != nil {
			log.Debug("CA.Sign: adding IP SAN", zap.String("node", node), zap.String("addr", a))
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else if a != "" && a != node {
			log.Debug("CA.Sign: adding DNS SAN", zap.String("node", node), zap.String("addr", a))
			tmpl.DNSNames = append(tmpl.DNSNames, a)
		} else {
			log.Debug("CA.Sign: address empty or the node name, skipped", zap.String("node", node), zap.String("addr", a))
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, csr.PublicKey, ca.key)
	if err != nil {
		log.Debug("CA.Sign: signing failed", zap.String("node", node), zap.Error(err))
		return nil, fmt.Errorf("sign certificate for %s: %w", node, err)
	}
	log.Debug("CA.Sign: signed", zap.String("node", node), zap.Strings("dns_names", tmpl.DNSNames),
		zap.Int("ip_sans", len(tmpl.IPAddresses)), zap.Time("not_after", tmpl.NotAfter))
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), nil
}

// Check reports when a node certificate expires, or why this CA would not
// accept it.
func (ca *CA) Check(certPEM []byte, node string, now time.Time) (time.Time, error) {
	log := zap.L()
	log.Debug("CA.Check: start", zap.String("node", node), zap.Int("cert_bytes", len(certPEM)), zap.Time("now", now))
	cert, err := parseCert(certPEM)
	if err != nil {
		log.Debug("CA.Check: certificate does not parse", zap.String("node", node), zap.Error(err))
		return time.Time{}, err
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca.cert)
	if _, err := cert.Verify(x509.VerifyOptions{
		Roots: pool, CurrentTime: now, DNSName: node,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}); err != nil {
		log.Debug("CA.Check: certificate not accepted", zap.String("node", node), zap.String("subject", cert.Subject.CommonName),
			zap.Time("not_after", cert.NotAfter), zap.Error(err))
		return cert.NotAfter, err
	}
	log.Debug("CA.Check: certificate valid", zap.String("node", node), zap.Time("not_after", cert.NotAfter))
	return cert.NotAfter, nil
}

// Fingerprint is the SHA-256 of the CA certificate, to tell CAs apart.
func (ca *CA) Fingerprint() string {
	sum := sha256.Sum256(ca.cert.Raw)
	return hex.EncodeToString(sum[:])
}

func parseCert(data []byte) (*x509.Certificate, error) {
	block, _ := pem.Decode(data)
	if block == nil || block.Type != "CERTIFICATE" {
		zap.L().Debug("parseCert: no CERTIFICATE PEM block", zap.Int("bytes", len(data)), zap.Bool("has_block", block != nil))
		return nil, fmt.Errorf("no certificate")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	zap.L().Debug("parseCert: parsed", zap.Error(err))
	return cert, err
}

func serialNumber() (*big.Int, error) {
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		zap.L().Debug("serialNumber: random serial failed", zap.Error(err))
		return nil, fmt.Errorf("certificate serial: %w", err)
	}
	return n, nil
}
