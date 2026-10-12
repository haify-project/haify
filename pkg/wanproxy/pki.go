package wanproxy

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"time"

	"go.uber.org/zap"
)

// PKI is the shared mTLS material distributed to BOTH sites. It is the
// shared-CA MVP: one CA + one leaf, reused for every WAN resource. (Per-resource
// PKI is a later phase.) The leaf carries SAN "haify-proxy" with server+client
// EKU so either proxy can act as the TLS server or client, and each proxy pins
// its peer to that SAN via the [tls] peer_name field.
type PKI struct {
	// CAPEM is the CA certificate (self-signed). CertPEM/KeyPEM are the leaf
	// certificate and its private key. Only these three are persisted; the CA
	// private key is discarded after signing the leaf, since the shared-CA MVP
	// never issues further certificates.
	CAPEM   []byte
	CertPEM []byte
	KeyPEM  []byte
}

// PKI cache filenames under PKIDir.
const (
	caFileName   = "ca.pem"
	certFileName = "cert.pem"
	keyFileName  = "key.pem"
)

// pkiValidity is how long the generated CA and leaf are valid. Ten years keeps
// the shared-CA MVP maintenance-free; rotation is a later concern.
const pkiValidity = 10 * 365 * 24 * time.Hour

// EnsurePKI loads the shared CA+leaf from dir, generating and caching it on the
// first call. It is idempotent: once the three PEM files exist and parse, every
// subsequent call returns the exact same bytes (generate once, reuse
// thereafter). The same material is what Provision distributes to both sites.
//
// dir is created with 0700; the private key is written 0600 and the certs 0644.
func EnsurePKI(dir string) (*PKI, error) {
	log := zap.L()
	log.Debug("EnsurePKI: start", zap.String("dir", dir))
	caPath := filepath.Join(dir, caFileName)
	certPath := filepath.Join(dir, certFileName)
	keyPath := filepath.Join(dir, keyFileName)

	// Fast path: reuse a previously generated, valid cache.
	if pki, ok := loadPKI(caPath, certPath, keyPath); ok {
		log.Debug("EnsurePKI: reusing cached PKI", zap.String("dir", dir))
		return pki, nil
	}

	log.Debug("EnsurePKI: no usable cache, generating", zap.String("dir", dir))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		log.Debug("EnsurePKI: creating dir failed", zap.String("dir", dir), zap.Error(err))
		return nil, fmt.Errorf("create PKI dir %q: %w", dir, err)
	}

	pki, err := generatePKI()
	if err != nil {
		log.Debug("EnsurePKI: generating failed", zap.Error(err))
		return nil, err
	}

	// Persist atomically-ish: certs then key. A partial cache (e.g. only ca.pem)
	// is rejected by loadPKI, so the next call regenerates cleanly.
	start := time.Now()
	if err := os.WriteFile(caPath, pki.CAPEM, 0o644); err != nil {
		log.Debug("EnsurePKI: writing CA failed", zap.String("path", caPath), zap.Error(err))
		return nil, fmt.Errorf("write CA: %w", err)
	}
	if err := os.WriteFile(certPath, pki.CertPEM, 0o644); err != nil {
		log.Debug("EnsurePKI: writing cert failed", zap.String("path", certPath), zap.Error(err))
		return nil, fmt.Errorf("write cert: %w", err)
	}
	if err := os.WriteFile(keyPath, pki.KeyPEM, 0o600); err != nil {
		log.Debug("EnsurePKI: writing key failed", zap.String("path", keyPath), zap.Error(err))
		return nil, fmt.Errorf("write key: %w", err)
	}
	log.Debug("EnsurePKI: PKI written", zap.String("dir", dir), zap.Duration("took", time.Since(start)))
	return pki, nil
}

// loadPKI returns the cached PKI when all three files exist and the leaf parses
// and is validly signed by the cached CA. A missing or malformed cache returns
// ok=false so EnsurePKI regenerates.
func loadPKI(caPath, certPath, keyPath string) (*PKI, bool) {
	log := zap.L()
	caPEM, err := os.ReadFile(caPath)
	if err != nil {
		log.Debug("loadPKI: CA not readable", zap.String("path", caPath), zap.Error(err))
		return nil, false
	}
	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		log.Debug("loadPKI: cert not readable", zap.String("path", certPath), zap.Error(err))
		return nil, false
	}
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		log.Debug("loadPKI: key not readable", zap.String("path", keyPath), zap.Error(err))
		return nil, false
	}
	caCert, err := parseCertPEM(caPEM)
	if err != nil {
		log.Debug("loadPKI: CA does not parse", zap.String("path", caPath), zap.Error(err))
		return nil, false
	}
	leafCert, err := parseCertPEM(certPEM)
	if err != nil {
		log.Debug("loadPKI: cert does not parse", zap.String("path", certPath), zap.Error(err))
		return nil, false
	}
	// The leaf must still be signed by the cached CA — guards a corrupted or
	// mismatched cache.
	if err := leafCert.CheckSignatureFrom(caCert); err != nil {
		log.Debug("loadPKI: cert not signed by the cached CA", zap.String("subject", leafCert.Subject.CommonName), zap.Error(err))
		return nil, false
	}
	log.Debug("loadPKI: cache valid", zap.String("ca_subject", caCert.Subject.CommonName),
		zap.String("subject", leafCert.Subject.CommonName), zap.Time("not_after", leafCert.NotAfter))
	return &PKI{CAPEM: caPEM, CertPEM: certPEM, KeyPEM: keyPEM}, true
}

// generatePKI creates a fresh CA + leaf. ed25519 keeps it dependency-free and
// small, with no curve parameters to configure.
func generatePKI() (*PKI, error) {
	log := zap.L()
	log.Debug("generatePKI: generating CA and leaf", zap.Duration("validity", pkiValidity))
	// --- CA ---
	caPub, caPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		log.Debug("generatePKI: CA key failed", zap.Error(err))
		return nil, fmt.Errorf("generate CA key: %w", err)
	}
	caSerial, err := randomSerial()
	if err != nil {
		log.Debug("generatePKI: CA serial failed", zap.Error(err))
		return nil, err
	}
	now := time.Now().Add(-time.Minute) // small backdate for clock skew
	caTemplate := &x509.Certificate{
		SerialNumber:          caSerial,
		Subject:               pkix.Name{CommonName: "haify-proxy-ca"},
		NotBefore:             now,
		NotAfter:              now.Add(pkiValidity),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, caPub, caPriv)
	if err != nil {
		log.Debug("generatePKI: creating CA cert failed", zap.Error(err))
		return nil, fmt.Errorf("create CA cert: %w", err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		log.Debug("generatePKI: parsing CA cert failed", zap.Error(err))
		return nil, fmt.Errorf("parse CA cert: %w", err)
	}

	// --- Leaf ---
	leafPub, leafPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		log.Debug("generatePKI: leaf key failed", zap.Error(err))
		return nil, fmt.Errorf("generate leaf key: %w", err)
	}
	leafSerial, err := randomSerial()
	if err != nil {
		log.Debug("generatePKI: leaf serial failed", zap.Error(err))
		return nil, err
	}
	leafTemplate := &x509.Certificate{
		SerialNumber: leafSerial,
		Subject:      pkix.Name{CommonName: CertSAN},
		DNSNames:     []string{CertSAN},
		NotBefore:    now,
		NotAfter:     now.Add(pkiValidity),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		// server + client so either proxy can be the TLS server or the client.
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTemplate, caCert, leafPub, caPriv)
	if err != nil {
		log.Debug("generatePKI: creating leaf cert failed", zap.Error(err))
		return nil, fmt.Errorf("create leaf cert: %w", err)
	}

	keyDER, err := x509.MarshalPKCS8PrivateKey(leafPriv)
	if err != nil {
		log.Debug("generatePKI: marshalling leaf key failed", zap.Error(err))
		return nil, fmt.Errorf("marshal leaf key: %w", err)
	}
	log.Debug("generatePKI: generated", zap.String("ca_subject", caTemplate.Subject.CommonName),
		zap.String("subject", leafTemplate.Subject.CommonName), zap.Time("not_after", leafTemplate.NotAfter))

	return &PKI{
		CAPEM:   pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}),
		CertPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER}),
		KeyPEM:  pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}),
	}, nil
}

// parseCertPEM decodes the first CERTIFICATE block from PEM bytes.
func parseCertPEM(data []byte) (*x509.Certificate, error) {
	block, _ := pem.Decode(data)
	if block == nil || block.Type != "CERTIFICATE" {
		zap.L().Debug("parseCertPEM: no CERTIFICATE PEM block", zap.Int("bytes", len(data)), zap.Bool("has_block", block != nil))
		return nil, fmt.Errorf("no CERTIFICATE PEM block")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	zap.L().Debug("parseCertPEM: parsed", zap.Error(err))
	return cert, err
}

// randomSerial returns a cryptographically-random 128-bit certificate serial.
func randomSerial() (*big.Int, error) {
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, limit)
	if err != nil {
		zap.L().Debug("randomSerial: random serial failed", zap.Error(err))
		return nil, fmt.Errorf("generate serial: %w", err)
	}
	return serial, nil
}
