package csi

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"go.uber.org/zap"
	"golang.org/x/sys/unix"

	"github.com/haify-project/haify/pkg/util"
)

// I/O limits for a volume (StorageClass parameters readBytesPerSecond,
// writeBytesPerSecond, readIOPS, writeIOPS).
//
// A pod's I/O to its volume is submitted from the pod's own cgroup, so the
// cgroup v2 io controller can limit it: the node plugin writes
// "<major>:<minor> rbps=… wbps=… riops=… wiops=…" for the volume's DRBD
// device into the pod-level cgroup's io.max when it publishes the volume.
// That limits the pod, every container in it, on that one device. A
// VolumeAttributesClass can change the limits later; the node plugin then
// rewrites io.max on its next pass (qos_reconcile.go).
//
// It needs cgroup v2 with the io controller enabled for the pod cgroups, and
// the host's /sys/fs/cgroup in the node plugin (deploy/k8s/30-node.yaml).
// Without them the volume is still published — a missing limit is not a
// reason to keep a pod from its data — and the node plugin logs why. Gateway
// exports (NFS, iSCSI, NVMe-oF) cannot be limited this way: their I/O comes
// from kernel threads in the root cgroup, which has no io.max.

var qosParams = map[string]string{
	"readBytesPerSecond":  "rbps",
	"writeBytesPerSecond": "wbps",
	"readIOPS":            "riops",
	"writeIOPS":           "wiops",
}

// parseQoS validates the I/O limit parameters and returns them as io.max
// keys; byte rates take sizes ("200Mi", "1G").
func parseQoS(p map[string]string) (map[string]string, error) {
	out := map[string]string{}
	for param, key := range qosParams {
		raw := strings.TrimSpace(p[param])
		if raw == "" {
			continue
		}
		var n uint64
		var err error
		if strings.HasSuffix(key, "bps") {
			// Kubernetes quantities ("100Mi") as well as haify sizes ("100MiB").
			if strings.HasSuffix(raw, "i") {
				raw += "B"
			}
			n, err = util.ParseSize(raw)
		} else {
			n, err = strconv.ParseUint(raw, 10, 64)
		}
		if err != nil || n == 0 {
			zap.L().Debug("parseQoS: invalid limit", zap.String("param", param), zap.String("value", raw), zap.Error(err))
			return nil, fmt.Errorf("invalid %s %q", param, raw)
		}
		zap.L().Debug("parseQoS: limit", zap.String("param", param), zap.Uint64("value", n))
		out[param] = strconv.FormatUint(n, 10)
	}
	return out, nil
}

// cgroupRoot is the cgroup v2 mount; a variable so tests can point it at a
// fixture tree.
var cgroupRoot = "/sys/fs/cgroup"

var podUIDRE = regexp.MustCompile(`/pods/([0-9a-fA-F-]{36})/`)

// writeIOMax writes "<major>:<minor> <limits>" for device into the io.max of
// the pod whose publish target this is.
func writeIOMax(device, target, limits string) error {
	log := zap.L()
	m := podUIDRE.FindStringSubmatch(target)
	if m == nil {
		log.Debug("writeIOMax: no pod UID in target", zap.String("target", target))
		return fmt.Errorf("no pod UID in the target path")
	}
	cg, err := podCgroup(m[1])
	if err != nil {
		log.Debug("writeIOMax: pod cgroup not found", zap.String("pod_uid", m[1]), zap.Error(err))
		return err
	}
	var st unix.Stat_t
	if err := unix.Stat(device, &st); err != nil {
		log.Debug("writeIOMax: stat device failed", zap.String("device", device), zap.Error(err))
		return fmt.Errorf("stat %s: %w", device, err)
	}
	line := fmt.Sprintf("%d:%d %s", unix.Major(uint64(st.Rdev)), unix.Minor(uint64(st.Rdev)), limits)
	path := filepath.Join(cg, "io.max")
	err = os.WriteFile(path, []byte(line), 0o644)
	log.Debug("writeIOMax: wrote io.max", zap.String("path", path), zap.String("line", line), zap.Error(err))
	if err != nil {
		return fmt.Errorf("write io.max of %s (is the io controller enabled for pod cgroups?): %w", cg, err)
	}
	return nil
}

// podCgroup finds the pod-level cgroup of pod uid: "pod<uid>" with the
// cgroupfs driver, "kubepods-…-pod<uid with _>.slice" with the systemd one.
func podCgroup(uid string) (string, error) {
	want := []string{"pod" + uid, "pod" + strings.ReplaceAll(uid, "-", "_") + ".slice"}
	var found string
	err := filepath.WalkDir(cgroupRoot, func(path string, d fs.DirEntry, err error) error {
		switch {
		case found != "":
			return fs.SkipAll
		case err != nil:
			return nil // an unreadable branch is not where the pod is
		case !d.IsDir():
			return nil
		}
		if strings.Count(strings.TrimPrefix(path, cgroupRoot), "/") > 4 {
			return fs.SkipDir
		}
		for _, w := range want {
			if strings.HasSuffix(path, "/"+w) || strings.HasSuffix(path, "-"+w) {
				found = path
				return fs.SkipAll
			}
		}
		return nil
	})
	if err != nil && found == "" {
		zap.L().Debug("podCgroup: walk failed", zap.String("pod_uid", uid), zap.String("root", cgroupRoot), zap.Error(err))
		return "", err
	}
	if found == "" {
		zap.L().Debug("podCgroup: not found", zap.String("pod_uid", uid), zap.String("root", cgroupRoot))
		return "", fmt.Errorf("no cgroup for pod %s under %s", uid, cgroupRoot)
	}
	zap.L().Debug("podCgroup: found", zap.String("pod_uid", uid), zap.String("cgroup", found))
	return found, nil
}
