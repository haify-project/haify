package ship

import (
	"bytes"
	"crypto/rand"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestShipRoundTrip(t *testing.T) {
	token, err := NewToken()
	require.NoError(t, err)
	r, err := Listen("127.0.0.1:0", token)
	require.NoError(t, err)
	defer func() { _ = r.Close() }()

	data := make([]byte, 5<<20+123)
	_, _ = rand.Read(data)
	var got bytes.Buffer
	synced := false
	done := make(chan error, 1)
	var recvSum string
	go func() {
		_, sum, err := r.Receive(&got, func() error { synced = true; return nil })
		recvSum = sum
		done <- err
	}()

	// A connection with the wrong token is dropped; the transfer still runs.
	bad, _ := NewToken()
	_, err = Send(r.Addr().String(), r.Fingerprint, bad, bytes.NewReader(data), uint64(len(data)), nil)
	require.Error(t, err)

	var sent uint64
	sum, err := Send(r.Addr().String(), r.Fingerprint, token, bytes.NewReader(data), uint64(len(data)), func(n uint64) { sent += n })
	require.NoError(t, err)
	require.NoError(t, <-done)
	assert.Equal(t, sum, recvSum)
	assert.True(t, bytes.Equal(data, got.Bytes()))
	assert.True(t, synced, "the device is flushed before the checksum is sent")
	assert.Equal(t, uint64(len(data)), sent)
}

func TestShipRefusesAnotherCertificate(t *testing.T) {
	token, _ := NewToken()
	r, err := Listen("127.0.0.1:0", token)
	require.NoError(t, err)
	defer func() { _ = r.Close() }()
	go func() { _, _, _ = r.Receive(io.Discard, nil) }()
	other, _ := NewToken() // any other 32 bytes
	_, err = Send(r.Addr().String(), other, token, bytes.NewReader([]byte("x")), 1, nil)
	assert.ErrorContains(t, err, "does not match the fingerprint")
}

func TestShipShortSourceFails(t *testing.T) {
	token, _ := NewToken()
	r, err := Listen("127.0.0.1:0", token)
	require.NoError(t, err)
	defer func() { _ = r.Close() }()
	errc := make(chan error, 1)
	go func() { _, _, err := r.Receive(io.Discard, nil); errc <- err }()
	_, err = Send(r.Addr().String(), r.Fingerprint, token, bytes.NewReader([]byte("short")), 100, nil)
	assert.Error(t, err)
	assert.Error(t, <-errc, "the receiver does not report success for a partial copy")
}
