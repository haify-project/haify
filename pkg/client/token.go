package client

import (
	"os"
	"path/filepath"
	"strings"

	"go.uber.org/zap"
)

// SystemTokenPath is the host-wide API token location, written during
// cluster setup so every local tool can authenticate without flags.
const SystemTokenPath = "/etc/haify/token"

// ResolveToken returns the API token for controller requests.
// Resolution order, highest priority first:
//
//  1. explicit value (e.g. a --token flag)
//  2. HAIFY_TOKEN environment variable
//  3. ~/.haify/token (per-user)
//  4. /etc/haify/token (host-wide)
//
// An empty result means no token is sent, which only works against
// controllers with [auth] disabled.
//
// The traces name the source the token came from, never the token itself.
func ResolveToken(explicit string) string {
	log := zap.L()
	// Whitespace is trimmed from every source, not only files: a Kubernetes
	// Secret made with --from-file of a file ending in a newline hands
	// HAIFY_TOKEN the newline, and a header value carrying one makes the
	// controller reset the stream with PROTOCOL_ERROR on every call.
	if explicit = strings.TrimSpace(explicit); explicit != "" {
		log.Debug("ResolveToken: using explicit token", zap.String("source", "flag"))
		return explicit
	}
	if env := strings.TrimSpace(os.Getenv("HAIFY_TOKEN")); env != "" {
		log.Debug("ResolveToken: using token from environment", zap.String("source", "HAIFY_TOKEN"))
		return env
	}
	if home, err := os.UserHomeDir(); err == nil {
		userPath := filepath.Join(home, ".haify", "token")
		if token := readTokenFile(userPath); token != "" {
			log.Debug("ResolveToken: using per-user token file", zap.String("source", "file"), zap.String("path", userPath))
			return token
		}
	} else {
		log.Debug("ResolveToken: no home directory, skipping per-user token file", zap.Error(err))
	}
	token := readTokenFile(SystemTokenPath)
	if token != "" {
		log.Debug("ResolveToken: using host-wide token file", zap.String("source", "file"), zap.String("path", SystemTokenPath))
	} else {
		log.Debug("ResolveToken: no token found, requests go unauthenticated")
	}
	return token
}

func readTokenFile(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		zap.L().Debug("readTokenFile: token file not readable", zap.String("path", path), zap.Error(err))
		return ""
	}
	token := strings.TrimSpace(string(data))
	zap.L().Debug("readTokenFile: read token file", zap.String("path", path), zap.Bool("has_token", token != ""))
	return token
}
