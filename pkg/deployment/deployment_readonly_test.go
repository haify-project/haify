package deployment

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// A read-only command against a host that never answers returns when the
// caller's context ends, not after dispatch's 30s SSH dial timeout.
func TestReadOnlyExecReturnsOnTheCallersDeadline(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.toml")
	key := filepath.Join(dir, "id")
	require.NoError(t, os.WriteFile(key, []byte("not a key"), 0o600))
	require.NoError(t, os.WriteFile(cfg, []byte("[ssh]\nuser = \"root\"\nkey_path = \""+key+"\"\n[exec]\nparallel = 2\n"), 0o600))
	c, err := NewWithOptions(zap.NewNop(), Options{ConfigPath: cfg})
	require.NoError(t, err)

	// TEST-NET-1: not routed anywhere, so the dial hangs or fails, never answers.
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, _ = c.DRBDStatus(ctx, []string{"192.0.2.1"}, "r")
	assert.Less(t, time.Since(start), 3*time.Second)
}
