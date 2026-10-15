// Package deployment handles the execution of commands on storage nodes
// using the dispatch SSH library.
package deployment

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/liliang-cn/dispatch/pkg/dispatch"
	"go.uber.org/zap"
)

// getLocalIPs returns all local IP addresses
func getLocalIPs() []string {
	var ips []string
	interfaces, err := net.Interfaces()
	if err != nil {
		zap.L().Debug("getLocalIPs: cannot list interfaces", zap.Error(err))
		return ips
	}

	for _, iface := range interfaces {
		addrs, err := iface.Addrs()
		if err != nil {
			zap.L().Debug("getLocalIPs: cannot read interface addresses",
				zap.String("interface", iface.Name), zap.Error(err))
			continue
		}
		for _, addr := range addrs {
			var ip net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip != nil && !ip.IsLoopback() {
				ips = append(ips, ip.String())
			}
		}
	}
	return ips
}

// asRootShim makes `sudo` a pass-through for a controller already running as
// root. The commands are written for remote nodes and say `sudo`; run locally
// they gain nothing from it, and each one put three lines (COMMAND, session
// opened, session closed) into the controller's own journal — on a polling
// controller about 97% of it, drowning every line the controller wrote.
// Exported so the `bash -c` scripts some commands start see it too.
const asRootShim = `sudo() { "$@"; }; export -f sudo; `

// localCommand runs a node command on this machine.
func localCommand(ctx context.Context, cmd string) *exec.Cmd {
	if os.Geteuid() == 0 {
		return exec.CommandContext(ctx, "bash", "-c", asRootShim+cmd)
	}
	return exec.CommandContext(ctx, "sh", "-c", cmd)
}

// isLocalIP checks if an IP address is local
func isLocalIP(host string, localAddrs []string) bool {
	for _, localIP := range localAddrs {
		if host == localIP {
			return true
		}
	}
	return false
}

// Client handles DRBD resource management via dispatch
type Client struct {
	dispatch *dispatch.Dispatch
	logger   *zap.Logger
	parallel int
	// configPath is the dispatch config the client was built from, which
	// StreamLines reads again for the SSH settings dispatch keeps to itself.
	configPath string
}

// log returns the client's logger, or the process-wide one for a client
// built without one (NewWithOptions(nil, ...), tests).
func (c *Client) log() *zap.Logger {
	if c != nil && c.logger != nil {
		return c.logger
	}
	return zap.L()
}

// cmdSummaryLen bounds how much of a command a trace shows.
const cmdSummaryLen = 120

// traceTextLen bounds how much of a failed command's output a trace shows.
const traceTextLen = 400

var (
	// tracePayloadRE matches an inline payload: a base64-wrapped script, or a
	// file DistributeConfig pushes on the command line. It says nothing to a
	// reader and can carry a secret, so a trace shows a placeholder.
	tracePayloadRE = regexp.MustCompile(`[A-Za-z0-9+/=]{48,}`)
	// traceSecretRE matches a secret handed over as name=value, such as the
	// iSCSI agent's incoming_password=..., so a trace shows that it was set
	// and not what it is.
	traceSecretRE = regexp.MustCompile(`(?i)((?:pass(?:word|wd|phrase)?|secret|token|auth)[a-z_]*=)('[^']*'|"[^"]*"|[^\s'"]+)`)
)

// maskForTrace hides inline payloads and name=secret arguments in s.
func maskForTrace(s string) string {
	s = tracePayloadRE.ReplaceAllString(s, "<payload>")
	return traceSecretRE.ReplaceAllString(s, "${1}<redacted>")
}

// CommandSummary is what a trace shows of a command: its first line, with
// inline payloads and secret arguments masked, cut to 120 characters. It costs
// two regexp passes, so callers build it only when Debug is enabled.
func CommandSummary(cmd string) string {
	line, rest, _ := strings.Cut(cmd, "\n")
	// Mask before cutting, on a window wider than the result, so a payload
	// or secret that straddles the cut is still recognised as one.
	if len(line) > 4*cmdSummaryLen {
		line = line[:4*cmdSummaryLen]
	}
	line = maskForTrace(line)
	if len(line) > cmdSummaryLen {
		return line[:cmdSummaryLen] + "..."
	}
	if strings.TrimSpace(rest) != "" {
		return line + " ..."
	}
	return line
}

// traceText is what a trace shows of a failed command's output: masked like
// a command and cut to a few hundred characters.
func traceText(s string) string {
	if len(s) > 4*traceTextLen {
		s = s[:4*traceTextLen]
	}
	s = maskForTrace(strings.TrimSpace(s))
	if len(s) > traceTextLen {
		return s[:traceTextLen] + "..."
	}
	return s
}

// Options tunes how the deployment client reaches storage nodes.
type Options struct {
	// ConfigPath is the dispatch TOML config to use. Empty keeps dispatch's
	// own default (~/.dispatch/config.toml, then ~/.ssh/config).
	ConfigPath string
	// Parallel caps the fan-out of a single dispatch call. 0 uses the default.
	Parallel int
}

const defaultParallel = 10

// New creates a new deployment Client using dispatch's default config
// discovery (~/.dispatch/config.toml, falling back to ~/.ssh/config).
func New(logger *zap.Logger) (*Client, error) {
	return NewWithOptions(logger, Options{})
}

// NewWithOptions creates a deployment Client honouring an explicit dispatch
// config path and parallelism.
//
// Passing the config path through matters because the controller may run as a
// user whose home directory is not where the operator put the dispatch config
// (systemd unit with its own HOME, non-root operator, test harness). Before
// this existed the `[dispatch] config_path` setting in controller.toml was
// parsed by nobody and silently ignored, so the controller kept using
// ~/.dispatch/config.toml and failed with opaque SSH auth errors.
func NewWithOptions(logger *zap.Logger, opts Options) (*Client, error) {
	log := logger
	if log == nil {
		log = zap.L()
	}
	log.Debug("NewWithOptions: building deployment client",
		zap.String("config_path", opts.ConfigPath), zap.Int("parallel", opts.Parallel))
	parallel := opts.Parallel
	if parallel <= 0 {
		log.Debug("NewWithOptions: no parallelism set, using default", zap.Int("parallel", defaultParallel))
		parallel = defaultParallel
	}

	// The parallelism is handed to dispatch as its default as well as passed
	// per Exec. dispatch's Copy takes no parallelism of its own and sizes its
	// worker semaphore from the config file's [exec] parallel; a file without
	// that line makes it 0, the semaphore unbuffered, and every Copy blocks
	// forever on its first host. In production that stalled each backup's
	// credential upload, and with it InstallFile, which Self-HA uses to ship
	// the controller binary.
	dispatchCfg := &dispatch.Config{Exec: &dispatch.ExecConfig{Parallel: parallel}}
	if opts.ConfigPath != "" {
		// dispatch silently falls back to its own default config
		// (~/.dispatch/config.toml, then ~/.ssh/config) when the path it is
		// handed does not exist. A typo in controller.toml would therefore put
		// the controller back on somebody else's SSH settings and fail later
		// with an opaque "unable to authenticate" — the exact dead end wiring
		// this path was meant to end. Refuse to start instead.
		if _, err := os.Stat(opts.ConfigPath); err != nil {
			log.Debug("NewWithOptions: dispatch config not readable",
				zap.String("config_path", opts.ConfigPath), zap.Error(err))
			return nil, fmt.Errorf("dispatch config %q: %w", opts.ConfigPath, err)
		}
		dispatchCfg.ConfigPath = opts.ConfigPath
	} else {
		log.Debug("NewWithOptions: no dispatch config path, using dispatch's default discovery")
	}

	client, err := dispatch.New(dispatchCfg)
	if err != nil {
		log.Debug("NewWithOptions: dispatch.New failed", zap.Error(err))
		return nil, fmt.Errorf("failed to create dispatch client: %w", err)
	}

	if logger != nil && opts.ConfigPath != "" {
		logger.Info("Using dispatch config", zap.String("path", opts.ConfigPath))
	}

	log.Debug("NewWithOptions: deployment client ready", zap.Int("parallel", parallel))
	return &Client{
		dispatch:   client,
		logger:     logger,
		parallel:   parallel,
		configPath: opts.ConfigPath,
	}, nil
}

// isLocalHost checks if a host is the local machine
func isLocalHost(host string) bool {
	hostname, _ := os.Hostname()

	// Check if host matches local hostname
	if host == hostname || host == "localhost" || host == "127.0.0.1" {
		zap.L().Debug("isLocalHost: host names this machine", zap.String("host", host), zap.String("hostname", hostname))
		return true
	}

	// Check if host matches any local IP address
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		zap.L().Debug("isLocalHost: cannot list interface addresses", zap.String("host", host), zap.Error(err))
		return false
	}

	for _, addr := range addrs {
		var ip net.IP
		switch v := addr.(type) {
		case *net.IPNet:
			ip = v.IP
		case *net.IPAddr:
			ip = v.IP
		}

		if ip != nil && ip.String() == host {
			zap.L().Debug("isLocalHost: host is a local address", zap.String("host", host))
			return true
		}
	}

	zap.L().Debug("isLocalHost: host is remote", zap.String("host", host))
	return false
}

// ============ Command Execution ============

// Exec executes a command on multiple hosts
func (c *Client) Exec(ctx context.Context, hosts []string, cmd string, opts ...ExecOption) (*ExecResult, error) {
	options := &execOptions{}
	for _, opt := range opts {
		opt(options)
	}

	parallel := c.parallel
	if options.parallel > 0 {
		parallel = options.parallel
	}
	timeout := 30 * time.Second
	if options.timeout > 0 {
		timeout = options.timeout
	}

	log := c.log()
	start := time.Now()
	if ce := log.Check(zap.DebugLevel, "Exec: running command"); ce != nil {
		ce.Write(zap.Strings("hosts", hosts),
			zap.String("cmd", CommandSummary(cmd)),
			zap.Int("parallel", parallel),
			zap.Duration("timeout", timeout))
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

	log.Debug("Exec: hosts classified",
		zap.Strings("local", localHosts),
		zap.Strings("remote", remoteHosts))

	// Initialize result
	result := &dispatch.ExecResult{
		Hosts: make(map[string]*dispatch.HostResult),
	}

	// Execute on local hosts using os/exec
	for _, host := range localHosts {
		start := time.Now()
		output, err := localCommand(ctx, cmd).CombinedOutput()
		end := time.Now()
		exitCode := 0
		var errorMsg error = nil
		if err != nil {
			errorMsg = err
			if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() >= 0 {
				exitCode = exitErr.ExitCode()
			} else {
				exitCode = 1
			}
		}
		log.Debug("Exec: ran on local host",
			zap.String("host", host),
			zap.Int("exit_code", exitCode),
			zap.Duration("took", end.Sub(start)))
		result.Hosts[host] = &dispatch.HostResult{
			Host:      host,
			Output:    output,
			StartTime: start,
			EndTime:   end,
			Duration:  end.Sub(start),
			ExitCode:  exitCode,
			ErrorMsg:  errorMsg,
			Success:   exitCode == 0 && errorMsg == nil,
		}
	}

	// Execute on remote hosts using dispatch
	if len(remoteHosts) > 0 {
		dispatchStart := time.Now()
		dispatchResult, dispatchErr := c.dispatchExec(ctx, remoteHosts, cmd, parallel, timeout, options.readOnly)
		if dispatchErr != nil {
			c.logger.Warn("Remote dispatch.Exec failed", zap.Error(dispatchErr))
			return nil, dispatchErr
		}
		log.Debug("Exec: dispatch returned",
			zap.Strings("hosts", remoteHosts),
			zap.Int("result_count", len(dispatchResult.Hosts)),
			zap.Duration("took", time.Since(dispatchStart)))
		for host, r := range dispatchResult.Hosts {
			result.Hosts[host] = r
		}
	}

	execResult := &ExecResult{
		Hosts: make(map[string]*HostResult),
	}

	for host, r := range result.Hosts {
		// Log stderr too. drbdadm/lvcreate/zfs report their real reason there,
		// so a stdout-only debug line shows `output_len: 0` for a command that
		// failed with a perfectly good explanation — the exact dead end this
		// log exists to prevent. Only a failure's text is shown, cut short and
		// masked: a successful command's output can be a config read back from
		// a node, CHAP credentials included.
		if ce := log.Check(zap.DebugLevel, "Exec: host result"); ce != nil {
			fields := []zap.Field{
				zap.String("host", host),
				zap.Bool("success", r.Success),
				zap.Int("exit_code", r.ExitCode),
				zap.Int("output_len", len(r.Output)),
				zap.Int("stderr_len", len(r.Error)),
				zap.Duration("took", r.Duration),
			}
			if !r.Success {
				fields = append(fields,
					zap.String("output", traceText(string(r.Output))),
					zap.String("stderr", traceText(string(r.Error))))
				if r.ErrorMsg != nil {
					fields = append(fields, zap.String("error_msg", traceText(r.ErrorMsg.Error())))
				}
			}
			ce.Write(fields...)
		}
		// dispatch keeps stdout and stderr apart, but almost everything worth
		// reporting from lvcreate/drbdadm/zfs goes to STDERR — "already exists",
		// "insufficient free space", "Refusing to be resized". Exposing only
		// stdout is why failures used to surface as an empty message ("creation
		// failed on 10.0.0.1: "), leaving callers nothing to act on. Callers
		// treat Output as "what the command said", so give them both streams.
		hr := &HostResult{
			Host:    host,
			Output:  combineStreams(string(r.Output), string(r.Error)),
			Success: r.Success,
			Error:   fmt.Errorf("%s", string(r.Error)),
		}
		// A command that never ran has no output at all: the reason is the
		// connection's — a changed SSH host key, a refused or timed-out
		// connection — and dispatch reports it in ErrorMsg. Dropping it made
		// an unreachable node read as "no output" in every error and alert.
		if r.ErrorMsg != nil {
			hr.Error = r.ErrorMsg
			if hr.Output == "" {
				hr.Output = r.ErrorMsg.Error()
			}
		}
		execResult.Hosts[host] = hr
	}

	if ce := log.Check(zap.DebugLevel, "Exec: done"); ce != nil {
		ce.Write(zap.Strings("hosts", hosts),
			zap.Int("result_count", len(execResult.Hosts)),
			zap.Strings("failed_hosts", execResult.FailedHosts()),
			zap.Duration("took", time.Since(start)))
	}
	return execResult, nil
}

// ============ Service Management ============

// ServiceStart starts a systemd service
func (c *Client) ServiceStart(ctx context.Context, hosts []string, service string) (*ExecResult, error) {
	c.log().Debug("ServiceStart: starting service", zap.Strings("hosts", hosts), zap.String("service", service))
	return c.Exec(ctx, hosts, fmt.Sprintf("sudo systemctl start %s", service))
}

// ServiceStop stops a systemd service
func (c *Client) ServiceStop(ctx context.Context, hosts []string, service string) (*ExecResult, error) {
	c.log().Debug("ServiceStop: stopping service", zap.Strings("hosts", hosts), zap.String("service", service))
	return c.Exec(ctx, hosts, fmt.Sprintf("sudo systemctl stop %s", service))
}

// ServiceRestart restarts a systemd service
func (c *Client) ServiceRestart(ctx context.Context, hosts []string, service string) (*ExecResult, error) {
	c.log().Debug("ServiceRestart: restarting service", zap.Strings("hosts", hosts), zap.String("service", service))
	return c.Exec(ctx, hosts, fmt.Sprintf("sudo systemctl restart %s", service))
}

// ============ Options ============

// ExecOption configures command execution
type ExecOption func(*execOptions)

type execOptions struct {
	parallel int
	timeout  time.Duration
	readOnly bool // see WithReadOnly
}

// ExecTimeoutOf reports the timeout an option list would apply, or 0 when it
// sets none and Exec's own default would be used.
//
// Exported so a caller can assert on what it is about to ask for. The default
// suits short queries and is badly wrong for a command that streams a volume,
// which is not visible at the call site without this.
func ExecTimeoutOf(opts ...ExecOption) time.Duration {
	o := &execOptions{}
	for _, opt := range opts {
		opt(o)
	}
	return o.timeout
}

// WithExecParallel sets parallelism
func WithExecParallel(n int) ExecOption {
	return func(o *execOptions) {
		o.parallel = n
	}
}

// WithExecTimeout sets timeout
func WithExecTimeout(d time.Duration) ExecOption {
	return func(o *execOptions) {
		o.timeout = d
	}
}
