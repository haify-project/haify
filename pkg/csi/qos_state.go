package csi

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"go.uber.org/zap"
)

// The published targets the node plugin keeps limits current for survive a
// restart of the plugin in small files, one per target. Without them a
// restarted plugin would know of no published volume until kubelet published
// it again, which for a running Pod is never.

// qosStateFile names the record of a target: a target path is too long and
// too full of slashes to be a file name itself.
func (t *qosTracker) qosStateFile(target string) string {
	sum := sha256.Sum256([]byte(target))
	return filepath.Join(t.dir, hex.EncodeToString(sum[:12])+".json")
}

func (t *qosTracker) save(rec qosRecord) {
	if t == nil || t.dir == "" {
		return
	}
	path := t.qosStateFile(rec.Target)
	data, err := json.Marshal(rec)
	if err == nil {
		err = os.MkdirAll(t.dir, 0o700)
	}
	if err == nil {
		// Written aside and renamed, so a crash never leaves half a record.
		tmp := path + ".tmp"
		if err = os.WriteFile(tmp, data, 0o600); err == nil {
			err = os.Rename(tmp, path)
		}
	}
	t.log.Debug("qosTracker.save: saved record", zap.String("path", path), zap.String("target", rec.Target), zap.Error(err))
	if err != nil {
		t.log.Warn("could not save the I/O limit record of a published volume; after a plugin restart its limits are not kept current until it is published again",
			zap.String("target", rec.Target), zap.Error(err))
	}
}

func (t *qosTracker) remove(target string) {
	if t == nil || t.dir == "" {
		return
	}
	path := t.qosStateFile(target)
	err := os.Remove(path)
	t.log.Debug("qosTracker.remove: removed record", zap.String("path", path), zap.Bool("already_gone", os.IsNotExist(err)),
		zap.Error(err))
}

// load reads the records a previous run of the plugin left.
func (t *qosTracker) load() {
	if t == nil || t.dir == "" {
		return
	}
	entries, err := os.ReadDir(t.dir)
	t.log.Debug("qosTracker.load: listed records", zap.String("dir", t.dir), zap.Int("entries", len(entries)), zap.Error(err))
	if err != nil {
		return
	}
	loaded := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		path := filepath.Join(t.dir, e.Name())
		data, err := os.ReadFile(path)
		var rec qosRecord
		if err == nil {
			err = json.Unmarshal(data, &rec)
		}
		if err != nil || rec.Target == "" || rec.VolumeID == "" {
			t.log.Debug("qosTracker.load: dropping unreadable record", zap.String("path", path), zap.Error(err))
			_ = os.Remove(path)
			continue
		}
		t.mu.Lock()
		t.records[rec.Target] = rec
		t.mu.Unlock()
		loaded++
		t.log.Debug("qosTracker.load: record", zap.String("resource", rec.VolumeID), zap.String("target", rec.Target))
	}
	t.log.Debug("qosTracker.load: done", zap.Int("loaded", loaded))
}
