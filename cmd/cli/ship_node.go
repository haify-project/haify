package main

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/haify-project/haify/pkg/ship"
	"github.com/spf13/cobra"
	"go.uber.org/zap"
)

// The node side of `haify resource ship`, run by the controllers as transient
// units on a node of each cluster. Each writes what it did to a state
// directory the controller reads: ready (receiver: "<fingerprint> <port>"),
// progress (sender: bytes sent) and result.json.

type shipResult struct {
	OK     bool   `json:"ok"`
	Bytes  uint64 `json:"bytes"`
	SHA256 string `json:"sha256,omitempty"`
	Error  string `json:"error,omitempty"`
}

func writeShipResult(dir string, r shipResult) {
	b, _ := json.Marshal(r)
	err := os.WriteFile(filepath.Join(dir, "result.json"), b, 0o600)
	zap.L().Debug("writeShipResult: result written", zap.String("dir", dir), zap.Bool("ok", r.OK), zap.Uint64("bytes", r.Bytes),
		zap.String("error", r.Error), zap.NamedError("write_err", err))
}

func readToken(path string) (string, error) {
	log := zap.L()
	b, err := os.ReadFile(path)
	if err != nil {
		log.Debug("readToken: token file not readable", zap.String("path", path), zap.Error(err))
		return "", err
	}
	log.Debug("readToken: token file read", zap.String("path", path), zap.Bool("has_token", len(strings.TrimSpace(string(b))) > 0))
	return strings.TrimSpace(string(b)), nil
}

func shipCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "ship", Short: "Node side of resource ship (run by the controller)", Hidden: true}
	cmd.AddCommand(shipRecv(), shipSend())
	return cmd
}

func shipRecv() *cobra.Command {
	var device, listen, tokenFile, dir string
	cmd := &cobra.Command{
		Use: "recv",
		RunE: func(cmd *cobra.Command, _ []string) error {
			log := zap.L()
			log.Debug("shipRecv: running command", zap.String("device", device), zap.String("listen", listen),
				zap.String("token_file", tokenFile), zap.String("state_dir", dir))
			token, err := readToken(tokenFile)
			if err != nil {
				log.Debug("shipRecv: readToken failed", zap.Error(err))
				return err
			}
			r, err := ship.Listen(listen, token)
			if err != nil {
				log.Debug("shipRecv: Listen failed", zap.Error(err))
				writeShipResult(dir, shipResult{Error: err.Error()})
				return err
			}
			defer func() { _ = r.Close() }()
			port := r.Addr().(*net.TCPAddr).Port
			log.Debug("shipRecv: listening", zap.Int("port", port), zap.String("fingerprint", r.Fingerprint))
			if err := os.WriteFile(filepath.Join(dir, "ready"), fmt.Appendf(nil, "%s %d\n", r.Fingerprint, port), 0o600); err != nil {
				log.Debug("shipRecv: ready file not writable", zap.String("state_dir", dir), zap.Error(err))
				return err
			}
			f, err := os.OpenFile(device, os.O_WRONLY, 0)
			if err != nil {
				log.Debug("shipRecv: device not openable", zap.String("device", device), zap.Error(err))
				writeShipResult(dir, shipResult{Error: err.Error()})
				return err
			}
			defer func() { _ = f.Close() }()
			log.Debug("shipRecv: receiving", zap.String("device", device))
			start := time.Now()
			n, sum, err := r.Receive(f, f.Sync)
			if err != nil {
				log.Debug("shipRecv: Receive failed", zap.Uint64("bytes", n), zap.Duration("took", time.Since(start)), zap.Error(err))
				writeShipResult(dir, shipResult{Bytes: n, Error: err.Error()})
				return err
			}
			writeShipResult(dir, shipResult{OK: true, Bytes: n, SHA256: sum})
			log.Debug("shipRecv: done", zap.Uint64("bytes", n), zap.String("sha256", sum), zap.Duration("took", time.Since(start)))
			return nil
		},
	}
	cmd.Flags().StringVar(&device, "device", "", "Device to write")
	cmd.Flags().StringVar(&listen, "listen", "0.0.0.0:0", "Address to listen on")
	cmd.Flags().StringVar(&tokenFile, "token-file", "", "File holding the one-time token")
	cmd.Flags().StringVar(&dir, "state-dir", "", "Where ready and result.json go")
	return cmd
}

func shipSend() *cobra.Command {
	var device, to, fingerprint, tokenFile, dir string
	var size uint64
	cmd := &cobra.Command{
		Use: "send",
		RunE: func(cmd *cobra.Command, _ []string) error {
			log := zap.L()
			log.Debug("shipSend: running command", zap.String("device", device), zap.Uint64("size", size), zap.String("to", to),
				zap.String("fingerprint", fingerprint), zap.String("token_file", tokenFile), zap.String("state_dir", dir))
			token, err := readToken(tokenFile)
			if err != nil {
				log.Debug("shipSend: readToken failed", zap.Error(err))
				return err
			}
			f, err := os.Open(device)
			if err != nil {
				log.Debug("shipSend: device not openable", zap.String("device", device), zap.Error(err))
				writeShipResult(dir, shipResult{Error: err.Error()})
				return err
			}
			defer func() { _ = f.Close() }()
			var sent atomic.Uint64
			stop := make(chan struct{})
			go func() {
				t := time.NewTicker(5 * time.Second)
				defer t.Stop()
				for {
					select {
					case <-stop:
						log.Debug("shipSend: progress writer stopped")
						return
					case <-t.C:
						err := os.WriteFile(filepath.Join(dir, "progress"), []byte(strconv.FormatUint(sent.Load(), 10)), 0o600)
						log.Debug("shipSend: progress tick", zap.Uint64("sent_bytes", sent.Load()), zap.Error(err))
					}
				}
			}()
			log.Debug("shipSend: sending", zap.String("to", to), zap.Uint64("size", size))
			start := time.Now()
			sum, err := ship.Send(to, fingerprint, token, f, size, func(n uint64) { sent.Add(n) })
			close(stop)
			if err != nil {
				log.Debug("shipSend: Send failed", zap.Uint64("sent_bytes", sent.Load()), zap.Duration("took", time.Since(start)), zap.Error(err))
				writeShipResult(dir, shipResult{Bytes: sent.Load(), Error: err.Error()})
				return err
			}
			writeShipResult(dir, shipResult{OK: true, Bytes: sent.Load(), SHA256: sum})
			log.Debug("shipSend: done", zap.Uint64("sent_bytes", sent.Load()), zap.String("sha256", sum), zap.Duration("took", time.Since(start)))
			return nil
		},
	}
	cmd.Flags().StringVar(&device, "device", "", "Device to read")
	cmd.Flags().Uint64Var(&size, "size", 0, "Bytes to send")
	cmd.Flags().StringVar(&to, "to", "", "Receiver host:port")
	cmd.Flags().StringVar(&fingerprint, "fingerprint", "", "The receiver's certificate fingerprint")
	cmd.Flags().StringVar(&tokenFile, "token-file", "", "File holding the one-time token")
	cmd.Flags().StringVar(&dir, "state-dir", "", "Where progress and result.json go")
	return cmd
}
