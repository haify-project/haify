package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	haifypb "github.com/haify-project/haify/api/proto/v1"
	"go.uber.org/zap"
)

// The receiving cluster's half of `haify resource ship`: the sending
// controller asks this one to open a receiver for one volume, on a node that
// holds the resource Primary, and later to close it and report what arrived.
// The data goes into the DRBD device, so this cluster replicates it to the
// resource's other nodes as it lands. The ship token and the start script
// that carries it are never traced.

const shipStateRoot = "/run/haify-ship"

var shipTokenRE = regexp.MustCompile(`^[0-9a-f]{64}$`)

func shipUnitName(kind, resource string, vol uint32) string {
	return fmt.Sprintf("haify-ship-%s-%s-%d", kind, resource, vol)
}

func shipStateDir(kind, resource string, vol uint32) string {
	return fmt.Sprintf("%s/%s-%s-%d", shipStateRoot, kind, resource, vol)
}

// shipStartScript starts `haify ship <kind>` as a transient unit with its
// token in a root-only file, and waits for it to say it is ready. haify is
// the node's CLI as ensureHaifyCLI found or installed it.
func shipStartScript(haify, kind, resource string, vol uint32, token string, args []string, ready string) string {
	d, unit := shipStateDir(kind, resource, vol), shipUnitName(kind, resource, vol)
	zap.L().Debug("shipStartScript: building", zap.String("kind", kind), zap.String("resource", resource),
		zap.Uint32("volume", vol), zap.String("unit", unit), zap.Bool("has_token", token != ""))
	return fmt.Sprintf(`set -e
%[7]s ship --help >/dev/null 2>&1 || { echo "%[7]s on this node cannot ship; update it" >&2; exit 1; }
systemctl stop %[2]s >/dev/null 2>&1 || true
systemctl reset-failed %[2]s >/dev/null 2>&1 || true
rm -rf %[1]s
mkdir -p -m 0700 %[1]s
umask 077
printf %%s %[3]s > %[1]s/token
systemd-run --quiet `+collectFlag+` --unit=%[2]s %[7]s ship %[4]s --token-file %[1]s/token --state-dir %[1]s %[5]s
for i in $(seq 1 120); do [ -s %[1]s/%[6]s ] && break; [ -s %[1]s/result.json ] && break; sleep 0.5; done
cat %[1]s/%[6]s 2>/dev/null || cat %[1]s/result.json 2>/dev/null || echo "no answer from the %[4]s unit" >&2
`, d, unit, token, kind, strings.Join(args, " "), ready, haify)
}

// shipCleanupScript stops the unit and removes its state.
func shipCleanupScript(kind, resource string, vol uint32) string {
	return fmt.Sprintf("systemctl stop %s >/dev/null 2>&1; rm -rf %s; true",
		shipUnitName(kind, resource, vol), shipStateDir(kind, resource, vol))
}

// shipReceiveNode is the node that takes the data: the resource's first
// diskful node.
func (rm *ResourceManager) shipReceiveNode(ctx context.Context, resource string) (string, string, error) {
	log := ctrlLogOrGlobal(rm.controller)
	r, err := rm.controller.db.GetResource(ctx, resource)
	if err != nil || r == nil {
		log.Debug("shipReceiveNode: resource not found", zap.String("resource", resource), zap.Error(err))
		return "", "", fmt.Errorf("resource %q not found", resource)
	}
	nodes := splitCSV(r.Nodes)
	if len(nodes) == 0 {
		log.Debug("shipReceiveNode: no replicas", zap.String("resource", resource))
		return "", "", fmt.Errorf("resource %q has no replicas", resource)
	}
	host := rm.controller.ResolveHost(nodes[0])
	log.Debug("shipReceiveNode: chosen", zap.String("resource", resource), zap.String("node", nodes[0]), zap.String("host", host))
	return nodes[0], host, nil
}

// PrepareShipReceive opens a receiver for volume vol of resource.
func (rm *ResourceManager) PrepareShipReceive(ctx context.Context, resource string, vol uint32, token string) (*haifypb.PrepareShipReceiveResponse, error) {
	log := ctrlLogOrGlobal(rm.controller)
	log.Debug("PrepareShipReceive: opening receiver", zap.String("resource", resource), zap.Uint32("volume", vol),
		zap.Bool("has_token", token != ""))
	if !shipTokenRE.MatchString(token) {
		log.Debug("PrepareShipReceive: malformed token", zap.Int("token_len", len(token)))
		return nil, fmt.Errorf("the token must be 64 hex characters")
	}
	node, host, err := rm.shipReceiveNode(ctx, resource)
	if err != nil {
		return nil, err
	}
	haify, err := rm.ensureHaifyCLI(ctx, host)
	if err != nil {
		log.Debug("PrepareShipReceive: no haify CLI on the receiver", zap.String("node", node), zap.Error(err))
		return nil, err
	}
	if err := rm.SetPrimary(ctx, resource, node, false); err != nil {
		log.Debug("PrepareShipReceive: promote failed", zap.String("node", node), zap.Error(err))
		return nil, fmt.Errorf("make %s Primary on %s to receive: %w", resource, node, err)
	}
	args := []string{"--device", fmt.Sprintf("/dev/drbd/by-res/%s/%d", resource, vol), "--listen", "0.0.0.0:0"}
	start := time.Now()
	res, err := rm.deployment.Exec(ctx, []string{host},
		"echo "+base64Std(shipStartScript(haify, "recv", resource, vol, token, args, "ready"))+" | base64 -d | sudo /bin/bash")
	if err := execFailure(res, err); err != nil {
		log.Debug("PrepareShipReceive: receiver did not start", zap.String("host", host), zap.Duration("took", time.Since(start)), zap.Error(err))
		return nil, fmt.Errorf("start the receiver on %s: %w", node, err)
	}
	var out string
	for _, h := range res.Hosts {
		out = strings.TrimSpace(h.Output)
	}
	f := strings.Fields(out)
	if len(f) != 2 || len(f[0]) != 64 {
		log.Debug("PrepareShipReceive: unexpected ready line", zap.String("host", host), zap.Int("fields", len(f)))
		return nil, fmt.Errorf("the receiver on %s did not start: %s", node, out)
	}
	port, err := strconv.ParseUint(f[1], 10, 16)
	if err != nil {
		log.Debug("PrepareShipReceive: bad port", zap.String("port", f[1]), zap.Error(err))
		return nil, fmt.Errorf("the receiver on %s reported port %q", node, f[1])
	}
	address := rm.controller.nodes.GetNodeAddressByName(node)
	log.Debug("PrepareShipReceive: receiver listening", zap.String("node", node), zap.String("address", address),
		zap.Uint64("port", port), zap.String("fingerprint", f[0]), zap.Duration("took", time.Since(start)))
	return &haifypb.PrepareShipReceiveResponse{Success: true, Address: address,
		Port: uint32(port), Fingerprint: f[0], Node: node}, nil
}

// shipNodeResult is result.json as `haify ship` writes it.
type shipNodeResult struct {
	OK     bool   `json:"ok"`
	Bytes  uint64 `json:"bytes"`
	SHA256 string `json:"sha256"`
	Error  string `json:"error"`
}

// readShipResult waits for kind's result on host, up to wait.
func (rm *ResourceManager) readShipResult(ctx context.Context, host, kind, resource string, vol uint32, wait time.Duration) (*shipNodeResult, bool, error) {
	log := ctrlLogOrGlobal(rm.controller)
	path := shipStateDir(kind, resource, vol) + "/result.json"
	deadline := time.Now().Add(wait)
	log.Debug("readShipResult: waiting", zap.String("host", host), zap.String("path", path), zap.Duration("wait", wait))
	for {
		start := time.Now()
		res, err := rm.deployment.Exec(ctx, []string{host}, "sudo cat "+path+" 2>/dev/null; true")
		if err != nil {
			log.Debug("readShipResult: read errored", zap.String("host", host), zap.Duration("took", time.Since(start)), zap.Error(err))
			return nil, false, err
		}
		var out string
		for _, h := range res.Hosts {
			out = strings.TrimSpace(h.Output)
		}
		if out != "" {
			var r shipNodeResult
			if err := json.Unmarshal([]byte(out), &r); err != nil {
				log.Debug("readShipResult: result unreadable", zap.String("host", host), zap.Error(err))
				return nil, true, fmt.Errorf("unreadable result on %s: %s", host, out)
			}
			log.Debug("readShipResult: result", zap.String("host", host), zap.Bool("ok", r.OK), zap.Uint64("bytes", r.Bytes),
				zap.String("error", r.Error))
			return &r, true, nil
		}
		if time.Now().After(deadline) {
			log.Debug("readShipResult: no result yet", zap.String("host", host), zap.String("path", path))
			return nil, false, nil
		}
		select {
		case <-ctx.Done():
			log.Debug("readShipResult: cancelled", zap.String("host", host), zap.Error(ctx.Err()))
			return nil, false, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// FinishShipReceive closes the receiver for vol and reports what arrived;
// last also releases the resource.
func (rm *ResourceManager) FinishShipReceive(ctx context.Context, resource string, vol uint32, last bool) (*haifypb.FinishShipReceiveResponse, error) {
	log := ctrlLogOrGlobal(rm.controller)
	log.Debug("FinishShipReceive: closing receiver", zap.String("resource", resource), zap.Uint32("volume", vol), zap.Bool("last", last))
	node, host, err := rm.shipReceiveNode(ctx, resource)
	if err != nil {
		return nil, err
	}
	r, found, err := rm.readShipResult(ctx, host, "recv", resource, vol, 30*time.Second)
	_, cerr := rm.deployment.Exec(ctx, []string{host}, "sudo sh -c '"+shipCleanupScript("recv", resource, vol)+"'")
	log.Debug("FinishShipReceive: receiver cleaned up", zap.String("host", host), zap.Error(cerr))
	if last {
		if serr := rm.SetSecondary(ctx, resource, node); serr != nil && err == nil {
			log.Debug("FinishShipReceive: demote failed", zap.String("node", node), zap.Error(serr))
			err = fmt.Errorf("release %s on %s: %w", resource, node, serr)
		} else {
			log.Debug("FinishShipReceive: resource released", zap.String("node", node), zap.NamedError("demote_error", serr))
		}
	}
	switch {
	case err != nil:
		log.Debug("FinishShipReceive: failed", zap.Error(err))
		return nil, err
	case !found:
		log.Debug("FinishShipReceive: receiver never finished", zap.String("node", node))
		return nil, fmt.Errorf("the receiver on %s never finished", node)
	case !r.OK:
		log.Debug("FinishShipReceive: receive failed", zap.String("error", r.Error), zap.Uint64("bytes", r.Bytes))
		return &haifypb.FinishShipReceiveResponse{Success: false, Message: r.Error, Bytes: r.Bytes}, nil
	}
	log.Debug("FinishShipReceive: received", zap.Uint64("bytes", r.Bytes), zap.String("node", node))
	return &haifypb.FinishShipReceiveResponse{Success: true, Bytes: r.Bytes, Sha256: r.SHA256,
		Message: fmt.Sprintf("%d bytes written on %s", r.Bytes, node)}, nil
}
