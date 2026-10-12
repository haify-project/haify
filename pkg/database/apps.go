package database

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	bolt "go.etcd.io/bbolt"
	"go.uber.org/zap"
)

// Applications (`haify app`): a single-instance PostgreSQL, MySQL, Redis or
// RustFS on a DRBD resource, failed over by drbd-reactor. The promoter config
// and unit on the nodes are what run; this record is how they were made —
// the binaries and ids every node agreed on at creation included — so they
// can be written again to a replica added later, and shown.

const appsBucket = "apps"

// App is one database application.
type App struct {
	Name      string
	Engine    string
	Resource  string
	ServiceIP string
	Port      int
	Vector    bool `json:",omitempty"`
	// Device is the DRBD device the app's filesystem lives on.
	Device string
	// Binaries the nodes agreed on when the app was created.
	Server  string
	Client  string
	Admin   string `json:",omitempty"`
	Init    string `json:",omitempty"`
	Flavor  string `json:",omitempty"`
	Version string `json:",omitempty"`
	// UID and GID of the daemon user, the same on every node.
	UID       int
	GID       int
	CreatedAt time.Time
	UpdatedAt time.Time
}

// SaveApp creates or replaces an app record.
func (db *DB) SaveApp(ctx context.Context, app *App) error {
	if app == nil || app.Name == "" {
		db.log().Debug("DB.SaveApp: refusing app without a name", zap.Bool("nil_app", app == nil))
		return fmt.Errorf("app name is required")
	}
	db.log().Debug("DB.SaveApp: saving app",
		zap.String("app", app.Name), zap.String("engine", app.Engine), zap.String("resource", app.Resource),
		zap.String("service_ip", app.ServiceIP), zap.Int("port", app.Port), zap.String("device", app.Device))
	db.mu.Lock()
	defer db.mu.Unlock()

	now := time.Now()
	if app.CreatedAt.IsZero() {
		app.CreatedAt = now
	}
	app.UpdatedAt = now
	data, err := json.Marshal(app)
	if err != nil {
		db.log().Debug("DB.SaveApp: marshal failed", zap.String("app", app.Name), zap.Error(err))
		return fmt.Errorf("failed to marshal app: %w", err)
	}
	start := time.Now()
	err = db.db.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists([]byte(appsBucket))
		if err != nil {
			return err
		}
		return b.Put([]byte(app.Name), data)
	})
	db.log().Debug("DB.SaveApp: put", zap.String("bucket", appsBucket), zap.String("key", app.Name),
		zap.Int("size_bytes", len(data)), zap.Duration("took", time.Since(start)), zap.Error(err))
	return err
}

// GetApp returns the app called name, or nil when there is none.
func (db *DB) GetApp(ctx context.Context, name string) (*App, error) {
	db.log().Debug("DB.GetApp: reading app", zap.String("app", name))
	db.mu.RLock()
	defer db.mu.RUnlock()

	var app *App
	size := 0
	start := time.Now()
	err := db.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(appsBucket))
		if b == nil {
			return nil
		}
		data := b.Get([]byte(name))
		if data == nil {
			return nil
		}
		size = len(data)
		app = &App{}
		return json.Unmarshal(data, app)
	})
	db.log().Debug("DB.GetApp: get", zap.String("bucket", appsBucket), zap.String("key", name),
		zap.Bool("found", app != nil), zap.Int("size_bytes", size),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return nil, fmt.Errorf("failed to read app %s: %w", name, err)
	}
	return app, nil
}

// ListApps returns every app, by name.
func (db *DB) ListApps(ctx context.Context) ([]*App, error) {
	db.log().Debug("DB.ListApps: listing apps")
	db.mu.RLock()
	defer db.mu.RUnlock()

	var apps []*App
	start := time.Now()
	err := db.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(appsBucket))
		if b == nil {
			db.log().Debug("DB.ListApps: apps bucket absent", zap.String("bucket", appsBucket))
			return nil
		}
		return b.ForEach(func(k, v []byte) error {
			var app App
			if err := json.Unmarshal(v, &app); err != nil {
				return fmt.Errorf("app %s: %w", k, err)
			}
			apps = append(apps, &app)
			return nil
		})
	})
	db.log().Debug("DB.ListApps: scan", zap.String("bucket", appsBucket), zap.Int("count", len(apps)),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return nil, fmt.Errorf("failed to list apps: %w", err)
	}
	sort.Slice(apps, func(i, j int) bool { return apps[i].Name < apps[j].Name })
	return apps, nil
}

// GetAppByResource returns the app running on resource, or nil.
func (db *DB) GetAppByResource(ctx context.Context, resource string) (*App, error) {
	db.log().Debug("DB.GetAppByResource: looking up app", zap.String("resource", resource))
	apps, err := db.ListApps(ctx)
	if err != nil {
		db.log().Debug("DB.GetAppByResource: listing apps failed", zap.String("resource", resource), zap.Error(err))
		return nil, err
	}
	for _, a := range apps {
		if a.Resource == resource {
			db.log().Debug("DB.GetAppByResource: matched", zap.String("resource", resource), zap.String("app", a.Name))
			return a, nil
		}
	}
	db.log().Debug("DB.GetAppByResource: no app on resource", zap.String("resource", resource), zap.Int("apps", len(apps)))
	return nil, nil
}

// DeleteApp removes an app record. Removing one that does not exist is not
// an error.
func (db *DB) DeleteApp(ctx context.Context, name string) error {
	db.log().Debug("DB.DeleteApp: deleting app", zap.String("app", name))
	db.mu.Lock()
	defer db.mu.Unlock()
	start := time.Now()
	err := db.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(appsBucket))
		if b == nil {
			db.log().Debug("DB.DeleteApp: apps bucket absent", zap.String("bucket", appsBucket))
			return nil
		}
		return b.Delete([]byte(name))
	})
	db.log().Debug("DB.DeleteApp: delete", zap.String("bucket", appsBucket), zap.String("key", name),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	return err
}
