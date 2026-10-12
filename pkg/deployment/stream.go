package deployment

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/liliang-cn/dispatch/pkg/inventory"
	dispatchssh "github.com/liliang-cn/dispatch/pkg/ssh"
	"go.uber.org/zap"
	"golang.org/x/crypto/ssh"
)

// streamKeepalive is how often a stream's SSH connection is probed. A node
// that loses power sends no FIN; without a probe a read on its stream blocks
// until TCP gives up, which takes the better part of a quarter of an hour.
var streamKeepalive = 15 * time.Second

// StreamLines runs cmd on host and calls onLine with each line of its output
// as it arrives, until the command ends, the connection is lost or ctx is
// cancelled. It is for commands that do not end on their own, such as
// `drbdsetup events2`; dispatch's own streaming waits for the command to exit
// before it will close, so it cannot be stopped.
//
// Closing the connection does not stop a command started without a terminal:
// it runs on until it next writes. Callers bound such a command themselves,
// with timeout(1).
func (c *Client) StreamLines(ctx context.Context, host, cmd string, onLine func(string)) error {
	log := c.log()
	start := time.Now()
	if ce := log.Check(zap.DebugLevel, "StreamLines: starting stream"); ce != nil {
		ce.Write(zap.String("host", host), zap.String("cmd", CommandSummary(cmd)))
	}
	if isLocalAddress(host) {
		log.Debug("StreamLines: host is this machine, streaming locally", zap.String("host", host))
		err := streamLocal(ctx, cmd, onLine)
		log.Debug("StreamLines: local stream ended",
			zap.String("host", host), zap.Duration("took", time.Since(start)), zap.Error(err))
		return err
	}
	inv, err := inventory.New(c.configPath)
	if err != nil {
		log.Debug("StreamLines: cannot load dispatch config", zap.String("config_path", c.configPath), zap.Error(err))
		return fmt.Errorf("load dispatch config: %w", err)
	}
	hosts, err := inv.GetHosts([]string{host})
	if err != nil || len(hosts) == 0 {
		log.Debug("StreamLines: cannot resolve host", zap.String("host", host), zap.Int("count", len(hosts)), zap.Error(err))
		return fmt.Errorf("resolve %s: %v", host, err)
	}
	h := hosts[0]
	sshCfg := inv.GetConfig().SSH
	var opts []dispatchssh.ClientOption
	if sshCfg.KnownHostsPath != "" {
		opts = append(opts, dispatchssh.WithKnownHosts(sshCfg.KnownHostsPath))
	}
	if sshCfg.StrictHostKey {
		opts = append(opts, dispatchssh.WithStrictHostKey(true))
	}
	log.Debug("StreamLines: host resolved",
		zap.String("host", host), zap.String("address", h.Address), zap.String("user", h.User), zap.Int("port", h.Port),
		zap.Bool("known_hosts", sshCfg.KnownHostsPath != ""), zap.Bool("strict_host_key", sshCfg.StrictHostKey))
	client, err := dispatchssh.NewClient(sshCfg.KeyPath, opts...)
	if err != nil {
		log.Debug("StreamLines: cannot build ssh client", zap.String("host", host), zap.Error(err))
		return fmt.Errorf("ssh client: %w", err)
	}
	connectStart := time.Now()
	conn, err := client.Connect(dispatchssh.HostSpec{
		Address: h.Address, User: h.User, Port: h.Port, KeyPath: h.KeyPath,
		UserSet: h.UserSet, PortSet: h.PortSet, KeyPathSet: h.KeyPathSet,
	})
	if err != nil {
		log.Debug("StreamLines: connect failed",
			zap.String("host", host), zap.Duration("took", time.Since(connectStart)), zap.Error(err))
		return fmt.Errorf("connect %s: %w", host, err)
	}
	log.Debug("StreamLines: connected", zap.String("host", host), zap.Duration("took", time.Since(connectStart)))
	var closeOnce sync.Once
	closeConn := func() { closeOnce.Do(func() { _ = conn.Close() }) }
	defer closeConn()

	session, err := conn.NewSession()
	if err != nil {
		log.Debug("StreamLines: cannot open session", zap.String("host", host), zap.Error(err))
		return fmt.Errorf("session on %s: %w", host, err)
	}
	stdout, err := session.StdoutPipe()
	if err != nil {
		log.Debug("StreamLines: cannot get stdout pipe", zap.String("host", host), zap.Error(err))
		return err
	}
	if err := session.Start(cmd); err != nil {
		log.Debug("StreamLines: cannot start command", zap.String("host", host), zap.Error(err))
		return fmt.Errorf("start on %s: %w", host, err)
	}

	done := make(chan struct{})
	defer close(done)
	go func() {
		t := time.NewTicker(streamKeepalive)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				log.Debug("StreamLines: context ended, closing connection", zap.String("host", host))
				closeConn()
				return
			case <-t.C:
				if !keepalive(conn) {
					log.Debug("StreamLines: keepalive unanswered, closing connection",
						zap.String("host", host), zap.Duration("interval", streamKeepalive))
					closeConn()
					return
				}
			}
		}
	}()

	err = scanLines(stdout, onLine)
	if ctx.Err() != nil {
		log.Debug("StreamLines: stream stopped by context",
			zap.String("host", host), zap.Duration("took", time.Since(start)), zap.Error(ctx.Err()))
		return ctx.Err()
	}
	if err != nil {
		log.Debug("StreamLines: stream read failed",
			zap.String("host", host), zap.Duration("took", time.Since(start)), zap.Error(err))
		return fmt.Errorf("stream from %s: %w", host, err)
	}
	log.Debug("StreamLines: stream ended", zap.String("host", host), zap.Duration("took", time.Since(start)))
	return fmt.Errorf("stream from %s ended", host)
}

// keepalive reports whether the connection still answers, within one
// keepalive period.
func keepalive(conn *ssh.Client) bool {
	answered := make(chan bool, 1)
	go func() {
		_, _, err := conn.SendRequest("keepalive@openssh.com", true, nil)
		answered <- err == nil
	}()
	select {
	case ok := <-answered:
		if !ok {
			zap.L().Debug("keepalive: request failed")
		}
		return ok
	case <-time.After(streamKeepalive):
		zap.L().Debug("keepalive: no answer in time", zap.Duration("interval", streamKeepalive))
		return false
	}
}

func streamLocal(ctx context.Context, cmd string, onLine func(string)) error {
	if ce := zap.L().Check(zap.DebugLevel, "streamLocal: starting local stream"); ce != nil {
		ce.Write(zap.String("cmd", CommandSummary(cmd)))
	}
	c := exec.CommandContext(ctx, "/bin/sh", "-c", cmd)
	stdout, err := c.StdoutPipe()
	if err != nil {
		zap.L().Debug("streamLocal: cannot get stdout pipe", zap.Error(err))
		return err
	}
	if err := c.Start(); err != nil {
		zap.L().Debug("streamLocal: cannot start command", zap.Error(err))
		return err
	}
	err = scanLines(stdout, onLine)
	waitErr := c.Wait()
	zap.L().Debug("streamLocal: command exited", zap.NamedError("read_error", err), zap.NamedError("wait_error", waitErr))
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		return err
	}
	return fmt.Errorf("local stream ended")
}

func scanLines(r io.Reader, onLine func(string)) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	lines := 0
	for sc.Scan() {
		lines++
		onLine(strings.TrimRight(sc.Text(), "\r"))
	}
	zap.L().Debug("scanLines: reader done", zap.Int("count", lines), zap.Error(sc.Err()))
	return sc.Err()
}

// isLocalAddress reports whether host names this machine, which dispatch
// runs without SSH and so may not have a key for.
func isLocalAddress(host string) bool {
	if host == "localhost" || host == "127.0.0.1" || host == "::1" {
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		zap.L().Debug("isLocalAddress: cannot list interface addresses", zap.String("host", host), zap.Error(err))
		return false
	}
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && n.IP.Equal(ip) {
			return true
		}
	}
	return false
}
