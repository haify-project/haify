package controller

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/haify-project/haify/pkg/database"
)

// Growing the filesystem a storage node has mounted from a volume.
//
// Growing a volume grows the backing volumes and the DRBD device. A
// filesystem on that device keeps its size until it is grown too, and who
// does that depends on who mounted it. A Pod's node, a hypervisor or a client
// of an iSCSI target grows its own. The filesystem of an NFS or SMB gateway
// is mounted by the gateway's promoter on a storage node, where nobody else
// reaches it: the export would go on showing the old size for ever. So for a
// resource with such a gateway the resize grows the filesystem as well, online,
// on the node that has it mounted — the one where the resource is Primary. A
// request can ask for the same on any other resource (grow_filesystem), for a
// filesystem `ha create` or `resource mount` put there.
//
// The filesystem can only be grown where it is mounted. While the gateway is
// failing over or stopped it is mounted nowhere, and the resize then reports
// that it is not finished rather than that it worked: repeating it later
// finds the volume already grown and does only what is left.

const (
	// growFSSettle is how long the script waits for the Primary to see the
	// new device size, which can trail `drbdadm resize` on another node.
	growFSSettle = 15 * time.Second
	// growFSTimeout bounds the script: the wait above, then the grow, which
	// on a large ext4 filesystem writes new group descriptors for a while.
	growFSTimeout = 5 * time.Minute
)

// growFSNameRe is what a resource name may be to be put into the script.
var growFSNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

// growFSScript grows the filesystem mounted from one volume of a resource on
// this node, and says what it found as key=value lines.
//
// It decides by itself whether the resource has a file gateway, from the
// promoter config drbd-reactor reads: that file is on every node the gateway
// can run on, stopped or not, and is there for gateways older than the
// controller's own record of them.
//
// The device is the one DRBD names for the volume, and the mount is found by
// that device, never by a path convention: an export path is whatever the
// operator chose. The size is waited for because growing a filesystem onto a
// device that has not grown yet succeeds and changes nothing. It is waited
// for to within a sixty-fourth: a device can come out a little under the
// size that was asked for (its backing volume is rounded to whole extents,
// and DRBD keeps its metadata at the end), and one that is still at its old
// size is nowhere near.
const growFSScript = `#!/bin/bash
cd /
export LC_ALL=C
res='@RES@'
vol=@VOL@
want=@WANT@
force=@FORCE@
say() { echo "fs=$1"; [ -z "$2" ] || echo "detail=$(echo "$2" | tr '\n' ' ')"; exit 0; }
gw=none
for t in nfs smb; do
  if ls /etc/drbd-reactor.d/haify-$t-$res.toml* >/dev/null 2>&1; then gw=$t; break; fi
done
echo "gateway=$gw"
[ "$gw" != none ] || [ "$force" = 1 ] || say not-needed
dev=$(drbdadm sh-dev "$res/$vol" 2>/dev/null | head -n1)
[ -n "$dev" ] || dev="/dev/drbd/by-res/$res/$vol"
real=$(readlink -f "$dev" 2>/dev/null)
[ -n "$real" ] && [ -b "$real" ] || say failed "$dev is not a block device on $(hostname)"
target=$(findmnt -rn -S "$real" -o TARGET 2>/dev/null | head -n1)
[ -n "$target" ] || say not-mounted
fstype=$(findmnt -rn -S "$real" -o FSTYPE 2>/dev/null | head -n1)
echo "mount=$target"
echo "fstype=$fstype"
i=0
min=$((want - want / 64))
bytes=$(blockdev --getsize64 "$real" 2>/dev/null)
while [ "${bytes:-0}" -lt "$min" ] && [ $i -lt @SETTLE@ ]; do
  sleep 1; i=$((i+1))
  bytes=$(blockdev --getsize64 "$real" 2>/dev/null)
done
[ "${bytes:-0}" -ge "$min" ] || say failed "the device still has ${bytes:-0} bytes here, not the $want it was grown to"
case "$fstype" in
  ext2|ext3|ext4) out=$(resize2fs "$real" 2>&1) || say failed "$out" ;;
  xfs) out=$(xfs_growfs "$target" 2>&1) || say failed "$out" ;;
  btrfs) out=$(btrfs filesystem resize max "$target" 2>&1) || say failed "$out" ;;
  *) say unsupported "$fstype" ;;
esac
echo "size=$(df -B1 --output=size "$target" 2>/dev/null | tail -n1 | tr -d ' ')"
say grown
`

// growFSResult is what one node said about the filesystem.
type growFSResult struct {
	node    string
	outcome string // grown, not-needed, not-mounted, unsupported, failed
	gateway string
	mount   string
	fstype  string
	size    uint64
	detail  string
}

// GrowFilesystem grows the filesystem mounted from one volume of resource,
// after the volume was grown to at least wantBytes.
//
// With required false it acts only on a resource that has an NFS or SMB
// gateway and is a no-op for every other; with required true it acts on any
// resource. It returns a sentence for the reply of the resize, "" when there
// was nothing to do. An error means the filesystem should have been grown
// and was not; the volume itself is grown either way, and calling again once
// the filesystem is mounted finishes the job.
func (rm *ResourceManager) GrowFilesystem(ctx context.Context, resource string, volumeID uint32, wantBytes uint64, required bool) (string, error) {
	log := rm.resourcesLog()
	log.Debug("GrowFilesystem: start", zap.String("resource", resource), zap.Uint32("volume_id", volumeID),
		zap.Uint64("want_bytes", wantBytes), zap.Bool("required", required))
	if rm.deployment == nil {
		log.Debug("GrowFilesystem: no deployment client", zap.String("resource", resource))
		return "", fmt.Errorf("deployment client not set")
	}
	if !growFSNameRe.MatchString(resource) {
		log.Debug("GrowFilesystem: resource name not usable in a script", zap.String("resource", resource))
		if required {
			return "", fmt.Errorf("%q is not a resource name a filesystem can be grown for", resource)
		}
		return "", nil
	}
	// Where the resource is Primary is where its filesystem is mounted. With
	// no Primary it is mounted nowhere; the replica nodes are then asked
	// only whether there is a gateway whose filesystem should have been.
	hosts := rm.guestHosts(ctx, resource)
	primary := len(hosts) > 0
	if !primary {
		var err error
		if hosts, err = rm.resourceHosts(ctx, resource); err != nil {
			log.Debug("GrowFilesystem: no hosts", zap.String("resource", resource), zap.Error(err))
			return "", err
		}
	}
	force := "0"
	if required {
		force = "1"
	}
	script := strings.NewReplacer(
		"@RES@", resource,
		"@VOL@", strconv.FormatUint(uint64(volumeID), 10),
		"@WANT@", strconv.FormatUint(wantBytes, 10),
		"@FORCE@", force,
		"@SETTLE@", strconv.Itoa(int(growFSSettle.Seconds())),
	).Replace(growFSScript)

	var results []growFSResult
	for _, host := range hosts {
		kv, err := rm.runGuestScript(ctx, host, script, growFSTimeout)
		r := growFSResult{node: rm.nodeLabel(host), outcome: kv["fs"], gateway: kv["gateway"], mount: kv["mount"],
			fstype: kv["fstype"], detail: kv["detail"]}
		r.size, _ = strconv.ParseUint(kv["size"], 10, 64)
		if r.outcome == "" {
			// A script that did not say what happened did not run to its end.
			r.outcome = "failed"
			r.detail = "the node did not answer"
			if err != nil {
				r.detail = err.Error()
			}
		}
		log.Debug("GrowFilesystem: host answered", zap.String("resource", resource), zap.String("host", host),
			zap.String("outcome", r.outcome), zap.String("gateway", r.gateway), zap.String("mount", r.mount),
			zap.String("fs_type", r.fstype), zap.Uint64("size_bytes", r.size), zap.String("detail", r.detail), zap.Error(err))
		results = append(results, r)
		// One node that grew it settles it. With no Primary the nodes are
		// only asked whether a gateway exists, and its config is on every
		// replica node alike: the first that answers has said it.
		if r.outcome == "grown" || (!primary && r.outcome != "failed") {
			break
		}
	}
	note, err := growFSVerdict(resource, volumeID, results, primary, required, rm.fileGatewayRecorded(ctx, resource))
	log.Debug("GrowFilesystem: done", zap.String("resource", resource), zap.String("note", note), zap.Error(err))
	if err == nil && note != "" {
		rm.controller.logger.Info("Filesystem grown with its volume", zap.String("resource", resource),
			zap.Uint32("volume_id", volumeID), zap.String("result", note))
	}
	return note, err
}

// growFSVerdict turns what the nodes said into the reply of the resize.
//
// One node that grew the filesystem settles it. Otherwise the filesystem was
// either none of this function's business — no gateway on any node that
// answered, and nobody asked — or it should have been grown and was not,
// which is an error that names every node's reason. A node that did not
// answer is such a reason only when something says a filesystem was expected:
// the request, another node that has the gateway's config, or (recorded) the
// controller's own record of a file gateway on the resource.
func growFSVerdict(resource string, volumeID uint32, results []growFSResult, primary, required, recorded bool) (string, error) {
	expected := required
	var why []string
	for _, r := range results {
		if r.outcome == "grown" {
			size := ""
			if r.size > 0 {
				size = fmt.Sprintf(" to %s", freeText(r.size))
			}
			return fmt.Sprintf("the %s filesystem mounted at %s on %s was grown%s", r.fstype, r.mount, r.node, size), nil
		}
		if r.gateway != "" && r.gateway != "none" {
			expected = true
		}
		switch r.outcome {
		case "not-needed":
		case "not-mounted":
			if primary {
				why = append(why, fmt.Sprintf("%s is Primary but has no filesystem mounted from %s/%d", r.node, resource, volumeID))
			}
		case "unsupported":
			why = append(why, fmt.Sprintf("the %s filesystem on %s cannot be grown online from here", r.detail, r.node))
		default:
			if recorded {
				expected = true
			}
			why = append(why, fmt.Sprintf("%s: %s", r.node, r.detail))
		}
	}
	if !expected {
		zap.L().Debug("growFSVerdict: no filesystem to grow", zap.String("resource", resource), zap.Int("nodes", len(results)))
		return "", nil
	}
	if !primary {
		why = append(why, resource+" is Primary on no node, so its filesystem is mounted nowhere (a failover is under way, or its gateway is stopped)")
	}
	if len(why) == 0 {
		why = append(why, "no node had it mounted")
	}
	sort.Strings(why)
	return "", fmt.Errorf("the filesystem on %s/%d was not grown: %s", resource, volumeID, strings.Join(why, "; "))
}

// resizeIncomplete is the reply to a resize whose volume grew and whose
// filesystem did not.
func resizeIncomplete(resource string, err error) string {
	return fmt.Sprintf("the volume of %s is grown, but %v. Repeat the resize once the filesystem is mounted again to finish it",
		resource, err)
}

// fileGatewayRecorded reports whether the controller has an NFS or SMB
// gateway on record for resource. The nodes' promoter configs are what
// decides; the record only stands in for a node that did not answer.
func (rm *ResourceManager) fileGatewayRecorded(ctx context.Context, resource string) bool {
	if rm.controller == nil || rm.controller.db == nil {
		return false
	}
	gw, err := rm.controller.db.GetGatewayByResource(ctx, resource)
	recorded := err == nil && gw != nil && (gw.Type == database.GatewayTypeNFS || gw.Type == database.GatewayTypeSMB)
	rm.resourcesLog().Debug("fileGatewayRecorded: read record", zap.String("resource", resource), zap.Bool("recorded", recorded), zap.Error(err))
	return recorded
}
