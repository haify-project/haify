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

	"go.uber.org/zap"
)

const magic = "HAIFYSHIP1"

// TokenSize is the length of the one-time token.
const TokenSize = 32

// NewToken returns a fresh one-time token, hex encoded.
func NewToken() (string, error) {
	b := make([]byte, TokenSize)
	if _, err := rand.Read(b); err != nil {
		zap.L().Debug("NewToken: reading random bytes failed", zap.Error(err))
		return "", err
	}
	zap.L().Debug("NewToken: token made", zap.Int("bytes", TokenSize))
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
	log := zap.L()
	log.Debug("Listen: start", zap.String("addr", addr), zap.Bool("has_token", token != ""))
	tok, err := hex.DecodeString(token)
	if err != nil || len(tok) != TokenSize {
		log.Debug("Listen: token malformed", zap.Int("token_len", len(token)), zap.Bool("hex_ok", err == nil))
		return nil, errors.New("token must be 64 hex characters")
	}
	cert, fp, err := ephemeralCert()
	if err != nil {
		log.Debug("Listen: making the certificate failed", zap.Error(err))
		return nil, err
	}
	ln, err := tls.Listen("tcp", addr, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS13})
	if err != nil {
		log.Debug("Listen: listening failed", zap.String("addr", addr), zap.Error(err))
		return nil, err
	}
	log.Debug("Listen: listening", zap.Stringer("addr", ln.Addr()), zap.String("fingerprint", fp))
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
	log := zap.L()
	log.Debug("Receiver.Receive: waiting for the sender", zap.Stringer("addr", r.ln.Addr()), zap.Bool("has_sync", sync != nil))
	for {
		conn, err := r.ln.Accept()
		if err != nil {
			log.Debug("Receiver.Receive: accept failed", zap.Error(err))
			return 0, "", err
		}
		log.Debug("Receiver.Receive: connection accepted", zap.Stringer("remote", conn.RemoteAddr()))
		start := time.Now()
		n, sum, err := r.serve(conn, dst, sync)
		if errors.Is(err, errBadToken) {
			log.Debug("Receiver.Receive: wrong token or header, dropping and waiting again", zap.Stringer("remote", conn.RemoteAddr()))
			_ = conn.Close()
			continue
		}
		_ = conn.Close()
		log.Debug("Receiver.Receive: transfer finished", zap.Stringer("remote", conn.RemoteAddr()), zap.Uint64("bytes", n),
			zap.String("sha256", sum), zap.Error(err), zap.Duration("took", time.Since(start)))
		return n, sum, err
	}
}

var errBadToken = errors.New("wrong token")

func (r *Receiver) serve(conn net.Conn, dst io.Writer, sync func() error) (uint64, string, error) {
	_ = conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	log := zap.L()
	hdr := make([]byte, len(magic)+TokenSize+8)
	if _, err := io.ReadFull(conn, hdr); err != nil {
		log.Debug("Receiver.serve: header read failed", zap.Stringer("remote", conn.RemoteAddr()), zap.Error(err))
		return 0, "", errBadToken
	}
	if string(hdr[:len(magic)]) != magic || subtle.ConstantTimeCompare(hdr[len(magic):len(magic)+TokenSize], r.token) != 1 {
		log.Debug("Receiver.serve: bad magic or token", zap.Stringer("remote", conn.RemoteAddr()),
			zap.Bool("magic_ok", string(hdr[:len(magic)]) == magic))
		return 0, "", errBadToken
	}
	size := binary.BigEndian.Uint64(hdr[len(magic)+TokenSize:])
	log.Debug("Receiver.serve: header accepted, receiving", zap.Stringer("remote", conn.RemoteAddr()), zap.Uint64("size_bytes", size))
	_ = conn.SetReadDeadline(time.Time{})
	h := sha256.New()
	start := time.Now()
	n, err := io.CopyN(io.MultiWriter(dst, h), bufio.NewReaderSize(conn, 4<<20), int64(size))
	log.Debug("Receiver.serve: copy returned", zap.Int64("bytes", n), zap.Uint64("size_bytes", size),
		zap.Error(err), zap.Duration("took", time.Since(start)))
	if err != nil {
		return uint64(n), "", fmt.Errorf("received %d of %d bytes: %w", n, size, err)
	}
	if sync != nil {
		start = time.Now()
		err := sync()
		log.Debug("Receiver.serve: device flushed", zap.Error(err), zap.Duration("took", time.Since(start)))
		if err != nil {
			return uint64(n), "", fmt.Errorf("flush the device: %w", err)
		}
	} else {
		log.Debug("Receiver.serve: no sync function, skipping flush")
	}
	sum := h.Sum(nil)
	if _, err := conn.Write(sum); err != nil {
		log.Debug("Receiver.serve: sending checksum failed", zap.Error(err))
		return uint64(n), "", fmt.Errorf("send the checksum: %w", err)
	}
	sumHex := hex.EncodeToString(sum)
	log.Debug("Receiver.serve: checksum sent", zap.Int64("bytes", n), zap.String("sha256", sumHex))
	return uint64(n), sumHex, nil
}

// Send streams size bytes of src to the receiver at addr, whose certificate
// must have fingerprint, and returns their SHA-256 once the receiver confirms
// it wrote the same bytes. progress, when set, is called with the bytes sent.
func Send(addr, fingerprint, token string, src io.Reader, size uint64, progress func(uint64)) (string, error) {
	log := zap.L()
	log.Debug("Send: start", zap.String("addr", addr), zap.String("fingerprint", fingerprint),
		zap.Bool("has_token", token != ""), zap.Uint64("size_bytes", size), zap.Bool("has_progress", progress != nil))
	tok, err := hex.DecodeString(token)
	if err != nil || len(tok) != TokenSize {
		log.Debug("Send: token malformed", zap.Int("token_len", len(token)), zap.Bool("hex_ok", err == nil))
		return "", errors.New("token must be 64 hex characters")
	}
	want, err := hex.DecodeString(fingerprint)
	if err != nil {
		log.Debug("Send: fingerprint not hex", zap.String("fingerprint", fingerprint), zap.Error(err))
		return "", errors.New("fingerprint must be hex")
	}
	start := time.Now()
	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 30 * time.Second}, "tcp", addr, &tls.Config{
		MinVersion: tls.VersionTLS13,
		// The certificate is made for this transfer; trust is the pinned
		// fingerprint, which came from the receiving cluster's controller.
		InsecureSkipVerify: true, //nolint:gosec // verified by VerifyPeerCertificate
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			if len(raw) == 0 {
				log.Debug("Send: receiver presented no certificate", zap.String("addr", addr))
				return errors.New("no certificate")
			}
			got := sha256.Sum256(raw[0])
			if !bytes.Equal(got[:], want) {
				log.Debug("Send: receiver certificate does not match the pinned fingerprint", zap.String("addr", addr),
					zap.String("fingerprint", fingerprint), zap.String("got", hex.EncodeToString(got[:])))
				return errors.New("the receiver's certificate does not match the fingerprint")
			}
			log.Debug("Send: receiver certificate matches the pinned fingerprint", zap.String("addr", addr))
			return nil
		},
	})
	log.Debug("Send: dial returned", zap.String("addr", addr), zap.Error(err), zap.Duration("took", time.Since(start)))
	if err != nil {
		return "", err
	}
	defer func() { _ = conn.Close() }()

	hdr := make([]byte, 0, len(magic)+TokenSize+8)
	hdr = append(hdr, magic...)
	hdr = append(hdr, tok...)
	hdr = binary.BigEndian.AppendUint64(hdr, size)
	if _, err := conn.Write(hdr); err != nil {
		log.Debug("Send: writing header failed", zap.String("addr", addr), zap.Error(err))
		return "", err
	}
	h := sha256.New()
	w := io.MultiWriter(conn, h)
	if progress != nil {
		w = io.MultiWriter(w, progressWriter(progress))
	}
	start = time.Now()
	n, err := io.CopyN(w, src, int64(size))
	log.Debug("Send: copy returned", zap.String("addr", addr), zap.Int64("bytes", n), zap.Uint64("size_bytes", size),
		zap.Error(err), zap.Duration("took", time.Since(start)))
	if err != nil {
		return "", fmt.Errorf("sent %d of %d bytes: %w", n, size, err)
	}
	theirs := make([]byte, sha256.Size)
	if _, err := io.ReadFull(conn, theirs); err != nil {
		log.Debug("Send: no checksum from the receiver", zap.String("addr", addr), zap.Error(err))
		return "", fmt.Errorf("no checksum from the receiver: %w", err)
	}
	ours := h.Sum(nil)
	if !bytes.Equal(ours, theirs) {
		log.Debug("Send: checksum mismatch", zap.String("addr", addr), zap.String("sent", hex.EncodeToString(ours)),
			zap.String("received", hex.EncodeToString(theirs)))
		return "", fmt.Errorf("checksum mismatch: sent %x, received %x", ours, theirs)
	}
	oursHex := hex.EncodeToString(ours)
	log.Debug("Send: receiver confirmed the checksum", zap.String("addr", addr), zap.String("sha256", oursHex))
	return oursHex, nil
}

type progressWriter func(uint64)

func (p progressWriter) Write(b []byte) (int, error) {
	p(uint64(len(b)))
	return len(b), nil
}

func ephemeralCert() (tls.Certificate, string, error) {
	log := zap.L()
	log.Debug("ephemeralCert: generating a one-transfer certificate")
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		log.Debug("ephemeralCert: generating key failed", zap.Error(err))
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
		log.Debug("ephemeralCert: creating certificate failed", zap.Error(err))
		return tls.Certificate{}, "", err
	}
	sum := sha256.Sum256(der)
	fp := hex.EncodeToString(sum[:])
	log.Debug("ephemeralCert: made", zap.String("subject", tmpl.Subject.CommonName), zap.Time("not_after", tmpl.NotAfter),
		zap.String("fingerprint", fp))
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, fp, nil
}
