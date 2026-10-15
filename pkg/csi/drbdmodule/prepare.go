package drbdmodule

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.uber.org/zap"
)

// BlockFileName is the file prepare writes into the host's /etc/modprobe.d.
const BlockFileName = "haify-drbd-loader.conf"

// blockFileContent stops modprobe from loading any drbd module from the
// host's disk. drbdsetup runs `modprobe drbd` whenever /proc/drbd is missing,
// and drbd-reactor runs drbdsetup every two seconds until it succeeds, so on a
// distribution that ships DRBD 8.4 in its kernel that module is loaded long
// before the loader has compiled DRBD 9, and loaded again within seconds of
// being removed. The loader inserts its module with insmod, which this file
// does not affect.
const blockFileContent = `# Written by the Haify CSI chart's DRBD module loader (drbdModuleLoader).
# DRBD on this node is loaded by the haify-drbd-loader DaemonSet. This line
# stops modprobe from loading any other drbd module first, such as the DRBD 8.4
# some distributions ship in their kernel: drbdadm and drbdsetup run
# "modprobe drbd" whenever DRBD is not loaded. Delete this file if the node
# gets DRBD from a package such as drbd-dkms instead.
install drbd /bin/false
`

// transportModules are unloaded before drbd itself, which they depend on.
// Kernel module names use underscores (drbd_transport_lb-tcp.ko registers as
// drbd_transport_lb_tcp).
var transportModules = []string{"drbd_transport_rdma", "drbd_transport_lb_tcp", "drbd_transport_tcp"}

// PrepareResult says what prepare found and did.
type PrepareResult struct {
	Decision Decision
	// Found is the module that was loaded when prepare started; nil if none.
	Found *Version
}

// Prepare gets the node ready for the loader container that follows it:
//
//   - a module of at least want is loaded: nothing to do;
//   - no module is loaded: block the host's own drbd module, so nothing loads
//     an old one while the loader compiles;
//   - an older module is loaded with no devices: block, then unload it;
//   - an older module with devices: fail, since unloading it would take that
//     storage away.
//
// The decision is recorded in StateDir for bring-up. Messages for the person
// reading `kubectl logs` go to out.
func (h *Host) Prepare(ctx context.Context, want Version, out func(string)) (PrepareResult, error) {
	h.log.Debug("Prepare: start", zap.String("want", want.String()), zap.String("modprobe_dir", h.ModprobeDir))
	loaded, err := h.Loaded()
	if err != nil {
		return PrepareResult{}, err
	}
	devices, err := h.Devices()
	if err != nil {
		return PrepareResult{}, fmt.Errorf("list DRBD devices: %w", err)
	}
	res := PrepareResult{Found: loaded, Decision: Decide(loaded, want, len(devices))}
	if loaded == nil {
		out(fmt.Sprintf("DRBD is not loaded; the loader will load it (need %s or newer)", want))
	} else {
		out(fmt.Sprintf("found DRBD %s loaded, %d DRBD device(s); need %s or newer", loaded, len(devices), want))
	}

	switch res.Decision {
	case Ready:
		out("nothing to load")
	case InUse:
		h.log.Debug("Prepare: old module in use; refusing", zap.String("found", loaded.String()), zap.Strings("devices", devices))
		return res, fmt.Errorf("DRBD %s is loaded and has devices (%s); it cannot be replaced while they exist. "+
			"Take its resources down or reboot the node, then let the loader run again",
			loaded, strings.Join(devices, ", "))
	case Load:
		if err := h.writeBlockFile(out); err != nil {
			return res, err
		}
	case Replace:
		if err := h.writeBlockFile(out); err != nil {
			return res, err
		}
		if err := h.unloadDRBD(ctx, out); err != nil {
			return res, err
		}
	}
	if err := h.writeState(res.Decision); err != nil {
		return res, fmt.Errorf("record decision in %s: %w", h.StateDir, err)
	}
	h.log.Debug("Prepare: done", zap.String("decision", res.Decision.String()))
	return res, nil
}

// writeBlockFile writes BlockFileName into ModprobeDir, unless that is unset
// or the file already says the same thing.
func (h *Host) writeBlockFile(out func(string)) error {
	if h.ModprobeDir == "" {
		h.log.Debug("writeBlockFile: no modprobe dir; skipped")
		return nil
	}
	path := filepath.Join(h.ModprobeDir, BlockFileName)
	if b, err := os.ReadFile(path); err == nil && string(b) == blockFileContent {
		h.log.Debug("writeBlockFile: already in place", zap.String("path", path))
		return nil
	}
	if err := os.WriteFile(path, []byte(blockFileContent), 0o644); err != nil {
		h.log.Debug("writeBlockFile: write failed", zap.String("path", path), zap.Error(err))
		return fmt.Errorf("write %s: %w", path, err)
	}
	out(fmt.Sprintf("wrote /etc/modprobe.d/%s: only the loader loads DRBD on this node now", BlockFileName))
	h.log.Debug("writeBlockFile: wrote", zap.String("path", path))
	return nil
}

// unloadDRBD removes the transports and then drbd. A transport that is not
// loaded is skipped. drbd still being there afterwards means it is built
// into the kernel or something loaded it again in between.
func (h *Host) unloadDRBD(ctx context.Context, out func(string)) error {
	for _, name := range append(append([]string{}, transportModules...), "drbd") {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := os.Stat(filepath.Join(h.SysDir, "module", name)); errors.Is(err, fs.ErrNotExist) {
			h.log.Debug("unloadDRBD: not loaded; skipped", zap.String("module", name))
			continue
		}
		start := time.Now()
		err := h.unload(name)
		h.log.Debug("unloadDRBD: delete_module", zap.String("module", name), zap.Duration("took", time.Since(start)), zap.Error(err))
		if err != nil {
			return fmt.Errorf("unload %s: %w", name, err)
		}
		out("unloaded " + name)
	}
	v, err := h.Loaded()
	if err != nil {
		return err
	}
	if v != nil {
		h.log.Debug("unloadDRBD: drbd still loaded", zap.String("version", v.String()))
		return fmt.Errorf("DRBD %s is still loaded after unloading it (built into the kernel, or loaded again)", v)
	}
	return nil
}

// bootUnit is the unit Haify installs on every node that holds a resource
// (pkg/controller/resource_boot_unit.go). It activates LVM, opens LUKS
// containers and runs `drbdadm adjust` on each resource.
const bootUnit = "haify-drbd-up.service"

// BringUp runs the node's Haify boot bring-up again after the loader has
// loaded DRBD. At boot that unit ran before Kubernetes started the loader, so
// every `drbdadm adjust` in it failed and the node rejoined no resource; this
// is its second chance. It runs only when prepare loaded the module in this
// pod, never when DRBD was already there, so restarting the loader pod on a
// working node changes nothing.
func (h *Host) BringUp(ctx context.Context, out func(string)) error {
	h.log.Debug("BringUp: start", zap.String("host_root", h.HostRoot))
	decision, err := h.readState()
	if err != nil {
		return fmt.Errorf("read the prepare decision: %w", err)
	}
	if decision != Load.String() && decision != Replace.String() {
		out("DRBD was already loaded; resources need no bring-up")
		h.log.Debug("BringUp: skipped", zap.String("decision", decision))
		return nil
	}
	unitPath := filepath.Join(h.HostRoot, "etc", "systemd", "system", bootUnit)
	if _, err := os.Stat(unitPath); err != nil {
		out("this node has no " + bootUnit + " yet (no Haify resource has been placed on it); nothing to bring up")
		h.log.Debug("BringUp: no boot unit", zap.String("path", unitPath), zap.Error(err))
		return nil
	}
	args := []string{"--target", "1", "--mount", "--uts", "--ipc", "--net", "--pid", "--",
		"systemctl", "restart", bootUnit}
	start := time.Now()
	b, err := h.run(ctx, "nsenter", args...)
	h.log.Debug("BringUp: systemctl restart", zap.String("unit", bootUnit), zap.Duration("took", time.Since(start)),
		zap.String("output", strings.TrimSpace(string(b))), zap.Error(err))
	if err != nil {
		return fmt.Errorf("systemctl restart %s on the host: %w: %s", bootUnit, err, strings.TrimSpace(string(b)))
	}
	out("ran " + bootUnit + ": this node's DRBD resources are up again")
	return nil
}
