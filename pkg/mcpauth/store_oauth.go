package mcpauth

import (
	"time"

	"go.uber.org/zap"
)

// OAuth clients and the access and refresh tokens issued to them. Only the
// IDs, kinds, roles and expiry of what is issued are ever logged; the secrets
// and their hashes never are.

// RegisterClient records an OAuth client. Past maxClients the oldest goes.
func (s *Store) RegisterClient(name string, redirects []string) (Client, error) {
	log := zap.L()
	log.Debug("Store.RegisterClient: registering client", zap.String("client_name", name),
		zap.Int("redirect_uris", len(redirects)))
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.reloadLocked(); err != nil {
		log.Debug("Store.RegisterClient: reload failed", zap.Error(err))
		return Client{}, err
	}
	id, err := newID("c_")
	if err != nil {
		log.Debug("Store.RegisterClient: could not make a client id", zap.Error(err))
		return Client{}, err
	}
	c := Client{ID: id, Name: name, RedirectURIs: redirects, Created: s.now().UTC()}
	s.data.Clients = append(s.data.Clients, c)
	if len(s.data.Clients) > maxClients {
		log.Debug("Store.RegisterClient: over the client limit, dropping the oldest",
			zap.Int("count", len(s.data.Clients)), zap.Int("max", maxClients))
		s.data.Clients = s.data.Clients[len(s.data.Clients)-maxClients:]
	}
	err = s.saveLocked()
	log.Debug("Store.RegisterClient: client registered", zap.String("client_id", c.ID),
		zap.Int("clients", len(s.data.Clients)), zap.Error(err))
	return c, err
}

// Client looks a registered client up.
func (s *Store) Client(id string) (Client, bool) {
	log := zap.L()
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.reloadLocked(); err != nil {
		log.Debug("Store.Client: reload failed, treating client as unknown", zap.String("client_id", id), zap.Error(err))
		return Client{}, false
	}
	for _, c := range s.data.Clients {
		if c.ID == id {
			log.Debug("Store.Client: client found", zap.String("client_id", id), zap.String("client_name", c.Name))
			return c, true
		}
	}
	log.Debug("Store.Client: client not found", zap.String("client_id", id), zap.Int("clients", len(s.data.Clients)))
	return Client{}, false
}

// IssueOAuth gives a client an access token and a refresh token under the
// static token that approved it, replacing any it already held under that
// approval so the file does not grow with every refresh.
func (s *Store) IssueOAuth(parent Token, client Client, role Role, accessTTL, refreshTTL time.Duration) (access, refresh string, exp time.Time, err error) {
	zap.L().Debug("Store.IssueOAuth: issuing tokens", zap.String("parent_id", parent.ID),
		zap.String("client_id", client.ID), zap.String("role", string(role)),
		zap.Duration("access_ttl", accessTTL), zap.Duration("refresh_ttl", refreshTTL))
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.reloadLocked(); err != nil {
		zap.L().Debug("Store.IssueOAuth: reload failed", zap.Error(err))
		return "", "", time.Time{}, err
	}
	return s.issueLocked(parent.ID, parent.Name, client, role, accessTTL, refreshTTL)
}

func (s *Store) issueLocked(parentID, parentName string, client Client, role Role, accessTTL, refreshTTL time.Duration) (string, string, time.Time, error) {
	log := zap.L()
	kept := s.data.Tokens[:0]
	replaced := 0
	for _, t := range s.data.Tokens {
		if t.Kind != KindStatic && t.Parent == parentID && t.Client == client.ID {
			log.Debug("Store.issueLocked: replacing token the client held under this approval",
				zap.String("token_id", t.ID), zap.String("kind", t.Kind), zap.Time("expires", t.Expires))
			replaced++
			continue
		}
		kept = append(kept, t)
	}
	s.data.Tokens = kept

	access, err := newSecret()
	if err != nil {
		log.Debug("Store.issueLocked: could not make an access token", zap.Error(err))
		return "", "", time.Time{}, err
	}
	refresh, err := newSecret()
	if err != nil {
		log.Debug("Store.issueLocked: could not make a refresh token", zap.Error(err))
		return "", "", time.Time{}, err
	}
	now := s.now().UTC()
	exp := now.Add(accessTTL)
	name := "oauth:" + client.Name + " (approved by " + parentName + ")"
	for _, t := range []struct {
		kind, secret string
		ttl          time.Duration
	}{{KindAccess, access, accessTTL}, {KindRefresh, refresh, refreshTTL}} {
		id, err := newID("o_")
		if err != nil {
			log.Debug("Store.issueLocked: could not make a token id", zap.String("kind", t.kind), zap.Error(err))
			return "", "", time.Time{}, err
		}
		s.data.Tokens = append(s.data.Tokens, Token{
			ID: id, Name: name, Role: role, Kind: t.kind, Hash: hashOf(t.secret),
			Created: now, Expires: now.Add(t.ttl), Parent: parentID, Client: client.ID,
		})
		log.Debug("Store.issueLocked: token issued", zap.String("token_id", id), zap.String("kind", t.kind),
			zap.String("role", string(role)), zap.Time("expires", now.Add(t.ttl)))
	}
	err = s.saveLocked()
	log.Debug("Store.issueLocked: tokens saved", zap.String("parent_id", parentID), zap.String("client_id", client.ID),
		zap.Int("replaced", replaced), zap.Time("access_expires", exp), zap.Error(err))
	return access, refresh, exp, err
}

// Refresh exchanges a refresh token for a new pair. The old refresh token is
// spent: presenting it again fails.
func (s *Store) Refresh(refreshSecret, clientID string, accessTTL, refreshTTL time.Duration) (access, refresh string, exp time.Time, role Role, err error) {
	log := zap.L()
	log.Debug("Store.Refresh: exchanging refresh token", zap.String("client_id", clientID),
		zap.Bool("has_refresh_token", refreshSecret != ""))
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.reloadLocked(); err != nil {
		log.Debug("Store.Refresh: reload failed", zap.Error(err))
		return "", "", time.Time{}, "", err
	}
	t, ok := s.findLocked(hashOf(refreshSecret))
	if !ok || t.Kind != KindRefresh || t.Client != clientID {
		log.Debug("Store.Refresh: refresh token rejected", zap.Bool("found", ok), zap.String("kind", t.Kind),
			zap.Bool("client_matches", t.Client == clientID))
		return "", "", time.Time{}, "", ErrInvalid
	}
	var client Client
	for _, c := range s.data.Clients {
		if c.ID == clientID {
			client = c
		}
	}
	if client.ID == "" {
		log.Debug("Store.Refresh: client no longer registered", zap.String("client_id", clientID))
		return "", "", time.Time{}, "", ErrInvalid
	}
	parentName := t.Parent
	for _, p := range s.data.Tokens {
		if p.ID == t.Parent {
			parentName = p.Name
		}
	}
	log.Debug("Store.Refresh: refresh token accepted", zap.String("token_id", t.ID), zap.String("parent_id", t.Parent),
		zap.String("role", string(t.Role)))
	access, refresh, exp, err = s.issueLocked(t.Parent, parentName, client, t.Role, accessTTL, refreshTTL)
	return access, refresh, exp, t.Role, err
}
