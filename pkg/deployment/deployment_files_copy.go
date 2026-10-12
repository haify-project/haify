package deployment

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/liliang-cn/dispatch/pkg/dispatch"
	"go.uber.org/zap"
)

// Files that travel over SFTP rather than on a command line: secrets, which
// must never appear in one, and binaries, which do not fit in one.

// DistributeSecret writes content to relPath — resolved against the SSH login
// user's home directory — at mode 0600, WITHOUT the content ever appearing in a
// command line.
//
// DistributeConfig cannot be used for secrets. It base64-encodes the payload
// into the remote command string, so the bytes are visible in `ps` on the node
// for the duration of the copy and in anything that logs commands along the
// way. That is harmless for a DRBD config and disqualifying for an object-store
// secret key. This path hands the content to the scp protocol over the existing
// SSH session instead, where it travels in the encrypted stream and the mode is
// set by the receiving end before the file has any content.
//
// relPath is relative on purpose: the copy runs as the login user, who cannot
// necessarily write /etc or /run, and a 0600 file in that user's own home needs
// no privilege at all. Callers are expected to have created the parent
// directory 0700 first, so the file is private even for the instant between
// create and chmod.
func (c *Client) DistributeSecret(ctx context.Context, hosts []string, content, relPath string) (*ConfigResult, error) {
	log := c.log()
	start := time.Now()
	// Only the size of the secret is traced, never its bytes.
	log.Debug("DistributeSecret: distributing secret file",
		zap.Strings("hosts", hosts), zap.String("path", relPath), zap.Int("size_bytes", len(content)))
	if filepath.IsAbs(relPath) {
		log.Debug("DistributeSecret: refusing an absolute path", zap.String("path", relPath))
		return nil, fmt.Errorf("DistributeSecret: %q must be relative to the login user's home", relPath)
	}

	result := &ConfigResult{Path: relPath, Success: true, Hosts: make(map[string]*HostResult)}

	// Stage the payload in a private directory on the controller. A 0600 file
	// inside a 0700 directory is never readable by another local user, not even
	// between os.MkdirTemp and os.WriteFile.
	dir, err := os.MkdirTemp("", "haify-secret-")
	if err != nil {
		log.Debug("DistributeSecret: cannot create staging directory", zap.Error(err))
		return nil, fmt.Errorf("DistributeSecret: stage secret: %w", err)
	}
	// This is the one cleanup in this file that is worth a word if it fails: the
	// staged file is the secret itself, and leaving it on the controller's disk
	// defeats the point of taking the scp path in the first place. Nothing can
	// be done about it from here — the caller's operation may well have
	// succeeded — so it is logged rather than returned, loudly enough that an
	// operator can go and shred it.
	defer func() {
		if err := os.RemoveAll(dir); err != nil {
			c.logger.Warn("failed to remove staged secret from the controller; remove it by hand",
				zap.String("dir", dir), zap.Error(err))
		}
	}()
	local := filepath.Join(dir, filepath.Base(relPath))
	if err := os.WriteFile(local, []byte(content), 0600); err != nil {
		log.Debug("DistributeSecret: cannot write staged file", zap.String("dir", dir), zap.Error(err))
		return nil, fmt.Errorf("DistributeSecret: stage secret: %w", err)
	}

	var localHosts, remoteHosts []string
	localAddrs := getLocalIPs()
	for _, host := range hosts {
		if isLocalIP(host, localAddrs) {
			localHosts = append(localHosts, host)
		} else {
			remoteHosts = append(remoteHosts, host)
		}
	}
	log.Debug("DistributeSecret: hosts classified",
		zap.Strings("local", localHosts), zap.Strings("remote", remoteHosts))

	// A node that is this machine skips SSH entirely; $HOME is the controller
	// process's own, which is the same account the local Exec branch runs as.
	for _, host := range localHosts {
		hr := &HostResult{Host: host, Success: true}
		home, err := os.UserHomeDir()
		if err == nil {
			dest := filepath.Join(home, relPath)
			if err = os.MkdirAll(filepath.Dir(dest), 0700); err == nil {
				err = os.WriteFile(dest, []byte(content), 0600)
			}
		}
		if err != nil {
			hr.Success, hr.Error = false, err
			result.Success = false
		}
		log.Debug("DistributeSecret: wrote on local host",
			zap.String("host", host), zap.Bool("success", hr.Success), zap.Error(err))
		result.Hosts[host] = hr
	}

	if len(remoteHosts) > 0 {
		copyStart := time.Now()
		copyResult, err := copyWithin(ctx, secretCopyTimeout, func() (*dispatch.CopyResult, error) {
			return c.dispatch.Copy(ctx, remoteHosts, local, relPath, dispatch.WithCopyMode(0600))
		})
		if err != nil {
			log.Debug("DistributeSecret: copy errored",
				zap.Strings("hosts", remoteHosts), zap.Duration("took", time.Since(copyStart)), zap.Error(err))
			return nil, fmt.Errorf("DistributeSecret: copy to %v: %w", remoteHosts, err)
		}
		log.Debug("DistributeSecret: copy returned",
			zap.Strings("hosts", remoteHosts), zap.Duration("took", time.Since(copyStart)))
		for _, host := range remoteHosts {
			hr := &HostResult{Host: host, Success: false, Error: fmt.Errorf("no result for host")}
			if r := copyResult.Hosts[host]; r != nil {
				hr.Success, hr.Error = r.Success, r.Error
			}
			if !hr.Success {
				result.Success = false
			}
			log.Debug("DistributeSecret: remote host result",
				zap.String("host", host), zap.Bool("success", hr.Success), zap.NamedError("host_error", hr.Error))
			result.Hosts[host] = hr
		}
	}

	c.logger.Debug("Secret distributed",
		zap.Strings("hosts", hosts), zap.String("path", relPath), zap.Bool("success", result.Success))
	log.Debug("DistributeSecret: done", zap.String("path", relPath), zap.Duration("took", time.Since(start)))
	return result, nil
}

// secretCopyTimeout bounds copying a secret file: a few hundred bytes, so
// anything longer is a transfer that is not coming back.
const secretCopyTimeout = 2 * time.Minute

// copyWithin runs a dispatch copy but gives up after d or when ctx ends.
//
// dispatch's Copy stops watching its context once the per-host transfers have
// started and then waits for all of them, so one SFTP session on a stalled
// connection blocks its caller forever. Seen on a scheduled backup to a node
// whose network had paused: the run never returned, and the schedule's
// one-run-at-a-time guard then skipped every later tick. The abandoned copy
// finishes or dies with its connection; its result is discarded.
func copyWithin(ctx context.Context, d time.Duration, copyFn func() (*dispatch.CopyResult, error)) (*dispatch.CopyResult, error) {
	zap.L().Debug("copyWithin: starting bounded copy", zap.Duration("timeout", d))
	type outcome struct {
		res *dispatch.CopyResult
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		res, err := copyFn()
		done <- outcome{res, err}
	}()
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case o := <-done:
		zap.L().Debug("copyWithin: copy finished", zap.Error(o.err))
		return o.res, o.err
	case <-timer.C:
		zap.L().Debug("copyWithin: copy timed out, abandoning it", zap.Duration("timeout", d))
		return nil, fmt.Errorf("copy did not finish within %s", d)
	case <-ctx.Done():
		zap.L().Debug("copyWithin: context ended before the copy", zap.Error(ctx.Err()))
		return nil, ctx.Err()
	}
}

// InstallFile installs a local file — typically a binary — at an absolute
// path on every host, owned by root at the given mode. DistributeConfig cannot
// carry it: that path puts the content on a command line, which an executable
// of several megabytes does not fit on. The file travels by SFTP into the login
// user's home instead and is moved into place with sudo install.
func (c *Client) InstallFile(ctx context.Context, hosts []string, localPath, remotePath string, mode os.FileMode) (*ConfigResult, error) {
	log := c.log()
	start := time.Now()
	log.Debug("InstallFile: installing file",
		zap.Strings("hosts", hosts), zap.String("local_path", localPath),
		zap.String("path", remotePath), zap.Stringer("mode", mode))
	if !filepath.IsAbs(remotePath) {
		log.Debug("InstallFile: refusing a relative target", zap.String("path", remotePath))
		return nil, fmt.Errorf("InstallFile: %q must be absolute", remotePath)
	}
	result := &ConfigResult{Path: remotePath, Success: true, Hosts: make(map[string]*HostResult)}
	modeArg := strconv.FormatUint(uint64(mode.Perm()), 8)

	var localHosts, remoteHosts []string
	localAddrs := getLocalIPs()
	for _, host := range hosts {
		if isLocalIP(host, localAddrs) {
			localHosts = append(localHosts, host)
		} else {
			remoteHosts = append(remoteHosts, host)
		}
	}
	log.Debug("InstallFile: hosts classified",
		zap.Strings("local", localHosts), zap.Strings("remote", remoteHosts))

	for _, host := range localHosts {
		hostStart := time.Now()
		hr := &HostResult{Host: host, Success: true}
		if out, err := localCommand(ctx, fmt.Sprintf("sudo install -D -m %s %s %s", modeArg, shellQuote(localPath), shellQuote(remotePath))).CombinedOutput(); err != nil {
			hr.Success, hr.Error = false, fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
			result.Success = false
		}
		log.Debug("InstallFile: installed on local host",
			zap.String("host", host), zap.Bool("success", hr.Success),
			zap.NamedError("host_error", hr.Error), zap.Duration("took", time.Since(hostStart)))
		result.Hosts[host] = hr
	}

	if len(remoteHosts) > 0 {
		staged := ".haify-install-" + filepath.Base(remotePath)
		copyStart := time.Now()
		copyResult, err := c.dispatch.Copy(ctx, remoteHosts, localPath, staged, dispatch.WithCopyMode(0700))
		if err != nil {
			log.Debug("InstallFile: copy errored",
				zap.Strings("hosts", remoteHosts), zap.Duration("took", time.Since(copyStart)), zap.Error(err))
			return nil, fmt.Errorf("InstallFile: copy to %v: %w", remoteHosts, err)
		}
		log.Debug("InstallFile: copy returned",
			zap.Strings("hosts", remoteHosts), zap.String("staged", staged), zap.Duration("took", time.Since(copyStart)))
		var copied []string
		for _, host := range remoteHosts {
			r := copyResult.Hosts[host]
			if r == nil || !r.Success {
				hr := &HostResult{Host: host, Error: fmt.Errorf("copy failed")}
				if r != nil && r.Error != nil {
					hr.Error = r.Error
				}
				log.Debug("InstallFile: copy failed on host",
					zap.String("host", host), zap.Bool("has_result", r != nil), zap.NamedError("host_error", hr.Error))
				result.Hosts[host] = hr
				result.Success = false
				continue
			}
			log.Debug("InstallFile: copied to host", zap.String("host", host))
			copied = append(copied, host)
		}
		if len(copied) > 0 {
			cmd := fmt.Sprintf(`sudo install -D -m %s "$HOME/%s" %s; rc=$?; rm -f "$HOME/%s"; exit $rc`,
				modeArg, staged, remotePath, staged)
			execResult, err := c.Exec(ctx, copied, cmd)
			if err != nil {
				log.Debug("InstallFile: install errored", zap.Strings("hosts", copied), zap.Error(err))
				return nil, fmt.Errorf("InstallFile: install on %v: %w", copied, err)
			}
			for _, host := range copied {
				hr := &HostResult{Host: host, Error: fmt.Errorf("no result for host")}
				if r := execResult.Hosts[host]; r != nil {
					hr = r
				}
				if !hr.Success {
					result.Success = false
				}
				log.Debug("InstallFile: install result", zap.String("host", host), zap.Bool("success", hr.Success))
				result.Hosts[host] = hr
			}
		} else {
			log.Debug("InstallFile: no host received the file, nothing to install")
		}
	}
	log.Debug("InstallFile: done",
		zap.String("path", remotePath), zap.Bool("success", result.Success),
		zap.Strings("failed_hosts", result.FailedHosts()), zap.Duration("took", time.Since(start)))
	return result, nil
}
