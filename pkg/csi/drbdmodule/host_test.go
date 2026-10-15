package drbdmodule

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// fakeNode is a temporary /proc, /sys, /etc/modprobe.d, state dir and host
// root, with module unloading and command execution recorded instead of run.
type fakeNode struct {
	h        *Host
	unloaded []string
	ran      [][]string
	runErr   error
	// reload makes unloading drbd a no-op, as if something loaded it again.
	reload bool
}

func newFakeNode(t *testing.T) *fakeNode {
	t.Helper()
	root := t.TempDir()
	h := &Host{
		ProcDir:     filepath.Join(root, "proc"),
		SysDir:      filepath.Join(root, "sys"),
		ModprobeDir: filepath.Join(root, "modprobe.d"),
		StateDir:    filepath.Join(root, "state"),
		HostRoot:    filepath.Join(root, "host"),
		log:         zap.NewNop(),
	}
	for _, d := range []string{h.ProcDir, filepath.Join(h.SysDir, "module"), filepath.Join(h.SysDir, "block"), h.ModprobeDir, h.StateDir} {
		require.NoError(t, os.MkdirAll(d, 0o755))
	}
	n := &fakeNode{h: h}
	h.unload = func(name string) error {
		n.unloaded = append(n.unloaded, name)
		if name == "drbd" && n.reload {
			return nil
		}
		return os.RemoveAll(filepath.Join(h.SysDir, "module", name))
	}
	h.run = func(_ context.Context, name string, args ...string) ([]byte, error) {
		n.ran = append(n.ran, append([]string{name}, args...))
		return []byte("ok"), n.runErr
	}
	return n
}

// loadModule makes a module appear loaded, with a sysfs version when given.
func (n *fakeNode) loadModule(t *testing.T, name, version string) {
	t.Helper()
	dir := filepath.Join(n.h.SysDir, "module", name)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	if version != "" {
		require.NoError(t, os.WriteFile(filepath.Join(dir, "version"), []byte(version+"\n"), 0o644))
	}
}

func (n *fakeNode) addDevice(t *testing.T, name string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Join(n.h.SysDir, "block", name), 0o755))
}

func collect() (*[]string, func(string)) {
	var lines []string
	return &lines, func(s string) { lines = append(lines, s) }
}

var min92 = Version{Major: 9, Minor: 2}

func TestLoaded(t *testing.T) {
	n := newFakeNode(t)
	v, err := n.h.Loaded()
	require.NoError(t, err)
	assert.Nil(t, v)

	n.loadModule(t, "drbd", "9.3.4")
	v, err = n.h.Loaded()
	require.NoError(t, err)
	assert.Equal(t, "9.3.4", v.String())

	// No sysfs version: /proc/drbd is read instead.
	require.NoError(t, os.Remove(filepath.Join(n.h.SysDir, "module", "drbd", "version")))
	_, err = n.h.Loaded()
	assert.Error(t, err, "loaded without any readable version")
	require.NoError(t, os.WriteFile(filepath.Join(n.h.ProcDir, "drbd"), []byte("version: 8.4.11 (api:1/proto:86-101)\n"), 0o644))
	v, err = n.h.Loaded()
	require.NoError(t, err)
	assert.Equal(t, "8.4.11", v.String())
}

func TestCheck(t *testing.T) {
	n := newFakeNode(t)
	msg, ok, err := n.h.Check(min92)
	require.NoError(t, err)
	assert.False(t, ok)
	assert.Contains(t, msg, "not loaded")

	n.loadModule(t, "drbd", "8.4.11")
	msg, ok, _ = n.h.Check(min92)
	assert.False(t, ok)
	assert.Contains(t, msg, "8.4.11")

	n.loadModule(t, "drbd", "9.3.4")
	msg, ok, _ = n.h.Check(min92)
	assert.True(t, ok)
	assert.Contains(t, msg, "9.3.4")
}

func TestPrepareReady(t *testing.T) {
	n := newFakeNode(t)
	n.loadModule(t, "drbd", "9.3.4")
	n.addDevice(t, "drbd1000")
	lines, out := collect()
	res, err := n.h.Prepare(context.Background(), min92, out)
	require.NoError(t, err)
	assert.Equal(t, Ready, res.Decision)
	assert.Empty(t, n.unloaded)
	assert.NoFileExists(t, filepath.Join(n.h.ModprobeDir, BlockFileName), "a working node's modprobe config is left alone")
	assert.Contains(t, (*lines)[0], "found DRBD 9.3.4")
	s, err := n.h.readState()
	require.NoError(t, err)
	assert.Equal(t, "ready", s)
}

func TestPrepareLoad(t *testing.T) {
	n := newFakeNode(t)
	_, out := collect()
	res, err := n.h.Prepare(context.Background(), min92, out)
	require.NoError(t, err)
	assert.Equal(t, Load, res.Decision)
	b, err := os.ReadFile(filepath.Join(n.h.ModprobeDir, BlockFileName))
	require.NoError(t, err)
	assert.Contains(t, string(b), "install drbd /bin/false")

	// Running again (the pod restarted) rewrites nothing and still succeeds.
	_, err = n.h.Prepare(context.Background(), min92, out)
	require.NoError(t, err)
}

func TestPrepareWithoutModprobeDir(t *testing.T) {
	n := newFakeNode(t)
	n.h.ModprobeDir = ""
	_, out := collect()
	res, err := n.h.Prepare(context.Background(), min92, out)
	require.NoError(t, err)
	assert.Equal(t, Load, res.Decision)
}

func TestPrepareReplacesUnusedOldModule(t *testing.T) {
	n := newFakeNode(t)
	n.loadModule(t, "drbd", "8.4.11")
	n.loadModule(t, "drbd_transport_tcp", "")
	lines, out := collect()
	res, err := n.h.Prepare(context.Background(), min92, out)
	require.NoError(t, err)
	assert.Equal(t, Replace, res.Decision)
	assert.Equal(t, []string{"drbd_transport_tcp", "drbd"}, n.unloaded, "transports first, absent ones skipped")
	assert.FileExists(t, filepath.Join(n.h.ModprobeDir, BlockFileName))
	assert.Contains(t, *lines, "unloaded drbd")
}

func TestPrepareRefusesOldModuleInUse(t *testing.T) {
	n := newFakeNode(t)
	n.loadModule(t, "drbd", "8.4.11")
	n.addDevice(t, "drbd0")
	_, out := collect()
	res, err := n.h.Prepare(context.Background(), min92, out)
	require.Error(t, err)
	assert.Equal(t, InUse, res.Decision)
	assert.Contains(t, err.Error(), "drbd0")
	assert.Empty(t, n.unloaded)
}

func TestPrepareModuleComesBack(t *testing.T) {
	n := newFakeNode(t)
	n.loadModule(t, "drbd", "8.4.11")
	n.reload = true
	_, out := collect()
	_, err := n.h.Prepare(context.Background(), min92, out)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "still loaded")
}

func TestPrepareUnloadFails(t *testing.T) {
	n := newFakeNode(t)
	n.loadModule(t, "drbd", "8.4.11")
	n.h.unload = func(string) error { return errors.New("resource busy") }
	_, out := collect()
	_, err := n.h.Prepare(context.Background(), min92, out)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "resource busy")
}

func TestBringUp(t *testing.T) {
	unit := func(n *fakeNode) {
		dir := filepath.Join(n.h.HostRoot, "etc", "systemd", "system")
		require.NoError(t, os.MkdirAll(dir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, bootUnit), []byte("[Unit]\n"), 0o644))
	}
	ctx := context.Background()
	_, out := collect()

	t.Run("after a load it restarts the boot unit", func(t *testing.T) {
		n := newFakeNode(t)
		unit(n)
		require.NoError(t, n.h.writeState(Load))
		require.NoError(t, n.h.BringUp(ctx, out))
		require.Len(t, n.ran, 1)
		assert.Equal(t, "nsenter", n.ran[0][0])
		assert.Equal(t, []string{"systemctl", "restart", bootUnit}, n.ran[0][len(n.ran[0])-3:])
	})
	t.Run("after a replace too", func(t *testing.T) {
		n := newFakeNode(t)
		unit(n)
		require.NoError(t, n.h.writeState(Replace))
		require.NoError(t, n.h.BringUp(ctx, out))
		assert.Len(t, n.ran, 1)
	})
	t.Run("not when DRBD was already loaded", func(t *testing.T) {
		n := newFakeNode(t)
		unit(n)
		require.NoError(t, n.h.writeState(Ready))
		require.NoError(t, n.h.BringUp(ctx, out))
		assert.Empty(t, n.ran)
	})
	t.Run("not on a node without the unit", func(t *testing.T) {
		n := newFakeNode(t)
		require.NoError(t, n.h.writeState(Load))
		require.NoError(t, n.h.BringUp(ctx, out))
		assert.Empty(t, n.ran)
	})
	t.Run("reports a failed restart", func(t *testing.T) {
		n := newFakeNode(t)
		unit(n)
		n.runErr = errors.New("exit status 1")
		require.NoError(t, n.h.writeState(Load))
		assert.Error(t, n.h.BringUp(ctx, out))
	})
	t.Run("needs the prepare decision", func(t *testing.T) {
		n := newFakeNode(t)
		assert.Error(t, n.h.BringUp(ctx, out))
	})
}

func TestWait(t *testing.T) {
	n := newFakeNode(t)
	go func() {
		// Not loadModule: require must not be called off the test goroutine.
		time.Sleep(30 * time.Millisecond)
		dir := filepath.Join(n.h.SysDir, "module", "drbd")
		_ = os.MkdirAll(dir, 0o755)
		_ = os.WriteFile(filepath.Join(dir, "version.tmp"), []byte("9.3.4\n"), 0o644)
		_ = os.Rename(filepath.Join(dir, "version.tmp"), filepath.Join(dir, "version"))
	}()
	lines, out := collect()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, n.h.Wait(ctx, min92, 5*time.Millisecond, out))
	assert.Contains(t, (*lines)[0], "waiting: DRBD is not loaded")
	assert.Contains(t, (*lines)[len(*lines)-1], "DRBD 9.3.4 is loaded")

	n2 := newFakeNode(t)
	ctx2, cancel2 := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel2()
	assert.Error(t, n2.h.Wait(ctx2, min92, 5*time.Millisecond, out))
}
