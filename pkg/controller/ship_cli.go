package controller

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"go.uber.org/zap"
)

// A ship runs `haify ship send|recv` on a storage node, so that node needs a
// haify CLI with the ship command. How the node was set up decides where one
// lives: the Debian package installs /usr/bin/haify, a manual install or
// scripts/deploy.sh /usr/local/bin/haify, and a node added any other way may
// have none. ensureHaifyCLI uses whichever the node has, and otherwise
// installs this controller's own copy at haifyCLIInstallPath, the way
// ensureServiceIP installs service-ip.

// haifyCLIInstallPath is where the controller installs a haify CLI. It is
// under /opt/haify, which belongs to Haify, so it never overwrites a packaged
// /usr/bin/haify or an administrator's /usr/local/bin/haify, and it is not on
// PATH ahead of either.
const haifyCLIInstallPath = "/opt/haify/bin/haify"

// haifyCLINodePaths are the places a node's haify is looked for, in order of
// preference: an administrator's copy first, as PATH would pick it.
var haifyCLINodePaths = []string{"/usr/local/bin/haify", "/usr/bin/haify", haifyCLIInstallPath}

// haifyCLIProbe prints "found <path>" for the first haify on the node that has
// the ship command, else "missing <uname -m>".
func haifyCLIProbe() string {
	return fmt.Sprintf(`for p in %s; do
  [ -x "$p" ] && "$p" ship --help >/dev/null 2>&1 && { echo "found $p"; exit 0; }
done
echo "missing $(uname -m)"
`, strings.Join(haifyCLINodePaths, " "))
}

// haifyCLISources lists where this controller looks for a haify to install on
// a node of architecture goarch, given the directory its own executable is
// in. A node of the controller's architecture can take the CLI installed next
// to the controller (/opt/haify/bin), by make install-cli or deploy.sh
// (/usr/local/bin) or by the package (/usr/bin). Any other architecture needs
// a build for it placed next to the controller as haify-<goarch>.
func haifyCLISources(exeDir, goarch string) []string {
	if goarch != runtime.GOARCH {
		return []string{filepath.Join(exeDir, "haify-"+goarch)}
	}
	return []string{filepath.Join(exeDir, "haify"), "/usr/local/bin/haify", "/usr/bin/haify"}
}

// isExecutableFile reports whether p is a regular file with an execute bit.
func isExecutableFile(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.Mode().IsRegular() && fi.Mode().Perm()&0o111 != 0
}

// firstExecutable returns the first path that is a regular executable file.
func firstExecutable(paths []string) string {
	for _, p := range paths {
		if isExecutableFile(p) {
			return p
		}
	}
	return ""
}

// parseHaifyCLIProbe splits the probe's answer into the path found, or the
// node's machine when none was.
func parseHaifyCLIProbe(out string) (path, machine string, err error) {
	kind, val, _ := strings.Cut(strings.TrimSpace(out), " ")
	val = strings.TrimSpace(val)
	switch {
	case kind == "found" && val != "":
		return val, "", nil
	case kind == "missing" && val != "":
		return "", val, nil
	}
	return "", "", fmt.Errorf("unexpected answer %q", strings.TrimSpace(out))
}

// probeHaifyCLI runs haifyCLIProbe on host.
func (rm *ResourceManager) probeHaifyCLI(ctx context.Context, host string) (string, string, error) {
	res, err := rm.deployment.Exec(ctx, []string{host}, "echo "+base64Std(haifyCLIProbe())+" | base64 -d | /bin/sh")
	if err != nil {
		return "", "", fmt.Errorf("look for haify on %s: %w", rm.nodeLabel(host), err)
	}
	hr := res.Hosts[host]
	if hr == nil || !hr.Success {
		return "", "", fmt.Errorf("look for haify on %s: %s", rm.nodeLabel(host), hostFailure(hr))
	}
	path, machine, err := parseHaifyCLIProbe(hr.Output)
	if err != nil {
		return "", "", fmt.Errorf("look for haify on %s: %w", rm.nodeLabel(host), err)
	}
	return path, machine, nil
}

// ensureHaifyCLI returns the path of a haify on host that can run a ship,
// installing this controller's copy at haifyCLIInstallPath when the node has
// none. A node of another architecture gets only a build made for it.
func (rm *ResourceManager) ensureHaifyCLI(ctx context.Context, host string) (string, error) {
	path, machine, err := rm.probeHaifyCLI(ctx, host)
	if err != nil || path != "" {
		return path, err
	}

	node := rm.nodeLabel(host)
	exe, err := controllerExecutable()
	if err != nil {
		return "", fmt.Errorf("haify is not installed on %s, and this controller cannot locate itself to find a copy: %w", node, err)
	}
	goarch := goarchFromMachine(machine)
	candidates := haifyCLISources(filepath.Dir(exe), goarch)
	src := firstExecutable(candidates)
	if src == "" {
		return "", fmt.Errorf("no haify that can ship is installed on %s (%s), and this controller has none built for it "+
			"(looked in %s); install a current haify at /usr/local/bin/haify on %s, or place a %s build at %s",
			node, machine, strings.Join(candidates, ", "), node, machine, candidates[0])
	}

	rm.controller.logger.Info("Installing haify CLI for ship",
		zap.String("node", node), zap.String("source", src), zap.String("target", haifyCLIInstallPath))
	inst, err := rm.deployment.InstallFile(ctx, []string{host}, src, haifyCLIInstallPath, 0o755)
	if err != nil {
		return "", fmt.Errorf("install haify on %s: %w", node, err)
	}
	if !inst.Success {
		return "", fmt.Errorf("install haify on %s: %s", node, hostFailure(inst.Hosts[host]))
	}
	if path, _, err = rm.probeHaifyCLI(ctx, host); err != nil {
		return "", err
	}
	if path == "" {
		return "", fmt.Errorf("the haify installed on %s from %s cannot ship; update %s on this controller's host", node, src, src)
	}
	return path, nil
}
