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
	_ = os.WriteFile(filepath.Join(dir, "result.json"), b, 0o600)
}

func readToken(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
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
			token, err := readToken(tokenFile)
			if err != nil {
				return err
			}
			r, err := ship.Listen(listen, token)
			if err != nil {
				writeShipResult(dir, shipResult{Error: err.Error()})
				return err
			}
			defer func() { _ = r.Close() }()
			port := r.Addr().(*net.TCPAddr).Port
			if err := os.WriteFile(filepath.Join(dir, "ready"), fmt.Appendf(nil, "%s %d\n", r.Fingerprint, port), 0o600); err != nil {
				return err
			}
			f, err := os.OpenFile(device, os.O_WRONLY, 0)
			if err != nil {
				writeShipResult(dir, shipResult{Error: err.Error()})
				return err
			}
			defer func() { _ = f.Close() }()
			n, sum, err := r.Receive(f, f.Sync)
			if err != nil {
				writeShipResult(dir, shipResult{Bytes: n, Error: err.Error()})
				return err
			}
			writeShipResult(dir, shipResult{OK: true, Bytes: n, SHA256: sum})
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
			token, err := readToken(tokenFile)
			if err != nil {
				return err
			}
			f, err := os.Open(device)
			if err != nil {
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
						return
					case <-t.C:
						_ = os.WriteFile(filepath.Join(dir, "progress"), []byte(strconv.FormatUint(sent.Load(), 10)), 0o600)
					}
				}
			}()
			sum, err := ship.Send(to, fingerprint, token, f, size, func(n uint64) { sent.Add(n) })
			close(stop)
			if err != nil {
				writeShipResult(dir, shipResult{Bytes: sent.Load(), Error: err.Error()})
				return err
			}
			writeShipResult(dir, shipResult{OK: true, Bytes: sent.Load(), SHA256: sum})
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
