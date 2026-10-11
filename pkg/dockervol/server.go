package dockervol

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"
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
			var r request
			if req.Body != nil {
				_ = json.NewDecoder(req.Body).Decode(&r)
			}
			ctx, cancel := context.WithTimeout(req.Context(), callTimeout)
			defer cancel()
			out, err := fn(ctx, r)
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
	if err := os.MkdirAll(filepath.Dir(socket), 0o755); err != nil {
		return nil, err
	}
	_ = os.Remove(socket)
	return net.Listen("unix", socket)
}
