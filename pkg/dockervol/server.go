package dockervol

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"go.uber.org/zap"
)

// Docker's volume plugin protocol: every call is a POST with a JSON body, and
// every answer is JSON whose Err field, when set, is the failure Docker shows.

const contentType = "application/vnd.docker.plugins.v1.2+json"

type request struct {
	Name string            `json:"Name"`
	ID   string            `json:"ID"`
	Opts map[string]string `json:"Opts"`
}

type dockerVolume struct {
	Name       string         `json:"Name"`
	Mountpoint string         `json:"Mountpoint,omitempty"`
	Status     map[string]any `json:"Status,omitempty"`
}

func toDocker(v volumeInfo) *dockerVolume {
	return &dockerVolume{Name: v.Name, Mountpoint: v.Mountpoint, Status: v.Status}
}

// callTimeout bounds one call: Mount can promote, attach and format.
const callTimeout = 3 * time.Minute

// Handler serves the protocol for d.
func Handler(d *Driver) http.Handler {
	mux := http.NewServeMux()
	reply := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", contentType)
		_ = json.NewEncoder(w).Encode(v)
	}
	fail := func(w http.ResponseWriter, err error) { reply(w, map[string]string{"Err": err.Error()}) }
	handle := func(path string, fn func(ctx context.Context, r request) (any, error)) {
		mux.HandleFunc(path, func(w http.ResponseWriter, req *http.Request) {
			log := zap.L()
			var r request
			if req.Body != nil {
				if derr := json.NewDecoder(req.Body).Decode(&r); derr != nil {
					log.Debug("Handler: request body not decoded", zap.String("call", path), zap.Error(derr))
				}
			}
			log.Debug("Handler: call", zap.String("call", path), zap.String("volume", r.Name),
				zap.String("container", r.ID), zap.Int("opts", len(r.Opts)))
			ctx, cancel := context.WithTimeout(req.Context(), callTimeout)
			defer cancel()
			start := time.Now()
			out, err := fn(ctx, r)
			log.Debug("Handler: call done", zap.String("call", path), zap.String("volume", r.Name),
				zap.Duration("took", time.Since(start)), zap.Error(err))
			if err != nil {
				fail(w, err)
				return
			}
			reply(w, out)
		})
	}

	handle("/Plugin.Activate", func(context.Context, request) (any, error) {
		return map[string][]string{"Implements": {"VolumeDriver"}}, nil
	})
	handle("/VolumeDriver.Capabilities", func(context.Context, request) (any, error) {
		// Global: the volume is the same on every host, so Swarm may schedule
		// a task that uses it anywhere.
		return map[string]any{"Capabilities": map[string]string{"Scope": "global"}}, nil
	})
	handle("/VolumeDriver.Create", func(ctx context.Context, r request) (any, error) {
		return map[string]string{}, d.Create(ctx, r.Name, r.Opts)
	})
	handle("/VolumeDriver.Remove", func(ctx context.Context, r request) (any, error) {
		return map[string]string{}, d.Remove(ctx, r.Name)
	})
	handle("/VolumeDriver.Get", func(ctx context.Context, r request) (any, error) {
		v, err := d.Get(ctx, r.Name)
		if err != nil {
			return nil, err
		}
		return map[string]any{"Volume": toDocker(v)}, nil
	})
	handle("/VolumeDriver.List", func(ctx context.Context, _ request) (any, error) {
		vs, err := d.List(ctx)
		if err != nil {
			return nil, err
		}
		out := make([]*dockerVolume, 0, len(vs))
		for _, v := range vs {
			out = append(out, toDocker(v))
		}
		return map[string]any{"Volumes": out}, nil
	})
	handle("/VolumeDriver.Path", func(_ context.Context, r request) (any, error) {
		return map[string]string{"Mountpoint": d.Path(r.Name)}, nil
	})
	handle("/VolumeDriver.Mount", func(ctx context.Context, r request) (any, error) {
		mp, err := d.Mount(ctx, r.Name, r.ID)
		return map[string]string{"Mountpoint": mp}, err
	})
	handle("/VolumeDriver.Unmount", func(ctx context.Context, r request) (any, error) {
		return map[string]string{}, d.Unmount(ctx, r.Name, r.ID)
	})
	return mux
}

// Listen opens the plugin socket Docker discovers, replacing a stale one.
func Listen(socket string) (net.Listener, error) {
	log := zap.L()
	log.Debug("Listen: opening plugin socket", zap.String("socket", socket))
	if err := os.MkdirAll(filepath.Dir(socket), 0o755); err != nil {
		log.Debug("Listen: creating socket directory failed", zap.String("dir", filepath.Dir(socket)), zap.Error(err))
		return nil, err
	}
	if err := os.Remove(socket); err == nil {
		log.Debug("Listen: removed stale socket", zap.String("socket", socket))
	}
	ln, err := net.Listen("unix", socket)
	log.Debug("Listen: listen returned", zap.String("socket", socket), zap.Error(err))
	return ln, err
}
