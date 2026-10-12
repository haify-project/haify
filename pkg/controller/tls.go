package controller

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"math/big"
	"os"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc/credentials"

	"github.com/haify-project/haify/pkg/config"
)

// Traces in this file name certificate and key files by path and describe a
// certificate by subject and expiry; key material and PEM bodies are never
// logged.

// tlsSetup is everything transport security needs once [tls] is enabled: the
// credentials the gRPC server listens with, and the credentials the in-process
// REST gateway dials back with.
//
// The two are not the same, and that asymmetry is the whole reason this type
// exists. See newTLSSetup for why the loopback hop cannot simply reuse a
// normal client configuration.
type tlsSetup struct {
	serverCreds   credentials.TransportCredentials
	loopbackCreds credentials.TransportCredentials
	// mutual reports that client certificates are required and verified.
	mutual bool
	// restServer is what the REST gateway listens with when [tls] rest is set,
	// and restLoopback what the web UI's proxy dials it with; both nil
	// otherwise.
	restServer   *tls.Config
	restLoopback *tls.Config
}

// loopbackIdentityValidity is how long the ephemeral client certificate the
// REST gateway uses is good for. It lives only in this process's memory and
// dies with it, so the number only has to outlast one controller run; a year
// keeps a long-lived controller from failing its own health checks.
const loopbackIdentityValidity = 365 * 24 * time.Hour

// newTLSSetup builds the transport credentials for a [tls] section. It returns
// (nil, nil) when TLS is disabled, which every caller must treat as "stay
// plaintext" rather than as an error.
//
// The configuration is validated at load time (config.TLSConfig.Validate), so
// a failure here means the certificate changed underneath a running
// controller, not that an operator typed something wrong.
func newTLSSetup(cfg config.TLSConfig) (*tlsSetup, error) {
	log := zap.L()
	if !cfg.Enabled {
		log.Debug("newTLSSetup: TLS disabled, staying plaintext")
		return nil, nil
	}
	log.Debug("newTLSSetup: loading certificate", zap.String("cert_file", cfg.CertFile), zap.String("key_file", cfg.KeyFile),
		zap.String("client_ca_file", cfg.ClientCAFile), zap.Bool("rest", cfg.REST))

	cert, err := tls.LoadX509KeyPair(cfg.CertFile, cfg.KeyFile)
	if err != nil {
		log.Debug("newTLSSetup: key pair not loaded", zap.String("cert_file", cfg.CertFile), zap.Error(err))
		return nil, fmt.Errorf("load cert_file %q with key_file %q: %w", cfg.CertFile, cfg.KeyFile, err)
	}
	if len(cert.Certificate) == 0 {
		log.Debug("newTLSSetup: no certificate in cert_file", zap.String("cert_file", cfg.CertFile))
		return nil, fmt.Errorf("cert_file %q contains no certificate", cfg.CertFile)
	}
	if leaf := cert.Leaf; leaf != nil {
		log.Debug("newTLSSetup: certificate loaded", zap.String("subject", leaf.Subject.String()),
			zap.Strings("dns_names", leaf.DNSNames), zap.Int("ip_sans", len(leaf.IPAddresses)), zap.Time("not_after", leaf.NotAfter))
	} else {
		log.Debug("newTLSSetup: certificate loaded", zap.Int("chain", len(cert.Certificate)))
	}

	serverConf := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}

	// The REST gateway — and therefore the entire web UI — reaches the API by
	// dialling this same process at 127.0.0.1. That hop cannot be verified the
	// ordinary way: the operator's certificate is issued for the cluster's
	// service name or VIP, so its SANs will not cover the loopback literal,
	// and demanding that they do would make TLS unusable for everyone whose
	// certificate comes from a corporate or public CA. Skipping verification
	// outright is not acceptable either.
	//
	// Instead the hop pins the exact certificate that was just loaded from
	// disk: the peer is accepted only when it presents byte-for-byte the leaf
	// this process is serving. That is strictly stronger than CA verification
	// (no other certificate from the same CA is accepted), needs no extra
	// configuration, and is immune to whatever SANs the operator chose.
	loopbackConf := &tls.Config{
		MinVersion: tls.VersionTLS12,
		// Disables the name/chain check only; VerifyPeerCertificate below is
		// the real check and it is unconditional.
		InsecureSkipVerify:    true, // #nosec G402 -- replaced by exact-leaf pinning
		VerifyPeerCertificate: pinnedLeafVerifier(cert.Certificate[0]),
	}

	if cfg.ClientCAFile != "" {
		log.Debug("newTLSSetup: mutual TLS, loading client CAs", zap.String("client_ca_file", cfg.ClientCAFile))
		pool, err := loadCertPool(cfg.ClientCAFile)
		if err != nil {
			log.Debug("newTLSSetup: client CAs not loaded", zap.Error(err))
			return nil, err
		}

		// Mutual TLS would otherwise lock out the loopback hop along with
		// everyone else: the REST gateway has no client certificate, and
		// issuing it one from the operator's client CA is not something the
		// controller can do — it does not hold that CA's key.
		//
		// So the controller mints its own throwaway CA and a single client
		// leaf from it, in memory, at every start, and trusts that CA in
		// addition to the operator's. Nothing is written to disk and the key
		// never leaves this process, so unlike an address-based exemption for
		// 127.0.0.1 this does not hand every other local process a way around
		// client-certificate verification.
		loopbackCert, loopbackCA, err := newLoopbackIdentity()
		if err != nil {
			log.Debug("newTLSSetup: loopback identity not minted", zap.Error(err))
			return nil, err
		}
		pool.AddCert(loopbackCA)

		serverConf.ClientCAs = pool
		serverConf.ClientAuth = tls.RequireAndVerifyClientCert
		loopbackConf.Certificates = []tls.Certificate{loopbackCert}
		log.Debug("newTLSSetup: client certificates required, loopback identity trusted")
	}

	setup := &tlsSetup{
		serverCreds:   credentials.NewTLS(serverConf),
		loopbackCreds: credentials.NewTLS(loopbackConf),
		mutual:        cfg.ClientCAFile != "",
	}
	if cfg.REST {
		log.Debug("newTLSSetup: REST gateway served over TLS too")
		// No client certificates here even under mutual TLS: REST callers
		// authenticate with a bearer token, a browser behind the web UI has
		// no certificate to present, and the REST gateway's own hop to gRPC
		// keeps its loopback identity either way.
		setup.restServer = &tls.Config{
			Certificates: []tls.Certificate{cert},
			MinVersion:   tls.VersionTLS12,
		}
		// The web UI proxies to REST over 127.0.0.1: the same name problem as
		// the gRPC hop above, solved the same way.
		setup.restLoopback = &tls.Config{
			MinVersion:            tls.VersionTLS12,
			InsecureSkipVerify:    true, // #nosec G402 -- replaced by exact-leaf pinning
			VerifyPeerCertificate: pinnedLeafVerifier(cert.Certificate[0]),
		}
	}
	log.Debug("newTLSSetup: ready", zap.Bool("mutual", setup.mutual), zap.Bool("rest", cfg.REST))
	return setup, nil
}

// pinnedLeafVerifier accepts a peer only when its leaf is byte-identical to
// want. Comparing DER rather than a fingerprint keeps the check trivially
// auditable and free of hash-agility questions.
func pinnedLeafVerifier(want []byte) func([][]byte, [][]*x509.Certificate) error {
	return func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
		if len(rawCerts) == 0 {
			zap.L().Debug("pinnedLeafVerifier: peer presented no certificate")
			return errors.New("loopback peer presented no certificate")
		}
		if !bytes.Equal(rawCerts[0], want) {
			zap.L().Debug("pinnedLeafVerifier: peer certificate does not match the pinned leaf", zap.Int("chain", len(rawCerts)))
			return errors.New("loopback peer is not this controller: certificate does not match the configured tls.cert_file")
		}
		zap.L().Debug("pinnedLeafVerifier: peer is this controller")
		return nil
	}
}

// loadCertPool reads a PEM bundle into a fresh pool. An empty pool is an error
// rather than a permissive default: as ClientCAs it would reject every client,
// turning a typo in client_ca_file into a cluster-wide outage that looks like
// a certificate problem on the caller's side.
func loadCertPool(path string) (*x509.CertPool, error) {
	log := zap.L()
	pemBytes, err := os.ReadFile(path)
	if err != nil {
		log.Debug("loadCertPool: file not read", zap.String("path", path), zap.Error(err))
		return nil, fmt.Errorf("read client_ca_file %q: %w", path, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pemBytes) {
		log.Debug("loadCertPool: no PEM certificate in file", zap.String("path", path), zap.Int("bytes", len(pemBytes)))
		return nil, fmt.Errorf("client_ca_file %q contains no PEM certificate", path)
	}
	log.Debug("loadCertPool: loaded", zap.String("path", path), zap.Int("bytes", len(pemBytes)))
	return pool, nil
}

// newLoopbackIdentity mints an in-memory CA and one client leaf signed by it,
// for the REST gateway's hop to this process's own gRPC server under mutual
// TLS. It returns the leaf (with its key) and the CA certificate to trust.
//
// Shaped after pkg/wanproxy.EnsurePKI — ed25519, self-signed CA, single leaf —
// minus the on-disk cache, which would only create a credential to steal.
func newLoopbackIdentity() (tls.Certificate, *x509.Certificate, error) {
	log := zap.L()
	log.Debug("newLoopbackIdentity: minting in-memory CA and client leaf", zap.Duration("validity", loopbackIdentityValidity))
	caPub, caPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		log.Debug("newLoopbackIdentity: CA key not generated", zap.Error(err))
		return tls.Certificate{}, nil, fmt.Errorf("generate loopback CA key: %w", err)
	}
	now := time.Now().Add(-time.Minute) // small backdate for clock skew
	caSerial, err := randomCertSerial()
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	caTemplate := &x509.Certificate{
		SerialNumber:          caSerial,
		Subject:               pkix.Name{CommonName: "haify-controller-loopback-ca"},
		NotBefore:             now,
		NotAfter:              now.Add(loopbackIdentityValidity),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, caPub, caPriv)
	if err != nil {
		log.Debug("newLoopbackIdentity: CA certificate not created", zap.Error(err))
		return tls.Certificate{}, nil, fmt.Errorf("create loopback CA cert: %w", err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		log.Debug("newLoopbackIdentity: CA certificate not parsed", zap.Error(err))
		return tls.Certificate{}, nil, fmt.Errorf("parse loopback CA cert: %w", err)
	}

	leafPub, leafPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		log.Debug("newLoopbackIdentity: leaf key not generated", zap.Error(err))
		return tls.Certificate{}, nil, fmt.Errorf("generate loopback key: %w", err)
	}
	leafSerial, err := randomCertSerial()
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	leafTemplate := &x509.Certificate{
		SerialNumber: leafSerial,
		Subject:      pkix.Name{CommonName: "haify-controller-rest-gateway"},
		NotBefore:    now,
		NotAfter:     now.Add(loopbackIdentityValidity),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTemplate, caCert, leafPub, caPriv)
	if err != nil {
		log.Debug("newLoopbackIdentity: leaf certificate not created", zap.Error(err))
		return tls.Certificate{}, nil, fmt.Errorf("create loopback cert: %w", err)
	}

	log.Debug("newLoopbackIdentity: minted", zap.String("ca", caTemplate.Subject.CommonName),
		zap.String("leaf", leafTemplate.Subject.CommonName), zap.Time("not_after", leafTemplate.NotAfter))
	return tls.Certificate{
		Certificate: [][]byte{leafDER},
		PrivateKey:  leafPriv,
	}, caCert, nil
}

// randomCertSerial returns a cryptographically-random 128-bit serial.
func randomCertSerial() (*big.Int, error) {
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, limit)
	if err != nil {
		zap.L().Debug("randomCertSerial: no randomness", zap.Error(err))
		return nil, fmt.Errorf("generate certificate serial: %w", err)
	}
	return serial, nil
}
