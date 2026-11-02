// Command haify-installer is the web face of deploy/install/haify-install.sh.
// It serves a wizard on a random port behind a one-time token and runs the
// script for everything: --probe-json to show the nodes and their disks,
// --dry-run for the review, and the install itself, whose output it keeps so
// the page can be closed and opened again while it runs.
package main

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

//go:embed web
var webFS embed.FS

type server struct {
	script string
	token  string
	job    *job
}

func main() {
	listen := flag.String("listen", "", "address to serve on (default: every address, a free port from 38000 to 38999)")
	script := flag.String("script", "", "haify-install.sh (default: the one in this bundle)")
	flag.Parse()

	if *script == "" {
		exe, err := os.Executable()
		if err != nil {
			log.Fatal(err)
		}
		*script = filepath.Join(filepath.Dir(exe), "..", "deploy", "install", "haify-install.sh")
	}
	if _, err := os.Stat(*script); err != nil {
		log.Fatalf("installer script: %v", err)
	}
	ln, err := listener(*listen)
	if err != nil {
		log.Fatal(err)
	}
	s := &server{script: *script, token: newToken(), job: newJob()}
	port := ln.Addr().(*net.TCPAddr).Port
	fmt.Printf("\nOpen the Haify installer in a browser:\n\n")
	for _, ip := range localIPs() {
		fmt.Printf("  http://%s:%d/?t=%s\n", ip, port, s.token)
	}
	fmt.Printf("\nIt runs until you stop it (Ctrl+C); an install it started keeps running if the page is closed.\n")

	srv := &http.Server{Handler: s.routes(), ReadHeaderTimeout: 10 * time.Second}
	log.Fatal(srv.Serve(ln))
}

// listener binds the given address, or the first free port from 38000.
func listener(addr string) (net.Listener, error) {
	if addr != "" {
		return net.Listen("tcp", addr)
	}
	for p := 38000; p < 39000; p++ {
		if ln, err := net.Listen("tcp", fmt.Sprintf(":%d", p)); err == nil {
			return ln, nil
		}
	}
	return nil, fmt.Errorf("no free port from 38000 to 38999")
}

func newToken() string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		log.Fatal(err)
	}
	return hex.EncodeToString(b)
}

// localIPs: the addresses a browser elsewhere could use, IPv4 first.
func localIPs() []string {
	var v4, v6 []string
	ifaces, _ := net.Interfaces()
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || virtualBridge(ifc.Name) {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok || ipn.IP.IsLoopback() || ipn.IP.IsLinkLocalUnicast() {
				continue
			}
			if ipn.IP.To4() != nil {
				v4 = append(v4, ipn.IP.String())
			} else {
				v6 = append(v6, "["+ipn.IP.String()+"]")
			}
		}
	}
	if len(v4)+len(v6) == 0 {
		return []string{"127.0.0.1"}
	}
	return append(v4, v6...)
}

// virtualBridge: interfaces whose addresses only this host's guests and
// containers can reach (libvirt's NAT bridge, Docker, CNI and Podman).
func virtualBridge(name string) bool {
	for _, p := range []string{"virbr", "docker", "br-", "cni", "flannel", "podman", "lxcbr", "veth"} {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	static, _ := fs.Sub(webFS, "web")
	mux.Handle("GET /", http.FileServerFS(static))
	mux.HandleFunc("GET /api/probe", s.probe)
	mux.HandleFunc("POST /api/plan", s.plan)
	mux.HandleFunc("POST /api/install", s.install)
	mux.HandleFunc("GET /api/job", s.jobStatus)
	return s.auth(mux)
}

// auth admits the browser that opened the printed address: the token in it
// becomes a cookie, which every later request carries.
func (s *server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if t := r.URL.Query().Get("t"); t == s.token {
			http.SetCookie(w, &http.Cookie{Name: "haify_install", Value: s.token, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode})
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		if c, err := r.Cookie("haify_install"); err != nil || c.Value != s.token {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte("Open the address the installer printed in the terminal; it carries the token."))
			return
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// run runs the script to completion and returns what it printed.
func (s *server) run(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "bash", append([]string{s.script}, args...)...)
	cmd.Dir = workDir()
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// workDir is where the script writes files for the platform to apply.
func workDir() string {
	d := "/var/lib/haify/install"
	if err := os.MkdirAll(d, 0o755); err != nil {
		return os.TempDir()
	}
	return d
}

func (s *server) probe(w http.ResponseWriter, r *http.Request) {
	args := []string{"--probe-json"}
	if n := strings.TrimSpace(r.URL.Query().Get("nodes")); n != "" {
		args = append(args, "--nodes", n)
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", append([]string{s.script}, args...)...)
	cmd.Dir = workDir()
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": lastLines(stderr.String(), 5)})
		return
	}
	if !json.Valid(out) {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "the installer's node check printed something other than JSON: " + lastLines(string(out), 3)})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(out)
}

func (s *server) plan(w http.ResponseWriter, r *http.Request) {
	var c choices
	if err := json.NewDecoder(r.Body).Decode(&c); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	args, err := c.args()
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	out, err := s.run(ctx, append(args, "--dry-run", "--yes")...)
	writeJSON(w, http.StatusOK, map[string]any{"ok": err == nil, "output": out})
}

func (s *server) install(w http.ResponseWriter, r *http.Request) {
	var c choices
	if err := json.NewDecoder(r.Body).Decode(&c); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	args, err := c.args()
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if err := s.job.start(s.script, append(args, "--yes"), c.Env); err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]bool{"started": true})
}

func (s *server) jobStatus(w http.ResponseWriter, r *http.Request) {
	from := 0
	_, _ = fmt.Sscanf(r.URL.Query().Get("from"), "%d", &from)
	writeJSON(w, http.StatusOK, s.job.status(from))
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
