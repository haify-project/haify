package client

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveTokenPriority(t *testing.T) {
	// 1. Explicit token
	assert.Equal(t, "explicit-token", ResolveToken("explicit-token"))

	// 2. Env variable
	t.Setenv("HAIFY_TOKEN", "env-token")
	assert.Equal(t, "env-token", ResolveToken(""))

	// 3. User home token file
	t.Setenv("HAIFY_TOKEN", "")
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	tokenDir := filepath.Join(tmpHome, ".haify")
	require.NoError(t, os.MkdirAll(tokenDir, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(tokenDir, "token"), []byte(" home-token \n"), 0600))

	assert.Equal(t, "home-token", ResolveToken(""))
}

func TestReadTokenFileNonExistent(t *testing.T) {
	assert.Empty(t, readTokenFile("/non/existent/path/for/token"))
}

func TestResolveTokenTrimsEnvAndFlag(t *testing.T) {
	t.Setenv("HAIFY_TOKEN", "abc123\n")
	assert.Equal(t, "abc123", ResolveToken(""), "a Secret made from a file keeps the file's newline")
	assert.Equal(t, "flagtok", ResolveToken("  flagtok\n"))
}
