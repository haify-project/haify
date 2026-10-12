package deployment

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"go.uber.org/zap"
)

// ============ Config Distribution ============

// DistributeConfig distributes a configuration file to multiple nodes
func (c *Client) DistributeConfig(ctx context.Context, hosts []string, content, remotePath string, opts ...ConfigOption) (*ConfigResult, error) {
	options := &configOptions{}
	for _, opt := range opts {
		opt(options)
	}
	log := c.log()
	start := time.Now()
	// The content is never traced: a promoter config can carry CHAP
	// credentials. Its size says whether the right thing was sent.
	log.Debug("DistributeConfig: distributing file",
		zap.Strings("hosts", hosts),
		zap.String("path", remotePath),
		zap.Int("size_bytes", len(content)),
		zap.Bool("has_post_command", options.postCommand != ""))

	c.logger.Info("Distributing config",
		zap.Strings("hosts", hosts),
		zap.String("path", remotePath))

	localTempFile := "/tmp/" + filepath.Base(remotePath) + ".tmp"

	configResult := &ConfigResult{
		Path:    remotePath,
		Success: true,
		Hosts:   make(map[string]*HostResult),
	}

	// Separate local and remote hosts
	var localHosts []string
	var remoteHosts []string
	localAddrs := getLocalIPs()
	for _, host := range hosts {
		if isLocalIP(host, localAddrs) {
			localHosts = append(localHosts, host)
		} else {
			remoteHosts = append(remoteHosts, host)
		}
	}
	log.Debug("DistributeConfig: hosts classified",
		zap.Strings("local", localHosts), zap.Strings("remote", remoteHosts))

	// Handle local hosts - write directly
	for _, host := range localHosts {
		log.Debug("DistributeConfig: writing on local host",
			zap.String("host", host), zap.String("staging", localTempFile))
		// Write to temp path
		if err := os.WriteFile(localTempFile, []byte(content), 0644); err != nil {
			c.logger.Error("Failed to write local config", zap.String("host", host), zap.Error(err))
			configResult.Hosts[host] = &HostResult{
				Host:    host,
				Success: false,
				Error:   err,
			}
			configResult.Success = false
			continue
		}

		// Create directory and move to final location using local exec
		mkdirCmd := exec.Command("sudo", "mkdir", "-p", filepath.Dir(remotePath))
		if err := mkdirCmd.Run(); err != nil {
			c.logger.Error("Failed to create directory", zap.String("host", host), zap.Error(err))
			configResult.Hosts[host] = &HostResult{
				Host:    host,
				Success: false,
				Error:   err,
			}
			configResult.Success = false
			// Best-effort cleanup of the staging copy. The host has already been
			// recorded as failed with the error that matters; the temp path is a
			// fixed name that the next distribution overwrites, so a failed
			// unlink leaks nothing but one stale file.
			_ = os.Remove(localTempFile)
			continue
		}

		mvCmd := exec.Command("sudo", "mv", "-f", localTempFile, remotePath)
		if err := mvCmd.Run(); err != nil {
			c.logger.Error("Failed to move local config", zap.String("host", host), zap.Error(err))
			configResult.Hosts[host] = &HostResult{
				Host:    host,
				Success: false,
				Error:   err,
			}
			configResult.Success = false
			_ = os.Remove(localTempFile)
			continue
		}

		configResult.Hosts[host] = &HostResult{
			Host:    host,
			Success: true,
		}
		c.logger.Debug("Local config distributed", zap.String("host", host))
	}

	// Handle remote hosts - use dispatch.Copy
	if len(remoteHosts) > 0 {
		c.logger.Debug("Copying to remote hosts", zap.Strings("remote_hosts", remoteHosts))
		// For remote hosts, use cat + ssh + sudo tee to handle privileged paths
		for _, host := range remoteHosts {
			hostStart := time.Now()
			log.Debug("DistributeConfig: copying to remote host",
				zap.String("host", host), zap.String("path", remotePath))
			// First create directory
			mkdirCmd := fmt.Sprintf("sudo mkdir -p %s", filepath.Dir(remotePath))
			mkdirResult, err := c.Exec(ctx, []string{host}, mkdirCmd)
			if err != nil {
				c.logger.Error("Failed to create directory", zap.String("host", host), zap.Error(err))
				configResult.Hosts[host] = &HostResult{
					Host:    host,
					Success: false,
					Error:   err,
				}
				configResult.Success = false
				continue
			}
			if !mkdirResult.AllSuccess() {
				log.Debug("DistributeConfig: mkdir failed on remote host",
					zap.String("host", host), zap.String("dir", filepath.Dir(remotePath)))
				configResult.Hosts[host] = &HostResult{
					Host:    host,
					Success: false,
					Error:   fmt.Errorf("mkdir failed"),
				}
				configResult.Success = false
				continue
			}

			// Use cat | ssh | sudo tee to copy file with root permissions
			// Write content to temp file first
			if err := os.WriteFile(localTempFile, []byte(content), 0644); err != nil {
				c.logger.Error("Failed to write temp file", zap.Error(err))
				configResult.Hosts[host] = &HostResult{
					Host:    host,
					Success: false,
					Error:   err,
				}
				configResult.Success = false
				continue
			}

			// Copy using ssh with sudo tee (direct execution via dispatch)
			// First read file content
			fileContent, err := os.ReadFile(localTempFile)
			if err != nil {
				c.logger.Error("Failed to read temp file", zap.Error(err))
				configResult.Hosts[host] = &HostResult{
					Host:    host,
					Success: false,
					Error:   err,
				}
				configResult.Success = false
				continue
			}

			// Transfer via base64 so binary content survives intact. A single
			// `echo <base64>` breaks for large files: one shell argument is
			// capped at MAX_ARG_STRLEN (128 KiB on Linux), so a multi-MB binary
			// (e.g. the haify-proxy WAN binary) fails with "Argument list too long"
			// and the file is never written. Small content keeps the fast single
			// command; large content is streamed in sub-128 KiB base64 chunks.
			encoded := base64.StdEncoding.EncodeToString(fileContent)
			var copyErr error
			if len(encoded) <= maxInlineB64Len {
				log.Debug("DistributeConfig: sending inline",
					zap.String("host", host), zap.Int("encoded_bytes", len(encoded)))
				cmd := fmt.Sprintf("echo %s | base64 -d | sudo tee %s > /dev/null",
					fmt.Sprintf("%q", encoded), remotePath)
				r, err := c.Exec(ctx, []string{host}, cmd)
				switch {
				case err != nil:
					copyErr = err
				case !r.AllSuccess():
					copyErr = fmt.Errorf("copy failed")
				}
			} else {
				log.Debug("DistributeConfig: sending in chunks",
					zap.String("host", host), zap.Int("encoded_bytes", len(encoded)),
					zap.Int("chunk_bytes", maxInlineB64Len))
				copyErr = c.writeRemoteFileChunked(ctx, host, remotePath, encoded)
			}
			if copyErr != nil {
				c.logger.Error("Failed to copy config", zap.String("host", host), zap.Error(copyErr))
				configResult.Hosts[host] = &HostResult{
					Host:    host,
					Success: false,
					Error:   copyErr,
				}
				configResult.Success = false
				continue
			}

			configResult.Hosts[host] = &HostResult{
				Host:    host,
				Success: true,
			}
			c.logger.Debug("Remote config distributed", zap.String("host", host),
				zap.Duration("took", time.Since(hostStart)))
		}
		// The remote copies are done with the staging file; same best-effort
		// reasoning as the local branch above.
		_ = os.Remove(localTempFile)
	}

	// Run post-command if specified
	if options.postCommand != "" {
		log.Debug("DistributeConfig: running post-command", zap.Strings("hosts", hosts))
		_, postErr := c.Exec(ctx, hosts, options.postCommand)
		log.Debug("DistributeConfig: post-command done", zap.Error(postErr))
	}

	log.Debug("DistributeConfig: done",
		zap.String("path", remotePath),
		zap.Bool("success", configResult.Success),
		zap.Strings("failed_hosts", configResult.FailedHosts()),
		zap.Duration("took", time.Since(start)))
	return configResult, nil
}

// maxInlineB64Len bounds the base64 payload sent as a single `echo` argument.
// Linux caps one argument at MAX_ARG_STRLEN (128 KiB); stay well under it so the
// fast single-command path never trips "Argument list too long".
const maxInlineB64Len = 100 * 1024

// writeRemoteFileChunked writes base64-encoded content to remotePath on host by
// streaming it in sub-128 KiB chunks (each a single safe argument), then decoding
// once server-side. This is what lets DistributeConfig ship multi-MB binaries
// (e.g. haify-proxy) that overflow a single-argument echo. base64's alphabet
// (A-Za-z0-9+/=) contains no single-quote, so each chunk is quote-safe.
func (c *Client) writeRemoteFileChunked(ctx context.Context, host, remotePath, encoded string) error {
	log := c.log()
	start := time.Now()
	log.Debug("writeRemoteFileChunked: start",
		zap.String("host", host), zap.String("path", remotePath), zap.Int("encoded_bytes", len(encoded)))
	tmp := remotePath + ".b64.part"
	// Start from an empty temp file.
	if r, err := c.Exec(ctx, []string{host}, fmt.Sprintf("sudo sh -c ': > %s'", tmp)); err != nil {
		log.Debug("writeRemoteFileChunked: init temp file errored", zap.String("host", host), zap.Error(err))
		return fmt.Errorf("init temp file: %w", err)
	} else if !r.AllSuccess() {
		log.Debug("writeRemoteFileChunked: init temp file failed", zap.String("host", host), zap.String("tmp", tmp))
		return fmt.Errorf("init temp file on %s failed", host)
	}
	const chunk = maxInlineB64Len
	for i := 0; i < len(encoded); i += chunk {
		end := i + chunk
		if end > len(encoded) {
			end = len(encoded)
		}
		log.Debug("writeRemoteFileChunked: appending chunk",
			zap.String("host", host), zap.Int("offset", i), zap.Int("chunk_bytes", end-i))
		cmd := fmt.Sprintf("printf '%%s' '%s' | sudo tee -a %s > /dev/null", encoded[i:end], tmp)
		if r, err := c.Exec(ctx, []string{host}, cmd); err != nil {
			log.Debug("writeRemoteFileChunked: append errored", zap.String("host", host), zap.Int("offset", i), zap.Error(err))
			return fmt.Errorf("append chunk: %w", err)
		} else if !r.AllSuccess() {
			log.Debug("writeRemoteFileChunked: append failed", zap.String("host", host), zap.Int("offset", i))
			return fmt.Errorf("append chunk on %s failed", host)
		}
	}
	// Decode beside the target and rename it into place. Writing through the
	// target — what this used to do — fails with "Text file busy" when it is a
	// running executable: every re-push of the haify-proxy binary to a node
	// whose proxy was up. A rename replaces a busy file, and leaves the target
	// whole if anything before it fails. The new file takes the old one's mode
	// and owner, as writing into it did.
	next := remotePath + ".haify-new"
	cmd := fmt.Sprintf("sudo sh -c 'base64 -d %[1]s > %[2]s && { [ ! -e %[3]s ] || { chmod --reference=%[3]s %[2]s && chown --reference=%[3]s %[2]s; }; } && mv -f %[2]s %[3]s && rm -f %[1]s'",
		tmp, next, remotePath)
	if r, err := c.Exec(ctx, []string{host}, cmd); err != nil {
		log.Debug("writeRemoteFileChunked: decode errored", zap.String("host", host), zap.Error(err))
		return fmt.Errorf("decode remote file: %w", err)
	} else if !r.AllSuccess() {
		log.Debug("writeRemoteFileChunked: decode failed", zap.String("host", host), zap.String("path", remotePath))
		return fmt.Errorf("decode remote file on %s failed", host)
	}
	log.Debug("writeRemoteFileChunked: done",
		zap.String("host", host), zap.String("path", remotePath), zap.Duration("took", time.Since(start)))
	return nil
}

// DeleteConfig removes a config file from all nodes
func (c *Client) DeleteConfig(ctx context.Context, hosts []string, remotePath string) error {
	c.log().Debug("DeleteConfig: removing file", zap.Strings("hosts", hosts), zap.String("path", remotePath))
	c.logger.Info("Deleting config", zap.String("path", remotePath))

	_, err := c.Exec(ctx, hosts, fmt.Sprintf("sudo rm -f %s", remotePath))
	if err != nil {
		c.log().Debug("DeleteConfig: removal errored", zap.String("path", remotePath), zap.Error(err))
	}
	return err
}

// ConfigOption configures config distribution
type ConfigOption func(*configOptions)

type configOptions struct {
	backup      bool
	postCommand string
}

// WithBackup enables backup of existing config
func WithBackup(backup bool) ConfigOption {
	return func(o *configOptions) {
		o.backup = backup
	}
}

// WithPostCommand sets a command to run after distribution
func WithPostCommand(cmd string) ConfigOption {
	return func(o *configOptions) {
		o.postCommand = cmd
	}
}
