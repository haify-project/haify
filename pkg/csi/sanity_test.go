package csi_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/haify-project/haify/pkg/csi"
	"github.com/kubernetes-csi/csi-test/v5/pkg/sanity"
	"go.uber.org/zap"
)

// TestSanity runs the CSI sanity suite against the Identity, Controller, and
// Node services (Node uses a no-op mounter so no real mounts are performed).
// Set CSI_SANITY=1 to run it; otherwise the test is skipped so that
// `go test ./...` stays green in non-privileged CI environments.
func TestSanity(t *testing.T) {
	if os.Getenv("CSI_SANITY") == "" {
		t.Skip("set CSI_SANITY=1 to run the CSI sanity suite")
	}

	dir := t.TempDir()
	endpoint := "unix://" + filepath.Join(dir, "csi.sock")

	b := csi.NewSanityFakeBackend("n1", "n2", "n3")
	nop := csi.NewNopMounter()

	d := csi.NewDriver(
		endpoint,
		zap.NewNop(),
		csi.NewIdentityServer(),
		csi.NewControllerServer(b, zap.NewNop()),
		csi.NewNodeServer(b, nop, "n1", zap.NewNop()),
	)

	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = d.Run(ctx) }()
	defer cancel()

	cfg := sanity.NewTestConfig()
	cfg.Address = endpoint
	// Do NOT pre-create these — sanity calls os.Mkdir itself and errors if they already exist.
	cfg.TargetPath = filepath.Join(dir, "target")
	cfg.StagingPath = filepath.Join(dir, "staging")
	cfg.TestVolumeParameters = map[string]string{"pool": "vg0"}
	// A VolumeAttributesClass changing an I/O limit, for the ModifyVolume tests.
	cfg.TestVolumeMutableParameters = map[string]string{"readIOPS": "1000"}
	// The nop mounter's EnsureDir creates real subdirectories inside TargetPath/StagingPath,
	// so sanity's default os.Remove (which requires empty dirs) won't work. Use RemoveAll.
	cfg.RemoveTargetPath = func(path string) error { return os.RemoveAll(path) }
	cfg.RemoveStagingPath = func(path string) error { return os.RemoveAll(path) }

	sanity.Test(t, cfg)
}
