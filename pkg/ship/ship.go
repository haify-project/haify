// Package ship streams one block device from a node of one Haify cluster to a
// node of another, for `haify resource ship`.
//
// The receiver listens on TLS with a certificate made for this one transfer
// and announces its fingerprint; the controllers pass that fingerprint and a
// one-time token between the clusters over their own APIs. The sender pins the
// fingerprint, proves the token, says how many bytes follow, and sends them.
// The receiver writes exactly that many bytes to its device, flushes it, and
// answers with the SHA-256 of what it wrote, which the sender compares with
// what it read. One connection, one device, then both sides exit.
package ship

import (
	"bufio"
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"time"
)

const magic = "HAIFYSHIP1"

// TokenSize is the length of the one-time token.
const TokenSize = 32

// NewToken returns a fresh one-time token, hex encoded.
func NewToken() (string, error) {
	b := make([]byte, TokenSize)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// Receiver accepts one transfer.
type Receiver struct {
	ln          net.Listener
	Fingerprint string // hex SHA-256 of the certificate, for the sender to pin
	token       []byte
}

// Listen starts a receiver on addr for the hex token.
func Listen(addr, token string) (*Receiver, error) {
	tok, err := hex.DecodeString(token)
	if err != nil || len(tok) != TokenSize {
		return nil, errors.New("token must be 64 hex characters")
	}
	cert, fp, err := ephemeralCert()
	if err != nil {
		return nil, err
	}
	ln, err := tls.Listen("tcp", addr, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS13})
	if err != nil {
		return nil, err
	}
	return &Receiver{ln: ln, Fingerprint: fp, token: tok}, nil
}

// Addr is where the receiver listens.
func (r *Receiver) Addr() net.Addr { return r.ln.Addr() }

// Close stops listening.
func (r *Receiver) Close() error { return r.ln.Close() }

// Receive accepts connections until one presents the token, writes what it
// sends to dst and returns the byte count and its SHA-256. A connection with
// the wrong token is dropped and the receiver keeps waiting, so a stray
// connection cannot end the transfer.
func (r *Receiver) Receive(dst io.Writer, sync func() error) (uint64, string, error) {
	for {
		conn, err := r.ln.Accept()
		if err != nil {
			return 0, "", err
		}
		n, sum, err := r.serve(conn, dst, sync)
		if errors.Is(err, errBadToken) {
			_ = conn.Close()
			continue
		}
		_ = conn.Close()
		return n, sum, err
	}
}

var errBadToken = errors.New("wrong token")

func (r *Receiver) serve(conn net.Conn, dst io.Writer, sync func() error) (uint64, string, error) {
	_ = conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	hdr := make([]byte, len(magic)+TokenSize+8)
	if _, err := io.ReadFull(conn, hdr); err != nil {
		return 0, "", errBadToken
	}
	if string(hdr[:len(magic)]) != magic || subtle.ConstantTimeCompare(hdr[len(magic):len(magic)+TokenSize], r.token) != 1 {
		return 0, "", errBadToken
	}
	size := binary.BigEndian.Uint64(hdr[len(magic)+TokenSize:])
	_ = conn.SetReadDeadline(time.Time{})
	h := sha256.New()
	n, err := io.CopyN(io.MultiWriter(dst, h), bufio.NewReaderSize(conn, 4<<20), int64(size))
	if err != nil {
		return uint64(n), "", fmt.Errorf("received %d of %d bytes: %w", n, size, err)
	}
	if sync != nil {
		if err := sync(); err != nil {
			return uint64(n), "", fmt.Errorf("flush the device: %w", err)
		}
	}
	sum := h.Sum(nil)
	if _, err := conn.Write(sum); err != nil {
		return uint64(n), "", fmt.Errorf("send the checksum: %w", err)
	}
	return uint64(n), hex.EncodeToString(sum), nil
}

// Send streams size bytes of src to the receiver at addr, whose certificate
// must have fingerprint, and returns their SHA-256 once the receiver confirms
// it wrote the same bytes. progress, when set, is called with the bytes sent.
func Send(addr, fingerprint, token string, src io.Reader, size uint64, progress func(uint64)) (string, error) {
	tok, err := hex.DecodeString(token)
	if err != nil || len(tok) != TokenSize {
		return "", errors.New("token must be 64 hex characters")
	}
	want, err := hex.DecodeString(fingerprint)
	if err != nil {
		return "", errors.New("fingerprint must be hex")
	}
	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 30 * time.Second}, "tcp", addr, &tls.Config{
		MinVersion: tls.VersionTLS13,
		// The certificate is made for this transfer; trust is the pinned
		// fingerprint, which came from the receiving cluster's controller.
		InsecureSkipVerify: true, //nolint:gosec // verified by VerifyPeerCertificate
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			if len(raw) == 0 {
				return errors.New("no certificate")
			}
			got := sha256.Sum256(raw[0])
			if !bytes.Equal(got[:], want) {
				return errors.New("the receiver's certificate does not match the fingerprint")
			}
			return nil
		},
	})
	if err != nil {
		return "", err
	}
	defer func() { _ = conn.Close() }()

	hdr := make([]byte, 0, len(magic)+TokenSize+8)
	hdr = append(hdr, magic...)
	hdr = append(hdr, tok...)
	hdr = binary.BigEndian.AppendUint64(hdr, size)
	if _, err := conn.Write(hdr); err != nil {
		return "", err
	}
	h := sha256.New()
	w := io.MultiWriter(conn, h)
	if progress != nil {
		w = io.MultiWriter(w, progressWriter(progress))
	}
	if n, err := io.CopyN(w, src, int64(size)); err != nil {
		return "", fmt.Errorf("sent %d of %d bytes: %w", n, size, err)
	}
	theirs := make([]byte, sha256.Size)
	if _, err := io.ReadFull(conn, theirs); err != nil {
		return "", fmt.Errorf("no checksum from the receiver: %w", err)
	}
	ours := h.Sum(nil)
	if !bytes.Equal(ours, theirs) {
		return "", fmt.Errorf("checksum mismatch: sent %x, received %x", ours, theirs)
	}
	return hex.EncodeToString(ours), nil
}

type progressWriter func(uint64)

func (p progressWriter) Write(b []byte) (int, error) {
	p(uint64(len(b)))
	return len(b), nil
}

func ephemeralCert() (tls.Certificate, string, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "haify-ship"},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(48 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	sum := sha256.Sum256(der)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, hex.EncodeToString(sum[:]), nil
}
