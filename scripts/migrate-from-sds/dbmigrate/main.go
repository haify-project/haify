// dbmigrate copies an SDS controller database into a Haify one, renaming
// what the new controller derives from its own name. History (audit, events,
// inspections, deliveries) is copied unchanged; backups keep their resource
// names and object paths, because those name real objects in the bucket.
package main

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"time"

	bolt "go.etcd.io/bbolt"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/haify-project/haify/pkg/logging"
)

var history = map[string]bool{"audit": true, "audit_ship": true, "events": true, "inspections": true, "notify_delivery": true}

var valueRules = []string{
	"sds-meta", "haify-meta",
	"/dev/sds_", "/dev/haify_",
	`"sds_`, `"haify_`,
	`/sds_`, `/haify_`,
	"/var/lib/sds", "/var/lib/haify",
	"sds-controller.service", "haify-controller.service",
	"sds-ai.service", "haify-ai.service",
	"sds-mcp-http.service", "haify-mcp-http.service",
	`"sds.`, `"haify.`,
	`\"sds.`, `\"haify.`,
	"sdsthin", "haifythin",
}

var backupRules = []string{"/dev/sds_", "/dev/haify_", `"sds_`, `"haify_`, `/sds_`, `/haify_`, "sdsthin", "haifythin"}

// apply runs the rename pairs over s. Values can hold secrets (backup
// targets, notification channels), so only their sizes are traced.
func apply(rules []string, s string) string {
	in := len(s)
	for i := 0; i < len(rules); i += 2 {
		s = strings.ReplaceAll(s, rules[i], rules[i+1])
	}
	zap.L().Debug("apply: rules applied", zap.Int("rules", len(rules)/2), zap.Int("bytes_in", in), zap.Int("bytes_out", len(s)))
	return s
}

func renameKey(k string) string {
	orig := k
	k = strings.ReplaceAll(k, "sds-meta", "haify-meta")
	if strings.HasPrefix(k, "sds_") {
		k = "haify_" + strings.TrimPrefix(k, "sds_")
	}
	zap.L().Debug("renameKey: renamed", zap.String("key", orig), zap.String("new_key", k), zap.Bool("changed", k != orig))
	return k
}

func main() {
	// HAIFY_LOG_LEVEL=debug traces the copy on stderr; at the default level
	// nothing but the existing output is printed.
	log := logging.Console(logging.ParseLevel(os.Getenv(logging.EnvLevel), zapcore.InfoLevel))
	log.Debug("main: start", zap.Strings("args", os.Args[1:]))
	if len(os.Args) != 3 {
		log.Debug("main: wrong argument count", zap.Int("count", len(os.Args)-1))
		fmt.Fprintln(os.Stderr, "usage: dbmigrate <sds.db> <haify.db>")
		os.Exit(2)
	}
	start := time.Now()
	src, err := bolt.Open(os.Args[1], 0600, &bolt.Options{ReadOnly: true})
	log.Debug("main: opened source read-only", zap.String("path", os.Args[1]), zap.Error(err), zap.Duration("took", time.Since(start)))
	must(err)
	defer func() { _ = src.Close() }()
	if _, err := os.Stat(os.Args[2]); err == nil {
		log.Debug("main: destination already exists, refusing", zap.String("path", os.Args[2]))
		must(fmt.Errorf("%s exists", os.Args[2]))
	}
	start = time.Now()
	dst, err := bolt.Open(os.Args[2], 0600, nil)
	log.Debug("main: opened destination", zap.String("path", os.Args[2]), zap.Error(err), zap.Duration("took", time.Since(start)))
	must(err)
	// Closing flushes nothing that Update has not already committed, but a
	// failure here still means the file may not be complete.
	defer func() { must(dst.Close()) }()

	must(src.View(func(stx *bolt.Tx) error {
		return dst.Update(func(dtx *bolt.Tx) error {
			return stx.ForEach(func(name []byte, sb *bolt.Bucket) error {
				log.Debug("main: copying bucket", zap.ByteString("bucket", name))
				db, err := dtx.CreateBucket(name)
				if err != nil {
					log.Debug("main: creating bucket failed", zap.ByteString("bucket", name), zap.Error(err))
					return err
				}
				db.FillPercent = 1
				bucket := string(name)
				changed, copied := 0, 0
				err = sb.ForEach(func(k, v []byte) error {
					if v == nil {
						log.Debug("main: nested bucket, aborting", zap.String("bucket", bucket), zap.ByteString("key", k))
						return fmt.Errorf("nested bucket %s/%s not handled", bucket, k)
					}
					nk, nv := string(k), string(v)
					switch {
					case history[bucket]:
						log.Debug("main: history entry, copied unchanged", zap.String("bucket", bucket), zap.ByteString("key", k))
					case bucket == "backups":
						log.Debug("main: backup entry, renaming device paths only", zap.String("bucket", bucket), zap.ByteString("key", k))
						nv = apply(backupRules, nv)
					default:
						log.Debug("main: renaming key and value", zap.String("bucket", bucket), zap.ByteString("key", k))
						nk = renameKey(nk)
						nv = apply(valueRules, nv)
					}
					copied++
					if nk != string(k) || nv != string(v) {
						changed++
						fmt.Printf("[%s] %s%s\n", bucket, k, map[bool]string{true: " -> " + nk, false: ""}[nk != string(k)])
						if nv != string(v) {
							fmt.Printf("    %s\n", diff(string(v), nv))
						}
					}
					return db.Put([]byte(nk), []byte(nv))
				})
				if err == nil {
					seq := sb.Sequence()
					err = db.SetSequence(seq)
					log.Debug("main: bucket sequence carried over", zap.String("bucket", bucket), zap.Uint64("sequence", seq), zap.Error(err))
				}
				log.Debug("main: bucket copied", zap.String("bucket", bucket), zap.Int("count", copied),
					zap.Int("changed", changed), zap.Error(err))
				return err
			})
		})
	}))
	log.Debug("main: copy committed, checking for leftovers", zap.String("path", os.Args[2]))
	// Prove nothing derived is left outside history and backups.
	must(dst.View(func(tx *bolt.Tx) error {
		return tx.ForEach(func(name []byte, b *bolt.Bucket) error {
			if history[string(name)] || string(name) == "backups" {
				log.Debug("main: leftover check skips bucket", zap.ByteString("bucket", name))
				return nil
			}
			log.Debug("main: leftover check on bucket", zap.ByteString("bucket", name))
			return b.ForEach(func(k, v []byte) error {
				for _, bad := range []string{"sds-meta", "/var/lib/sds", "/dev/sds_", `"sds_`, `"sds.`, `\"sds.`, "sds-controller"} {
					if bytes.Contains(k, []byte(bad)) || bytes.Contains(v, []byte(bad)) {
						log.Debug("main: leftover old name", zap.ByteString("bucket", name), zap.ByteString("key", k), zap.String("pattern", bad))
						fmt.Printf("LEFT [%s] %s contains %s\n", name, k, bad)
					}
				}
				return nil
			})
		})
	}))
	log.Debug("main: done")
}

// diff shows the changed span of a value.
func diff(a, b string) string {
	zap.L().Debug("diff: start", zap.Int("old_bytes", len(a)), zap.Int("new_bytes", len(b)))
	i := 0
	for i < len(a) && i < len(b) && a[i] == b[i] {
		i++
	}
	ja, jb := len(a), len(b)
	for ja > i && jb > i && a[ja-1] == b[jb-1] {
		ja--
		jb--
	}
	lo := max(0, i-30)
	return fmt.Sprintf("…%s[%s => %s]%s…", a[lo:i], a[i:ja], b[i:jb], a[ja:min(len(a), ja+30)])
}

func must(err error) {
	if err != nil {
		zap.L().Debug("must: fatal error, exiting", zap.Error(err))
		fmt.Fprintln(os.Stderr, "dbmigrate:", err)
		os.Exit(1)
	}
}
