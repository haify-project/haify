package drbdmodule

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"go.uber.org/zap"
)

// Host is a node as the loader's containers see it. The paths are fields so
// tests can point them at a temporary directory; New fills in the real ones.
type Host struct {
	// ProcDir and SysDir are the kernel's /proc and /sys. A container sees the
	// host's DRBD module there, since modules and block devices are not
	// namespaced.
	ProcDir string
	SysDir  string
	// ModprobeDir is the host's /etc/modprobe.d mounted into the container;
	// empty means prepare writes nothing there.
	ModprobeDir string
	// StateDir is a directory the loader pod's containers share (an emptyDir),
	// where prepare records its decision for bring-up.
	StateDir string
	// HostRoot is the host's root filesystem; with hostPID that is
	// /proc/1/root.
	HostRoot string

	// unload removes one kernel module (delete_module(2) on Linux).
	unload func(name string) error
	// run executes a command and returns its combined output.
	run func(ctx context.Context, name string, args ...string) ([]byte, error)
	log *zap.Logger
}

// New returns a Host on the real /proc and /sys. modprobeDir and stateDir may
// be empty when the command does not use them.
func New(log *zap.Logger, modprobeDir, stateDir string) *Host {
	if log == nil {
		log = zap.L()
	}
	log.Debug("New: host", zap.String("modprobe_dir", modprobeDir), zap.String("state_dir", stateDir))
	return &Host{
		ProcDir:     "/proc",
		SysDir:      "/sys",
		ModprobeDir: modprobeDir,
		StateDir:    stateDir,
		HostRoot:    "/proc/1/root",
		unload:      deleteModule,
		run:         runCommand,
		log:         log,
	}
}

func runCommand(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

// Loaded returns the version of the loaded DRBD module, or nil when DRBD is
// not loaded. /sys/module/drbd/version is read first; /proc/drbd is the
// fallback for a module that does not export its version there.
func (h *Host) Loaded() (*Version, error) {
	modDir := filepath.Join(h.SysDir, "module", "drbd")
	if _, err := os.Stat(modDir); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			h.log.Debug("Loaded: drbd module not loaded", zap.String("path", modDir))
			return nil, nil
		}
		h.log.Debug("Loaded: stat failed", zap.String("path", modDir), zap.Error(err))
		return nil, fmt.Errorf("read %s: %w", modDir, err)
	}
	if b, err := os.ReadFile(filepath.Join(modDir, "version")); err == nil {
		v, perr := ParseVersion(string(b))
		if perr == nil {
			h.log.Debug("Loaded: version from sysfs", zap.String("version", v.String()))
			return &v, nil
		}
		h.log.Debug("Loaded: sysfs version unparsable, trying /proc/drbd", zap.Error(perr))
	}
	b, err := os.ReadFile(filepath.Join(h.ProcDir, "drbd"))
	if err != nil {
		h.log.Debug("Loaded: /proc/drbd unreadable", zap.Error(err))
		return nil, fmt.Errorf("drbd is loaded but its version is unreadable: %w", err)
	}
	v, err := ParseProcDRBD(string(b))
	if err != nil {
		h.log.Debug("Loaded: /proc/drbd unparsable", zap.Error(err))
		return nil, err
	}
	h.log.Debug("Loaded: version from /proc/drbd", zap.String("version", v.String()))
	return &v, nil
}

// Devices lists the DRBD block devices that exist (drbd0, drbd1000, ...).
func (h *Host) Devices() ([]string, error) {
	matches, err := filepath.Glob(filepath.Join(h.SysDir, "block", "drbd*"))
	if err != nil {
		h.log.Debug("Devices: glob failed", zap.Error(err))
		return nil, err
	}
	names := make([]string, 0, len(matches))
	for _, m := range matches {
		names = append(names, filepath.Base(m))
	}
	h.log.Debug("Devices: found", zap.Strings("devices", names))
	return names, nil
}

// Check reports whether a DRBD module of at least want is loaded, with a
// one-line message saying what was found.
func (h *Host) Check(want Version) (string, bool, error) {
	h.log.Debug("Check: start", zap.String("want", want.String()))
	v, err := h.Loaded()
	if err != nil {
		return "", false, err
	}
	if v == nil {
		return fmt.Sprintf("DRBD is not loaded (need %s or newer)", want), false, nil
	}
	if !v.AtLeast(want) {
		return fmt.Sprintf("DRBD %s is loaded, older than the %s Haify needs", v, want), false, nil
	}
	return fmt.Sprintf("DRBD %s is loaded (need %s or newer)", v, want), true, nil
}

// Wait polls until a DRBD module of at least want is loaded or ctx ends. It
// prints what it finds when that changes, and once a minute while it waits,
// so `kubectl logs` shows why a pod is still initialising.
func (h *Host) Wait(ctx context.Context, want Version, interval time.Duration, out func(string)) error {
	h.log.Debug("Wait: start", zap.String("want", want.String()), zap.Duration("interval", interval))
	last := ""
	lastPrinted := time.Time{}
	for {
		msg, ok, err := h.Check(want)
		if err != nil {
			msg = err.Error()
		}
		if ok {
			out(msg)
			h.log.Debug("Wait: done", zap.String("message", msg))
			return nil
		}
		if msg != last || time.Since(lastPrinted) >= time.Minute {
			out("waiting: " + msg)
			last, lastPrinted = msg, time.Now()
		}
		select {
		case <-ctx.Done():
			h.log.Debug("Wait: cancelled", zap.Error(ctx.Err()))
			return fmt.Errorf("gave up waiting for DRBD %s: %w", want, ctx.Err())
		case <-time.After(interval):
		}
	}
}

// stateFile is where prepare records its decision for bring-up.
func (h *Host) stateFile() string { return filepath.Join(h.StateDir, "decision") }

func (h *Host) writeState(d Decision) error {
	if h.StateDir == "" {
		h.log.Debug("writeState: no state dir; skipped", zap.String("decision", d.String()))
		return nil
	}
	err := os.WriteFile(h.stateFile(), []byte(d.String()+"\n"), 0o644)
	h.log.Debug("writeState: wrote", zap.String("path", h.stateFile()), zap.String("decision", d.String()), zap.Error(err))
	return err
}

func (h *Host) readState() (string, error) {
	b, err := os.ReadFile(h.stateFile())
	if err != nil {
		h.log.Debug("readState: unreadable", zap.String("path", h.stateFile()), zap.Error(err))
		return "", err
	}
	s := strings.TrimSpace(string(b))
	h.log.Debug("readState: read", zap.String("path", h.stateFile()), zap.String("decision", s))
	return s, nil
}
