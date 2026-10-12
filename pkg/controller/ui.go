package controller

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/tls"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync"

	"github.com/haify-project/haify/ui"
	"go.uber.org/zap"
)

// uiNotBuiltPage is served when the binary was built without a real web UI.
// ui/dist is a build artifact: on a fresh checkout it holds only the tracked
// .gitkeep, so a plain `go build` produces a controller with no UI assets. Say
// so plainly instead of answering every request with a bare 404.
const uiNotBuiltPage = `<!DOCTYPE html>
<html><head><title>Haify — UI not built</title></head>
<body style="font-family:sans-serif;max-width:40em;margin:4em auto">
<h1>Web UI not built</h1>
<p>This <code>haify-controller</code> was compiled without the web UI assets.
The API and <code>haify</code> are unaffected.</p>
<p>To include the UI, build with <code>make build</code> (which compiles
<code>web-ui/</code> and embeds it), then restart the controller.</p>
</body></html>
`

// defaultRESTPort is where the grpc-gateway REST API listens when
// [server] rest_port is unset, and defaultAIPort where the optional AI Copilot
// (cmd/haify-ai) does. The UI proxies
// to both on loopback so a single published port serves the whole app.
// defaultGRPCPort is the fallback when the config leaves Server.Port unset.
const defaultGRPCPort = 3374

// Variables rather than constants so tests can move them off ports a real
// controller on the same machine may hold.
var (
	defaultRESTPort = 3375
	defaultAIPort   = 7634
)

// UIServer serves the embedded web UI
type UIServer struct {
	logger *zap.Logger
	server *http.Server
	distFS fs.FS
	// api and ai proxy the SPA's own-origin calls to the REST gateway and the
	// AI Copilot, which listen on their own ports.
	//
	// The UI used to build absolute URLs to those ports from
	// window.location.hostname. That works when the browser can reach the node
	// directly, and only then: put the UI behind a reverse proxy — a tunnel, a
	// VPS, anything terminating TLS on 443 — and every API call goes to
	// https://<public-name>:3375, which is not exposed and never will be. The
	// page loads and nothing on it works.
	//
	// Serving the API under the UI's own origin makes one published port enough.
	api *httputil.ReverseProxy
	ai  *httputil.ReverseProxy
	// built is false when the embedded dist holds no index.html, i.e. the
	// binary carries the placeholder rather than a real UI.
	built bool
	// gz memoises the gzip encoding of each asset. The bundle is ~1.2 MB of
	// JS and CSS, which is a second over the LAN and half a minute over a
	// long-haul link, so shipping it raw is not an option once the UI is
	// published. The dist is embedded and therefore immutable, so an encoding
	// cached here can never go stale; a nil entry means gzip made the file
	// bigger and the raw bytes should be served instead.
	gzMu sync.RWMutex
	gz   map[string][]byte
}

// NewUIServer creates a new UI server
//
// restTLS, when set, is how to dial a REST gateway served over TLS.
func NewUIServer(logger *zap.Logger, listenAddress string, port int, restPort, aiPort int, restTLS *tls.Config) (*UIServer, error) {
	// Get the subdirectory from the embed
	logger.Debug("NewUIServer: building", zap.String("listen_address", listenAddress), zap.Int("port", port),
		zap.Int("rest_port", restPort), zap.Int("ai_port", aiPort), zap.Bool("rest_tls", restTLS != nil))
	distFS, err := fs.Sub(ui.FS, "dist")
	if err != nil {
		logger.Debug("NewUIServer: embedded UI filesystem unavailable", zap.Error(err))
		return nil, fmt.Errorf("failed to get UI filesystem: %w", err)
	}

	built := true
	if _, err := fs.Stat(distFS, "index.html"); err != nil {
		built = false
		logger.Warn("Web UI assets are not embedded in this binary; serving a placeholder page. Build with `make build` to include the UI.")
	}

	mkProxy := func(p int, tlsConf *tls.Config) *httputil.ReverseProxy {
		// Always loopback: these are the controller's own listeners, and the
		// hop must not depend on how the UI itself was addressed.
		scheme := "http"
		if tlsConf != nil {
			scheme = "https"
		}
		u, _ := url.Parse(fmt.Sprintf("%s://127.0.0.1:%d", scheme, p))
		logger.Debug("NewUIServer: proxy target", zap.String("scheme", scheme), zap.Int("port", p))
		proxy := httputil.NewSingleHostReverseProxy(u)
		if tlsConf != nil {
			transport := http.DefaultTransport.(*http.Transport).Clone()
			transport.TLSClientConfig = tlsConf
			proxy.Transport = transport
		}
		return proxy
	}

	uiServer := &UIServer{
		logger: logger,
		distFS: distFS,
		built:  built,
		api:    mkProxy(restPort, restTLS),
		ai:     mkProxy(aiPort, nil),
		gz:     make(map[string][]byte),
	}

	uiAddr := fmt.Sprintf("%s:%d", listenAddress, port)
	uiServer.server = &http.Server{
		Addr:    uiAddr,
		Handler: uiServer,
	}

	logger.Debug("NewUIServer: built", zap.String("address", uiAddr), zap.Bool("ui_built", built))
	return uiServer, nil
}

func (s *UIServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Add CORS headers
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")

	// One trace per request, at whichever exit it takes. Headers are not
	// traced: they carry the bearer token.
	if r.Method == "OPTIONS" {
		s.logger.Debug("UIServer.ServeHTTP: CORS preflight", zap.String("path", r.URL.Path))
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// API and Copilot calls are same-origin so that publishing this one port is
	// enough to make the UI usable from anywhere.
	switch {
	case strings.HasPrefix(r.URL.Path, "/v1/"):
		s.logger.Debug("UIServer.ServeHTTP: proxying to REST", zap.String("method", r.Method), zap.String("path", r.URL.Path))
		s.api.ServeHTTP(w, r)
		return
	case strings.HasPrefix(r.URL.Path, "/ai/"):
		s.logger.Debug("UIServer.ServeHTTP: proxying to the AI Copilot", zap.String("method", r.Method), zap.String("path", r.URL.Path))
		s.ai.ServeHTTP(w, r)
		return
	}

	// Nothing to serve: explain why rather than 404 on every path.
	if !s.built {
		s.logger.Debug("UIServer.ServeHTTP: UI not built, placeholder served", zap.String("path", r.URL.Path))
		w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(uiNotBuiltPage))
		return
	}

	// Remove leading slash from path for filesystem lookup
	reqPath := strings.TrimPrefix(r.URL.Path, "/")

	// Determine which file to serve
	var fileToServe string
	if reqPath == "" || reqPath == "/" {
		fileToServe = "index.html"
	} else {
		// Try to open the requested file
		f, err := s.distFS.Open(reqPath)
		if err != nil {
			// File not found, serve index.html for SPA routing
			fileToServe = "index.html"
		} else {
			// The open was only a probe for existence and type; the bytes are
			// read later with fs.ReadFile. Closing a handle nothing was read
			// from cannot lose data, so the error is dropped rather than
			// turning a servable page into a 500.
			_ = f.Close()
			stat, _ := f.Stat()
			if stat.IsDir() {
				// For directories, try to serve index.html inside
				fileToServe = reqPath + "/index.html"
				// Check if that exists
				if _, err := s.distFS.Open(fileToServe); err != nil {
					fileToServe = "index.html"
				}
			} else {
				fileToServe = reqPath
			}
		}
	}

	// Read and serve the file
	data, err := fs.ReadFile(s.distFS, fileToServe)
	if err != nil {
		s.logger.Debug("UIServer.ServeHTTP: asset unreadable", zap.String("path", r.URL.Path), zap.String("file", fileToServe), zap.Error(err))
		http.Error(w, "File not found", http.StatusNotFound)
		return
	}

	// Set content type based on file extension
	contentType := "text/html; charset=utf-8"
	if strings.HasSuffix(fileToServe, ".js") {
		contentType = "application/javascript; charset=utf-8"
	} else if strings.HasSuffix(fileToServe, ".css") {
		contentType = "text/css; charset=utf-8"
	} else if strings.HasSuffix(fileToServe, ".json") {
		contentType = "application/json; charset=utf-8"
	} else if strings.HasSuffix(fileToServe, ".png") {
		contentType = "image/png"
	} else if strings.HasSuffix(fileToServe, ".jpg") || strings.HasSuffix(fileToServe, ".jpeg") {
		contentType = "image/jpeg"
	} else if strings.HasSuffix(fileToServe, ".svg") {
		contentType = "image/svg+xml"
	}
	w.Header().Set("Content-Type", contentType)

	// Everything under static/ carries a content hash in its filename, so a
	// given URL's bytes never change and the browser may keep them forever.
	// index.html must not be cached: it is what points at the current hashes,
	// and a stale copy pins the user to a bundle that may no longer exist.
	if fileToServe != "index.html" && strings.HasPrefix(fileToServe, "static/") {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	}

	// Set even when the response goes out uncompressed: a shared cache that
	// stored those bytes without it would go on serving them to clients that
	// could have had the compressed ones.
	if compressible(contentType) {
		w.Header().Set("Vary", "Accept-Encoding")
	}
	if enc, ok := s.encodedFor(r, fileToServe, contentType, data); ok {
		w.Header().Set("Content-Encoding", "gzip")
		data = enc
	}
	s.logger.Debug("UIServer.ServeHTTP: serving asset", zap.String("path", r.URL.Path), zap.String("file", fileToServe),
		zap.String("content_type", contentType), zap.Int("bytes", len(data)), zap.Bool("gzip", w.Header().Get("Content-Encoding") == "gzip"))
	// Set explicitly so the browser can show a progress bar rather than
	// streaming a chunked response of unknown length.
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	_, _ = w.Write(data)
}

// encodedFor returns the gzip encoding of an asset when the client accepts it
// and compressing actually pays.
func (s *UIServer) encodedFor(r *http.Request, name, contentType string, data []byte) ([]byte, bool) {
	if !compressible(contentType) {
		return nil, false
	}
	if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
		return nil, false
	}
	enc, ok := s.gzipped(name, data)
	return enc, ok
}

// compressible reports whether gzipping this content type is worth the CPU.
// PNG, JPEG and the like are already compressed; re-encoding them burns time
// on a storage node to save nothing.
func compressible(contentType string) bool {
	return strings.HasPrefix(contentType, "text/") ||
		strings.HasPrefix(contentType, "application/javascript") ||
		strings.HasPrefix(contentType, "application/json") ||
		strings.HasPrefix(contentType, "image/svg+xml")
}

// gzipped compresses an asset on first use and memoises the result.
func (s *UIServer) gzipped(name string, data []byte) ([]byte, bool) {
	s.gzMu.RLock()
	enc, cached := s.gz[name]
	s.gzMu.RUnlock()
	if cached {
		return enc, enc != nil
	}

	var buf bytes.Buffer
	zw, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err == nil {
		if _, err = zw.Write(data); err == nil {
			err = zw.Close()
		}
	}
	enc = buf.Bytes()
	// Storing nil records the negative result too, so a file that does not
	// benefit is not re-compressed on every request just to be discarded.
	if err != nil || len(enc) >= len(data) {
		enc = nil
	}

	s.gzMu.Lock()
	s.gz[name] = enc
	s.gzMu.Unlock()
	// Once per asset: later requests hit the cache above.
	s.logger.Debug("UIServer.gzipped: compressed asset", zap.String("file", name), zap.Int("bytes", len(data)),
		zap.Int("gzip_bytes", len(enc)), zap.Bool("worth_it", enc != nil), zap.Error(err))
	return enc, enc != nil
}

// Start starts the UI server in a goroutine
func (s *UIServer) Start() error {
	s.logger.Debug("UIServer.Start: listening", zap.String("address", s.server.Addr))
	listener, err := net.Listen("tcp", s.server.Addr)
	if err != nil {
		s.logger.Debug("UIServer.Start: listen failed", zap.String("address", s.server.Addr), zap.Error(err))
		return err
	}

	s.logger.Info("UI server listening", zap.String("address", s.server.Addr))
	go func() {
		if err := s.server.Serve(listener); err != nil && err != http.ErrServerClosed {
			s.logger.Error("UI server error", zap.Error(err))
		} else {
			s.logger.Debug("UIServer.Start: server stopped", zap.Error(err))
		}
	}()

	return nil
}

// Shutdown stops the UI server
func (s *UIServer) Shutdown(ctx context.Context) error {
	s.logger.Info("Stopping UI server")
	err := s.server.Shutdown(ctx)
	s.logger.Debug("UIServer.Shutdown: stopped", zap.Error(err))
	return err
}

// restPort is the configured REST port, or the default when none is set.
func (c *Controller) restPort() int {
	if c.config != nil && c.config.Server.RESTPort > 0 {
		c.traceLogger().Debug("restPort: configured", zap.Int("port", c.config.Server.RESTPort))
		return c.config.Server.RESTPort
	}
	c.traceLogger().Debug("restPort: default", zap.Int("port", defaultRESTPort))
	return defaultRESTPort
}
