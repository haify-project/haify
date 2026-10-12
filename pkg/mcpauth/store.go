// Package mcpauth is the authentication behind the remote MCP server: access
// tokens with a role each, and the small OAuth 2.1 authorization server that
// lets clients which cannot be handed a token by hand — ChatGPT, claude.ai —
// obtain one.
//
// Claude Code and scripts present a token created with `haify-mcp token create`
// as a bearer credential. A client that speaks only OAuth is sent through the
// authorization flow in oauth.go, where the operator pastes such a token once
// to approve it; what the client gets back is a short-lived token of the same
// role or lower, revoked whenever the token that approved it is.
//
// Only a SHA-256 of a token is stored. A token is 256 random bits, so a fast
// hash is enough: there is nothing to brute-force, and lookups by hash need no
// comparison against every record.
package mcpauth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
)

// Token kinds.
const (
	// KindStatic is a token an operator created; it is what gets pasted into
	// a client's configuration or into the authorization page.
	KindStatic = "static"
	// KindAccess and KindRefresh are what the OAuth flow issues.
	KindAccess  = "access"
	KindRefresh = "refresh"
)

// ErrInvalid is returned for a token that is unknown, expired or revoked. It
// says nothing about which, so a caller probing cannot tell.
var ErrInvalid = errors.New("invalid token")

// Token is one stored credential. The secret itself is never stored.
type Token struct {
	ID      string    `json:"id"`
	Name    string    `json:"name"`
	Role    Role      `json:"role"`
	Kind    string    `json:"kind"`
	Hash    string    `json:"hash"`
	Created time.Time `json:"created"`
	// Expires is zero for a token that never does.
	Expires time.Time `json:"expires,omitzero"`
	// Parent is the static token that approved an OAuth token. Revoking the
	// parent revokes everything issued under it.
	Parent string `json:"parent,omitempty"`
	Client string `json:"client,omitempty"`
}

// Client is an OAuth client that registered itself.
type Client struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	RedirectURIs []string  `json:"redirect_uris"`
	Created      time.Time `json:"created"`
}

type fileData struct {
	Version int      `json:"version"`
	Tokens  []Token  `json:"tokens"`
	Clients []Client `json:"clients,omitempty"`
}

// maxClients bounds registrations: anyone who can reach the server may
// register a client, and a client can do nothing until an operator approves
// it, so the bound is only about the file not growing without limit.
const maxClients = 100

// Store keeps tokens and OAuth clients in one file.
//
// The file is re-read when it changes on disk, so `haify-mcp token revoke` run
// beside a serving process takes effect on the next request without a signal.
type Store struct {
	path string
	now  func() time.Time

	mu    sync.Mutex
	mtime time.Time
	data  fileData
}

// Open loads the store at path, creating it (and its directory, private to the
// owner) when there is none yet.
func Open(path string) (*Store, error) {
	log := zap.L()
	log.Debug("Open: opening token store", zap.String("path", path))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		log.Debug("Open: could not create the store directory", zap.String("dir", filepath.Dir(path)), zap.Error(err))
		return nil, fmt.Errorf("create %s: %w", filepath.Dir(path), err)
	}
	s := &Store{path: path, now: time.Now, data: fileData{Version: 1}}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.reloadLocked(); err != nil {
		log.Debug("Open: initial load failed", zap.String("path", path), zap.Error(err))
		return nil, err
	}
	log.Debug("Open: token store open", zap.String("path", path), zap.Int("tokens", len(s.data.Tokens)),
		zap.Int("clients", len(s.data.Clients)))
	return s, nil
}

func (s *Store) reloadLocked() error {
	log := zap.L()
	fi, err := os.Stat(s.path)
	if errors.Is(err, os.ErrNotExist) {
		log.Debug("Store.reloadLocked: no store file yet, creating it", zap.String("path", s.path))
		return s.saveLocked()
	}
	if err != nil {
		log.Debug("Store.reloadLocked: stat failed", zap.String("path", s.path), zap.Error(err))
		return fmt.Errorf("stat %s: %w", s.path, err)
	}
	if fi.ModTime().Equal(s.mtime) {
		return nil
	}
	start := time.Now()
	raw, err := os.ReadFile(s.path)
	if err != nil {
		log.Debug("Store.reloadLocked: read failed", zap.String("path", s.path), zap.Error(err))
		return fmt.Errorf("read %s: %w", s.path, err)
	}
	var d fileData
	if err := json.Unmarshal(raw, &d); err != nil {
		log.Debug("Store.reloadLocked: file is not a token store", zap.String("path", s.path), zap.Error(err))
		return fmt.Errorf("%s is not a token store: %w", s.path, err)
	}
	s.data, s.mtime = d, fi.ModTime()
	log.Debug("Store.reloadLocked: store changed on disk, reloaded", zap.String("path", s.path),
		zap.Time("mtime", fi.ModTime()), zap.Int("size_bytes", len(raw)), zap.Int("tokens", len(d.Tokens)),
		zap.Int("clients", len(d.Clients)), zap.Duration("took", time.Since(start)))
	return nil
}

func (s *Store) saveLocked() error {
	log := zap.L()
	start := time.Now()
	s.pruneLocked()
	raw, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		log.Debug("Store.saveLocked: marshal failed", zap.Error(err))
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".tokens-*")
	if err != nil {
		log.Debug("Store.saveLocked: could not create temp file", zap.String("dir", filepath.Dir(s.path)), zap.Error(err))
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		log.Debug("Store.saveLocked: chmod temp file failed", zap.String("tmp", tmp.Name()), zap.Error(err))
		return err
	}
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		log.Debug("Store.saveLocked: write temp file failed", zap.String("tmp", tmp.Name()), zap.Error(err))
		return err
	}
	if err := tmp.Close(); err != nil {
		log.Debug("Store.saveLocked: close temp file failed", zap.String("tmp", tmp.Name()), zap.Error(err))
		return err
	}
	if err := os.Rename(tmp.Name(), s.path); err != nil {
		log.Debug("Store.saveLocked: rename into place failed", zap.String("path", s.path), zap.Error(err))
		return err
	}
	if fi, err := os.Stat(s.path); err == nil {
		s.mtime = fi.ModTime()
	} else {
		log.Debug("Store.saveLocked: stat after save failed, mtime not updated", zap.String("path", s.path), zap.Error(err))
	}
	log.Debug("Store.saveLocked: store written", zap.String("path", s.path), zap.Int("size_bytes", len(raw)),
		zap.Int("tokens", len(s.data.Tokens)), zap.Int("clients", len(s.data.Clients)), zap.Duration("took", time.Since(start)))
	return nil
}

// pruneLocked drops what has expired, and OAuth tokens whose parent is gone.
func (s *Store) pruneLocked() {
	now := s.now()
	live := map[string]bool{}
	for _, t := range s.data.Tokens {
		if t.Kind == KindStatic && (t.Expires.IsZero() || t.Expires.After(now)) {
			live[t.ID] = true
		}
	}
	log := zap.L()
	before := len(s.data.Tokens)
	kept := s.data.Tokens[:0]
	for _, t := range s.data.Tokens {
		if !t.Expires.IsZero() && !t.Expires.After(now) {
			log.Debug("Store.pruneLocked: dropping expired token", zap.String("token_id", t.ID),
				zap.String("kind", t.Kind), zap.Time("expires", t.Expires))
			continue
		}
		if t.Kind != KindStatic && !live[t.Parent] {
			log.Debug("Store.pruneLocked: dropping OAuth token whose approving token is gone",
				zap.String("token_id", t.ID), zap.String("kind", t.Kind), zap.String("parent_id", t.Parent))
			continue
		}
		kept = append(kept, t)
	}
	s.data.Tokens = kept
	if len(kept) != before {
		log.Debug("Store.pruneLocked: pruned tokens", zap.Int("before", before), zap.Int("after", len(kept)))
	}
}

func newSecret() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "haifymcp_" + base64.RawURLEncoding.EncodeToString(b), nil
}

func hashOf(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

func newID(prefix string) (string, error) {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(b), nil
}

// Create makes a static token. ttl zero means it does not expire. The secret
// is returned once and is not recoverable afterwards.
func (s *Store) Create(name string, role Role, ttl time.Duration) (string, Token, error) {
	log := zap.L()
	log.Debug("Store.Create: creating static token", zap.String("name", name), zap.String("role", string(role)),
		zap.Duration("ttl", ttl))
	name = strings.TrimSpace(name)
	if name == "" {
		log.Debug("Store.Create: refused, no name")
		return "", Token{}, errors.New("a token needs a name")
	}
	if role.Rank() == 0 {
		log.Debug("Store.Create: refused, unknown role", zap.String("role", string(role)))
		return "", Token{}, fmt.Errorf("unknown role %q", role)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.reloadLocked(); err != nil {
		log.Debug("Store.Create: reload failed", zap.Error(err))
		return "", Token{}, err
	}
	for _, t := range s.data.Tokens {
		if t.Kind == KindStatic && t.Name == name {
			log.Debug("Store.Create: refused, name taken", zap.String("name", name), zap.String("token_id", t.ID))
			return "", Token{}, fmt.Errorf("a token named %q already exists; revoke it or pick another name", name)
		}
	}
	secret, err := newSecret()
	if err != nil {
		log.Debug("Store.Create: could not make a secret", zap.Error(err))
		return "", Token{}, err
	}
	id, err := newID("t_")
	if err != nil {
		log.Debug("Store.Create: could not make a token id", zap.Error(err))
		return "", Token{}, err
	}
	t := Token{ID: id, Name: name, Role: role, Kind: KindStatic, Hash: hashOf(secret), Created: s.now().UTC()}
	if ttl > 0 {
		t.Expires = t.Created.Add(ttl)
		log.Debug("Store.Create: token expires", zap.Time("expires", t.Expires))
	} else {
		log.Debug("Store.Create: token never expires")
	}
	s.data.Tokens = append(s.data.Tokens, t)
	if err := s.saveLocked(); err != nil {
		log.Debug("Store.Create: save failed", zap.Error(err))
		return "", Token{}, err
	}
	log.Debug("Store.Create: token created", zap.String("token_id", t.ID), zap.String("name", t.Name),
		zap.String("role", string(t.Role)))
	return secret, t, nil
}

// Verify finds the live access credential a secret belongs to: a static token,
// or an OAuth access token whose approving token still stands. A refresh token
// is not one.
func (s *Store) Verify(secret string) (Token, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	log := zap.L()
	if err := s.reloadLocked(); err != nil {
		log.Debug("Store.Verify: reload failed", zap.Error(err))
		return Token{}, err
	}
	t, ok := s.findLocked(hashOf(secret))
	if !ok || t.Kind == KindRefresh {
		log.Debug("Store.Verify: credential rejected", zap.Bool("has_secret", secret != ""), zap.Bool("found", ok),
			zap.String("kind", t.Kind))
		return Token{}, ErrInvalid
	}
	log.Debug("Store.Verify: credential accepted", zap.String("token_id", t.ID), zap.String("kind", t.Kind),
		zap.String("role", string(t.Role)), zap.Time("expires", t.Expires))
	return t, nil
}

// findLocked returns the live token with the hash: not expired, and for an
// OAuth token, with its parent still live.
func (s *Store) findLocked(hash string) (Token, bool) {
	now := s.now()
	var found *Token
	live := map[string]bool{}
	for i := range s.data.Tokens {
		t := &s.data.Tokens[i]
		if t.Kind == KindStatic && (t.Expires.IsZero() || t.Expires.After(now)) {
			live[t.ID] = true
		}
		if t.Hash == hash {
			found = t
		}
	}
	log := zap.L()
	if found == nil {
		log.Debug("Store.findLocked: no token with that hash", zap.Int("tokens", len(s.data.Tokens)))
		return Token{}, false
	}
	if !found.Expires.IsZero() && !found.Expires.After(now) {
		log.Debug("Store.findLocked: token expired", zap.String("token_id", found.ID), zap.String("kind", found.Kind),
			zap.Time("expires", found.Expires))
		return Token{}, false
	}
	if found.Kind != KindStatic && !live[found.Parent] {
		log.Debug("Store.findLocked: approving token no longer live", zap.String("token_id", found.ID),
			zap.String("kind", found.Kind), zap.String("parent_id", found.Parent))
		return Token{}, false
	}
	return *found, true
}

// Revoke removes the static token with that ID or name, and everything issued
// under it. It reports how many records went.
func (s *Store) Revoke(ref string) (int, error) {
	log := zap.L()
	// A secret pasted here by mistake must not reach the log.
	refField := zap.String("ref", ref)
	if strings.HasPrefix(ref, "haifymcp_") {
		refField = zap.String("ref", "<secret, not logged>")
	}
	log.Debug("Store.Revoke: revoking token", refField)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.reloadLocked(); err != nil {
		log.Debug("Store.Revoke: reload failed", zap.Error(err))
		return 0, err
	}
	var id string
	for _, t := range s.data.Tokens {
		if t.Kind == KindStatic && (t.ID == ref || t.Name == ref) {
			id = t.ID
		}
	}
	if id == "" {
		log.Debug("Store.Revoke: no such token", refField)
		return 0, fmt.Errorf("no token named or numbered %q", ref)
	}
	kept := s.data.Tokens[:0]
	removed := 0
	for _, t := range s.data.Tokens {
		if t.ID == id || t.Parent == id {
			log.Debug("Store.Revoke: removing token", zap.String("token_id", t.ID), zap.String("kind", t.Kind))
			removed++
			continue
		}
		kept = append(kept, t)
	}
	s.data.Tokens = kept
	err := s.saveLocked()
	log.Debug("Store.Revoke: revoked", zap.String("token_id", id), zap.Int("removed", removed), zap.Error(err))
	return removed, err
}

// List returns the static tokens, oldest first.
func (s *Store) List() ([]Token, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.reloadLocked(); err != nil {
		zap.L().Debug("Store.List: reload failed", zap.Error(err))
		return nil, err
	}
	var out []Token
	for _, t := range s.data.Tokens {
		if t.Kind == KindStatic {
			out = append(out, t)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.Before(out[j].Created) })
	zap.L().Debug("Store.List: listed static tokens", zap.Int("count", len(out)), zap.Int("records", len(s.data.Tokens)))
	return out, nil
}
