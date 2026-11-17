package controller

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/haify-project/haify/pkg/event"
)

// Showing a running libvirt guest its grown disk.
//
// Growing a volume grows the backing volumes and the DRBD device. QEMU keeps
// the size it read when it opened the disk, so a running guest goes on seeing
// the old one until QEMU is told (`virsh blockresize`) or the guest is
// restarted. For a resource that carries the libvirt guest label the
// controller tells it, on the node the guest runs on.
//
// Only libvirt guests are told here. A Proxmox VE volume carries
// haify.pve/managed-by and no libvirt label: PVE resizes the volume through
// this same API and then sends QEMU its own block_resize. Where another
// manager of libvirt guests (OpenNebula, Nova) sends one too, the two agree:
// blockresize to the size the device already has changes nothing.

// guestResizeTimeout bounds the script that tells the guest. It waits up to
// guestResizeSettle for the Primary to see the new device size first.
const (
	guestResizeSettle  = 15 * time.Second
	guestResizeTimeout = 45 * time.Second
)

// guestResizeScript finds the guest's disk that is this volume and resizes it
// to the size the DRBD device has on this node.
//
// The disk is found by its source, never by position: a guest can have any
// number of disks in any order. Both sides are resolved to the device node
// first, so a disk defined on /dev/drbdN or /dev/drbd/by-disk/... is found as
// well as one on /dev/drbd/by-res/<resource>/<volume>.
//
// The size is read from the device rather than computed by the controller.
// QEMU refuses a size larger than the host device, and a moment after
// `drbdadm resize` returned on one node the Primary may not have the new size
// yet, so the script waits for at least @WANT@ bytes before it reads it.
const guestResizeScript = `#!/bin/bash
cd /
export LC_ALL=C
dom='@DOM@'
src='@SRC@'
want=@WANT@
say() { echo "guest=$1"; [ -z "$2" ] || echo "detail=$(echo "$2" | tr '\n' ' ')"; exit 0; }
command -v virsh >/dev/null 2>&1 || say no-virsh
state=$(virsh domstate "$dom" 2>&1) || say undefined "$state"
state=$(echo $state)
case "$state" in running|paused) ;; *) say not-running "$state" ;; esac
real=$(readlink -f "$src" 2>/dev/null)
[ -n "$real" ] && [ -b "$real" ] || say failed "$src is not a block device on $(hostname)"
target=
while read -r _type _device tgt source; do
  [ -n "$source" ] && [ "$source" != "-" ] || continue
  if [ "$(readlink -f "$source" 2>/dev/null)" = "$real" ]; then target=$tgt; break; fi
done < <(virsh domblklist "$dom" --details 2>/dev/null | tail -n +3)
[ -n "$target" ] || say no-disk
i=0
bytes=$(blockdev --getsize64 "$real" 2>/dev/null)
while [ "${bytes:-0}" -lt "$want" ] && [ $i -lt @SETTLE@ ]; do
  sleep 1; i=$((i+1))
  bytes=$(blockdev --getsize64 "$real" 2>/dev/null)
done
echo "target=$target"
[ "${bytes:-0}" -gt 0 ] || say failed "could not read the size of $src"
echo "bytes=$bytes"
out=$(virsh blockresize "$dom" "$target" "${bytes}B" 2>&1) || say failed "$out"
say resized
`

// TellGuestOfResize shows the libvirt guest of resource the new size of one
// of its volumes, after the volume was grown to at least wantBytes. It
// returns a sentence for the reply of the resize, "" for a resource that
// belongs to no guest.
//
// It returns no error on purpose: the volume is already grown, and nothing
// that goes wrong here changes that. A running guest that could not be told
// is reported in the sentence and raised as an event, because someone then
// has to run blockresize by hand or restart the guest.
func (rm *ResourceManager) TellGuestOfResize(ctx context.Context, resource string, volumeID uint32, wantBytes uint64) string {
	log := rm.resourcesLog()
	log.Debug("TellGuestOfResize: start", zap.String("resource", resource), zap.Uint32("volume_id", volumeID),
		zap.Uint64("want_bytes", wantBytes))
	domain := rm.guestDomain(ctx, resource)
	if domain == "" {
		log.Debug("TellGuestOfResize: not a guest's resource; nothing to tell", zap.String("resource", resource))
		return ""
	}
	later := fmt.Sprintf("guest %s sees the new size when it next starts", domain)
	hosts := rm.guestHosts(ctx, resource)
	if len(hosts) == 0 {
		log.Debug("TellGuestOfResize: resource is Primary nowhere; guest not running", zap.String("resource", resource),
			zap.String("domain", domain))
		return fmt.Sprintf("guest %s is not running; it sees the new size when it next starts", domain)
	}
	src := fmt.Sprintf("/dev/drbd/by-res/%s/%d", resource, volumeID)
	script := strings.NewReplacer(
		"@DOM@", domain,
		"@SRC@", src,
		"@WANT@", strconv.FormatUint(wantBytes, 10),
		"@SETTLE@", strconv.Itoa(int(guestResizeSettle.Seconds())),
	).Replace(guestResizeScript)

	// A guest in the middle of a live migration is Primary on two hosts and
	// runs on one of them; the first that resizes it settles the matter.
	var notes []string
	for _, host := range hosts {
		node := rm.nodeLabel(host)
		kv, err := rm.runGuestScript(ctx, host, script, guestResizeTimeout)
		outcome := kv["guest"]
		log.Debug("TellGuestOfResize: host answered", zap.String("resource", resource), zap.String("domain", domain),
			zap.String("host", host), zap.String("outcome", outcome), zap.String("target", kv["target"]),
			zap.String("bytes", kv["bytes"]), zap.Error(err))
		byHand := fmt.Sprintf("run `virsh blockresize %s %s <size>` on %s, or restart the guest", domain, src, node)
		switch outcome {
		case "resized":
			rm.controller.logger.Info("Guest shown its grown disk", zap.String("resource", resource), zap.String("domain", domain),
				zap.String("node", node), zap.String("target", kv["target"]), zap.String("bytes", kv["bytes"]))
			return fmt.Sprintf("guest %s on %s now sees the new size on %s", domain, node, kv["target"])
		case "not-running":
			notes = append(notes, fmt.Sprintf("guest %s is not running on %s; it sees the new size when it next starts", domain, node))
		case "no-virsh":
			notes = append(notes, fmt.Sprintf("%s has no virsh, so guest %s was not told; %s", node, domain, later))
		case "undefined":
			notes = append(notes, fmt.Sprintf("%s has no guest named %s (label %s), so no guest was told the new size",
				node, domain, libvirtDomainLabel))
		case "no-disk":
			msg := fmt.Sprintf("guest %s on %s has no disk on %s, so it was not told the new size; %s", domain, node, src, byHand)
			rm.guestResizeFailed(resource, host, domain, msg)
			notes = append(notes, msg)
		default:
			// "failed", or a script that did not run to its end.
			why := kv["detail"]
			if why == "" && err != nil {
				why = err.Error()
			}
			if why == "" {
				why = "the node did not answer"
			}
			msg := fmt.Sprintf("guest %s on %s was not told the new size (%s); %s", domain, node, why, byHand)
			rm.guestResizeFailed(resource, host, domain, msg)
			notes = append(notes, msg)
		}
	}
	return strings.Join(notes, "; ")
}

// guestResizeFailed records that a running guest still sees the old size.
func (rm *ResourceManager) guestResizeFailed(resource, host, domain, msg string) {
	rm.controller.logger.Warn("Volume resized, but its running guest was not shown the new size",
		zap.String("resource", resource), zap.String("domain", domain), zap.String("node", rm.nodeLabel(host)),
		zap.String("reason", msg))
	rm.publishGuestEvent(event.TypeGuestResizeFailed, event.SeverityWarning, resource, host, domain,
		"The volume was resized, but "+msg)
}

// resizedMessage is the reply to a successful resize: what happened to the
// volume, then what happened to its guest.
func resizedMessage(guestNote string) string {
	if guestNote == "" {
		return "Volume resized successfully"
	}
	return "Volume resized successfully; " + guestNote
}
