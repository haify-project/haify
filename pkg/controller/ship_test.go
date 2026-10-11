package controller

import (
	"context"
	"errors"
	"strings"
	"testing"

	haifypb "github.com/haify-project/haify/api/proto/v1"
	"github.com/stretchr/testify/assert"
)

type fakeShipTarget struct {
	ShipTarget
	list    []*haifypb.ResourceInfo
	listErr error
}

func (f *fakeShipTarget) ListResources(context.Context) ([]*haifypb.ResourceInfo, error) {
	return f.list, f.listErr
}

func TestCheckShipTarget(t *testing.T) {
	ctx := context.Background()
	bad := &fakeShipTarget{listErr: errors.New("rpc error: code = Unauthenticated desc = missing or invalid API token")}
	assert.ErrorContains(t, checkShipTarget(ctx, bad, "10.1.0.10:3374", "db"), "Unauthenticated",
		"a wrong token stops the ship before any snapshot is taken")

	taken := &fakeShipTarget{list: []*haifypb.ResourceInfo{{Name: "db"}}}
	assert.ErrorContains(t, checkShipTarget(ctx, taken, "10.1.0.10:3374", "db"), "already has a resource named db")
	assert.NoError(t, checkShipTarget(ctx, taken, "10.1.0.10:3374", "db-copy"))
}

// The token is on no command line: the start script writes it to a root-only
// file and the unit reads it from there.
func TestShipStartScriptKeepsTheTokenOffTheCommandLine(t *testing.T) {
	tok := strings.Repeat("ab", 32)
	s := shipStartScript("recv", "db", 0, tok, []string{"--device", "/dev/drbd/by-res/db/0"}, "ready")
	assert.Contains(t, s, "umask 077")
	assert.Contains(t, s, "printf %s "+tok+" > /run/haify-ship/recv-db-0/token")
	run := s[strings.Index(s, "systemd-run"):]
	run = run[:strings.Index(run, "\n")]
	assert.NotContains(t, run, tok)
	assert.Contains(t, run, "--token-file /run/haify-ship/recv-db-0/token")
}
