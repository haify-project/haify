package csi

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"go.uber.org/zap"
)

// How long a local agent waits for a resource it just brought up to reach a
// peer with the data, and for a promote to succeed.
var (
	agentConnectTimeout = 60 * time.Second
	agentPromoteTimeout = 30 * time.Second
	agentPollInterval   = time.Second
)

// localStatus is the part of `drbdsetup status --json` the agent reads.
type localStatus struct {
	Name        string `json:"name"`
	Role        string `json:"role"`
	Connections []struct {
		Name            string `json:"name"`
		ConnectionState string `json:"connection-state"`
		PeerRole        string `json:"peer-role"`
		PeerDevices     []struct {
			PeerDiskState string `json:"peer-disk-state"`
		} `json:"peer_devices"`
	} `json:"connections"`
}

// status reads the resource's state on this node; an error means it is not up.
func (a *LocalAgent) status(ctx context.Context, name string) (*localStatus, error) {
	out, err := a.run(ctx, "drbdsetup", "status", name, "--json")
	if err != nil {
		a.log.Debug("LocalAgent.status: not up", zap.String("resource", name), zap.Error(err))
		return nil, fmt.Errorf("drbdsetup status %s: %v: %s", name, err, strings.TrimSpace(out))
	}
	var all []localStatus
	if err := json.Unmarshal([]byte(out), &all); err != nil || len(all) == 0 {
		a.log.Debug("LocalAgent.status: unreadable", zap.String("resource", name), zap.Error(err))
		return nil, fmt.Errorf("drbdsetup status %s: unreadable output", name)
	}
	return &all[0], nil
}

func (a *LocalAgent) isUp(ctx context.Context, name string) bool {
	_, err := a.status(ctx, name)
	return err == nil
}

// reachesData reports whether the node is connected to a peer that has the
// data, which is what it needs before it can be promoted.
func (s *localStatus) reachesData() bool {
	for _, c := range s.Connections {
		if c.ConnectionState != "Connected" {
			continue
		}
		for _, pd := range c.PeerDevices {
			if pd.PeerDiskState == "UpToDate" {
				return true
			}
		}
	}
	return false
}

// otherPrimaries lists the peers that are Primary.
func (s *localStatus) otherPrimaries() []string {
	var out []string
	for _, c := range s.Connections {
		if c.PeerRole == "Primary" {
			out = append(out, c.Name)
		}
	}
	return out
}

// Ensure makes the resource up here and waits until it reaches a peer with the
// data. NodeStage calls it right after attaching the node as a client, rather
// than waiting for the next reconcile.
func (a *LocalAgent) Ensure(ctx context.Context, name string) error {
	start := time.Now()
	if err := a.Reconcile(ctx); err != nil {
		a.log.Debug("LocalAgent.Ensure: reconcile reported errors", zap.String("resource", name), zap.Error(err))
	}
	deadline := time.Now().Add(agentConnectTimeout)
	for {
		st, err := a.status(ctx, name)
		if err == nil && st.reachesData() {
			a.log.Debug("LocalAgent.Ensure: connected", zap.String("resource", name), zap.Duration("took", time.Since(start)))
			return nil
		}
		if time.Now().After(deadline) {
			if err != nil {
				return fmt.Errorf("%s did not come up on this node: %w", name, err)
			}
			return fmt.Errorf("%s is up on this node but reached no peer with the data within %s", name, agentConnectTimeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(agentPollInterval):
		}
		if err != nil {
			// The resource is not up yet: the controller's answer may have
			// predated the attach. Ask again.
			a.log.Debug("LocalAgent.Ensure: not up yet, reconciling again", zap.String("resource", name), zap.Error(err))
			if rerr := a.Reconcile(ctx); rerr != nil {
				a.log.Debug("LocalAgent.Ensure: reconcile reported errors", zap.String("resource", name), zap.Error(rerr))
			}
		}
	}
}

// Promote makes this node Primary. It never forces: with quorum on every
// resource Haify creates, DRBD refuses a promote that could split the data,
// and a refused promote is retried until the old Primary lets go.
func (a *LocalAgent) Promote(ctx context.Context, name string) error {
	start := time.Now()
	deadline := time.Now().Add(agentPromoteTimeout)
	for {
		out, err := a.run(ctx, "drbdadm", "primary", name)
		a.log.Debug("LocalAgent.Promote: drbdadm primary ran", zap.String("resource", name),
			zap.String("output", strings.TrimSpace(out)), zap.Duration("took", time.Since(start)), zap.Error(err))
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("drbdadm primary %s: %v: %s", name, err, strings.TrimSpace(out))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(agentPollInterval):
		}
	}
}

// Demote makes this node Secondary. A resource that is not up here is already
// as good as Secondary.
func (a *LocalAgent) Demote(ctx context.Context, name string) error {
	if !a.isUp(ctx, name) {
		a.log.Debug("LocalAgent.Demote: not up, nothing to demote", zap.String("resource", name))
		return nil
	}
	start := time.Now()
	out, err := a.run(ctx, "drbdadm", "secondary", name)
	a.log.Debug("LocalAgent.Demote: drbdadm secondary ran", zap.String("resource", name),
		zap.String("output", strings.TrimSpace(out)), zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return fmt.Errorf("drbdadm secondary %s: %v: %s", name, err, strings.TrimSpace(out))
	}
	return nil
}

// SetTwoPrimaries allows or disallows two Primaries on the resource's
// connections. DRBD passes the option on to the connected peers.
func (a *LocalAgent) SetTwoPrimaries(ctx context.Context, name string, allow bool) error {
	val := "no"
	if allow {
		val = "yes"
	}
	start := time.Now()
	out, err := a.run(ctx, "drbdadm", "net-options", "--allow-two-primaries="+val, name)
	a.log.Debug("LocalAgent.SetTwoPrimaries: drbdadm net-options ran", zap.String("resource", name), zap.Bool("allow", allow),
		zap.String("output", strings.TrimSpace(out)), zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return fmt.Errorf("drbdadm net-options --allow-two-primaries=%s %s: %v: %s", val, name, err, strings.TrimSpace(out))
	}
	return nil
}

// OtherPrimaries lists the peers that are Primary, as this node sees them.
func (a *LocalAgent) OtherPrimaries(ctx context.Context, name string) ([]string, error) {
	st, err := a.status(ctx, name)
	if err != nil {
		return nil, err
	}
	return st.otherPrimaries(), nil
}

// allowsTwoPrimaries reports whether the resource currently allows two
// Primaries here.
func (a *LocalAgent) allowsTwoPrimaries(ctx context.Context, name string) bool {
	out, err := a.run(ctx, "drbdsetup", "show", name)
	allowed := err == nil && twoPrimariesPattern.MatchString(out)
	a.log.Debug("LocalAgent.allowsTwoPrimaries: read", zap.String("resource", name), zap.Bool("allowed", allowed), zap.Error(err))
	return allowed
}

var twoPrimariesPattern = regexp.MustCompile(`allow-two-primaries\s+yes`)
