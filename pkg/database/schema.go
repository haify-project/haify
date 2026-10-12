package database

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"

	bolt "go.etcd.io/bbolt"
	"go.uber.org/zap"
)

// The controller's entire inventory — nodes, pools, resources, volumes,
// gateways, HA configs, RBAC, audit trail, backup targets, notification
// channels — lives in one bbolt file on the metadata volume. Until this file
// existed, nothing on disk recorded which shape that inventory had been written
// in, so an upgrade simply opened whatever was there and hoped: a renamed
// bucket reads as absent, a changed struct field deserializes to its zero
// value, and neither is an error. The controller then reports a smaller or
// subtly different cluster than the one that exists, and the first write it
// makes bakes that in.
//
// Worse than an upgrade is a rollback. Reinstalling the previous binary is a
// routine, low-drama operation for an operator, and it is the one path where an
// older controller writes a database laid out by a newer one — silently
// dropping whatever the newer schema added. That is why the version check
// refuses to open a database from the future outright; it matters more than the
// migrations themselves.

const (
	// metaBucket holds the database's own bookkeeping. The leading underscore
	// keeps it out of the namespace the data buckets use, so it can never
	// collide with a future entity bucket.
	metaBucket = "_meta"
	// schemaVersionKey stores the version as decimal ASCII rather than a packed
	// integer, so an operator staring at a broken database can read it out with
	// strings(1) without a bbolt-aware tool.
	schemaVersionKey = "schema_version"
)

// legacySchemaVersion is the version assigned to a database that predates this
// mechanism.
//
// The distinction that matters here is "existing database with no version
// record" versus "brand new database". Both are missing the key, and getting it
// wrong is destructive in one direction: stamping an existing production
// database with the current version marks migrations as done that never ran,
// and the shape mismatch that migration was meant to fix stays and never gets
// another chance. So a file that already held a database is read as version 1,
// never as fresh.
const legacySchemaVersion = 1

// ErrSchemaIncompatible marks every condition under which this binary must not
// touch the database on disk. Callers gate on it with IsSchemaIncompatible to
// distinguish "cannot safely use this database" — which has to stop the
// process — from ordinary open failures.
var ErrSchemaIncompatible = errors.New("incompatible database schema")

// IncompatibleSchemaError describes a database this binary refuses to open.
type IncompatibleSchemaError struct {
	// Path is the database file.
	Path string
	// Found is the schema version on disk, or -1 when the stored value could
	// not be parsed.
	Found int
	// Supported is the highest version this binary understands.
	Supported int
	// Raw is the unparseable stored value; set only when Found is -1.
	Raw string
}

func (e *IncompatibleSchemaError) Error() string {
	if e.Found < 0 {
		return fmt.Sprintf("database %s has an unreadable schema version record (%q); "+
			"refusing to open it rather than guess at its layout — restore a known-good copy "+
			"(look for %s.v*.bak, written before each schema upgrade) before starting the controller",
			e.Path, e.Raw, e.Path)
	}
	return fmt.Sprintf("database %s has schema version %d but this controller supports at most version %d; "+
		"refusing to open it, because an older binary writing a newer database drops whatever the newer "+
		"schema added, silently and irreversibly — run a controller new enough for version %d, or restore "+
		"the pre-upgrade copy left beside the database (%s.v*.bak) before starting this one",
		e.Path, e.Found, e.Supported, e.Found, e.Path)
}

// Unwrap lets callers match with errors.Is(err, ErrSchemaIncompatible).
func (e *IncompatibleSchemaError) Unwrap() error { return ErrSchemaIncompatible }

// IsSchemaIncompatible reports whether err means the database on disk must not
// be used by this binary. It is a start-up-fatal condition: unlike a missing
// file or a locked database, no amount of retrying or degrading makes it safe.
func IsSchemaIncompatible(err error) bool { return errors.Is(err, ErrSchemaIncompatible) }

// migration is one step of the chain, taking the database from version to-1 to
// version to.
//
// apply runs inside the same write transaction that stamps the new version, so
// a step and its version bump land together or not at all. A step must tolerate
// buckets that do not exist: a database that has been sitting at version 1
// since before a bucket was introduced is exactly the case migrations exist
// for, and CreateBucketIfNotExists is the correct reflex here, not Bucket().
type migration struct {
	// to is the version the database is at once this step commits.
	to int
	// name appears in the logs and in failure messages; keep it descriptive
	// enough to identify the step without reading the code.
	name string
	// apply performs the change. It must be idempotent-safe to *re-run from the
	// previous version* — it never re-runs against a version it already
	// produced, because the stamp commits with it.
	apply func(tx *bolt.Tx) error
}

// migrations is the ordered chain, each step taking the database one version
// forward. It is a var rather than a const-like literal only so tests can
// exercise the runner against synthetic chains.
//
// It is deliberately empty. Version 1 is the schema as it has shipped to date;
// there is no structural change to make right now, and inventing one to prove
// the machinery works would rewrite production metadata for no reason. New
// steps append here and never renumber, since the numbers are already on disk
// in the field.
var migrations = []migration{}

// SchemaVersion is the schema version this binary writes and understands: the
// last step in the chain, or the pre-versioning baseline when there are none.
func SchemaVersion() int {
	if len(migrations) == 0 {
		return legacySchemaVersion
	}
	return migrations[len(migrations)-1].to
}

// validateMigrationChain rejects a chain with a gap, a repeat, or a step that
// starts below the baseline. A mis-numbered chain would either skip a step or
// re-run one against a database it has already transformed, so it is caught
// before anything is written rather than discovered halfway through an upgrade
// on a production cluster.
func validateMigrationChain(chain []migration) error {
	zap.L().Debug("validateMigrationChain: checking chain", zap.Int("count", len(chain)))
	want := legacySchemaVersion + 1
	for i, m := range chain {
		zap.L().Debug("validateMigrationChain: checking step",
			zap.Int("index", i), zap.Int("to_version", m.to), zap.String("name", m.name), zap.Int("expected_version", want))
		if m.to != want {
			zap.L().Debug("validateMigrationChain: step targets the wrong version",
				zap.Int("index", i), zap.Int("to_version", m.to), zap.Int("expected_version", want))
			return fmt.Errorf("migration chain is malformed: step %d targets version %d, expected %d", i, m.to, want)
		}
		if m.name == "" {
			zap.L().Debug("validateMigrationChain: step has no name", zap.Int("to_version", m.to))
			return fmt.Errorf("migration chain is malformed: step to version %d has no name", m.to)
		}
		if m.apply == nil {
			zap.L().Debug("validateMigrationChain: step has no apply function",
				zap.Int("to_version", m.to), zap.String("name", m.name))
			return fmt.Errorf("migration chain is malformed: step %q to version %d has no apply function", m.name, m.to)
		}
		want++
	}
	zap.L().Debug("validateMigrationChain: chain is well formed", zap.Int("count", len(chain)))
	return nil
}

// readSchemaVersion returns the version recorded in the database and whether a
// record was present at all.
func readSchemaVersion(bdb *bolt.DB, path string) (version int, present bool, err error) {
	zap.L().Debug("readSchemaVersion: reading schema version", zap.String("path", path))
	start := time.Now()
	err = bdb.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(metaBucket))
		if b == nil {
			zap.L().Debug("readSchemaVersion: meta bucket absent", zap.String("bucket", metaBucket))
			return nil
		}
		raw := b.Get([]byte(schemaVersionKey))
		if raw == nil {
			zap.L().Debug("readSchemaVersion: no version record",
				zap.String("bucket", metaBucket), zap.String("key", schemaVersionKey))
			return nil
		}
		v, perr := strconv.Atoi(string(raw))
		if perr != nil || v < 1 {
			zap.L().Debug("readSchemaVersion: version record is unreadable",
				zap.String("bucket", metaBucket), zap.String("key", schemaVersionKey),
				zap.Int("size_bytes", len(raw)), zap.Error(perr))
			return &IncompatibleSchemaError{Path: path, Found: -1, Supported: SchemaVersion(), Raw: string(raw)}
		}
		version, present = v, true
		return nil
	})
	zap.L().Debug("readSchemaVersion: get", zap.String("bucket", metaBucket), zap.String("key", schemaVersionKey),
		zap.Bool("found", present), zap.Int("version", version),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return 0, false, err
	}
	return version, present, nil
}

// putSchemaVersion stamps the version inside an existing write transaction, so
// it can commit atomically with the migration that earned it.
func putSchemaVersion(tx *bolt.Tx, version int) error {
	zap.L().Debug("putSchemaVersion: stamping schema version", zap.Int("version", version))
	b, err := tx.CreateBucketIfNotExists([]byte(metaBucket))
	if err != nil {
		zap.L().Debug("putSchemaVersion: creating meta bucket failed", zap.String("bucket", metaBucket), zap.Error(err))
		return fmt.Errorf("failed to create %s bucket: %w", metaBucket, err)
	}
	err = b.Put([]byte(schemaVersionKey), []byte(strconv.Itoa(version)))
	zap.L().Debug("putSchemaVersion: put", zap.String("bucket", metaBucket), zap.String("key", schemaVersionKey),
		zap.Int("version", version), zap.Error(err))
	return err
}

// backupBeforeMigration copies the database aside before the chain runs.
//
// The trade-off is disk against recoverability, and it is lopsided here. This
// file is the cluster's only record of what exists; the audit trail is capped
// at tens of megabytes ([audit] max_entries) and everything else in it is
// small, so a copy costs little on the metadata volume. Losing it
// costs every gateway, HA config and RBAC assignment the operator ever
// declared, none of which can be reconstructed from the storage nodes.
//
// Three deliberate choices follow from that:
//
//   - The copy is taken only when a migration is actually going to run. Copying
//     on every start would double the write volume of a restart loop to protect
//     against nothing.
//   - The name carries the source version, not a timestamp, so an upgrade that
//     fails and is retried overwrites its own copy instead of filling the
//     volume with one file per attempt.
//   - It is written to a .partial name and renamed into place. A truncated copy
//     that looks like a restore point is worse than no restore point at all.
//
// The copies are never deleted automatically: after a successful migration this
// is the only way back to the pre-migration state, which is exactly what an
// operator rolling a release back needs.
func backupBeforeMigration(bdb *bolt.DB, path string, from int) (string, error) {
	dst := fmt.Sprintf("%s.v%d.bak", path, from)
	tmp := dst + ".partial"
	zap.L().Debug("backupBeforeMigration: copying database aside",
		zap.String("path", path), zap.Int("from_version", from), zap.String("dest", dst))
	start := time.Now()

	// View holds a consistent snapshot for the length of the copy, so the
	// backup is a valid database even though writers are not stopped.
	// Both removals are best-effort cleanup on a path that already has a real
	// error to report. Whether the half-written .partial went away does not
	// change the outcome — the caller is being told the backup failed either
	// way — and the name is deterministic, so the next attempt overwrites any
	// leftover instead of accumulating files. Reporting a cleanup failure here
	// would replace the error that explains what actually went wrong.
	if err := bdb.View(func(tx *bolt.Tx) error { return tx.CopyFile(tmp, 0600) }); err != nil {
		zap.L().Debug("backupBeforeMigration: copy failed", zap.String("tmp", tmp),
			zap.Duration("took", time.Since(start)), zap.Error(err))
		_ = os.Remove(tmp)
		return "", fmt.Errorf("failed to copy database to %s: %w", tmp, err)
	}
	zap.L().Debug("backupBeforeMigration: copy written", zap.String("tmp", tmp), zap.Duration("took", time.Since(start)))
	if err := os.Rename(tmp, dst); err != nil {
		zap.L().Debug("backupBeforeMigration: rename failed", zap.String("tmp", tmp), zap.String("dest", dst), zap.Error(err))
		_ = os.Remove(tmp)
		return "", fmt.Errorf("failed to move database copy into place at %s: %w", dst, err)
	}
	zap.L().Debug("backupBeforeMigration: copy in place", zap.String("dest", dst), zap.Duration("took", time.Since(start)))
	return dst, nil
}

// ensureSchema brings the opened database up to the version this binary
// understands, or refuses to use it.
//
// preexisting says whether the file already held a database before bolt.Open
// touched it; it is what separates an unversioned production database from a
// brand new one. It has to be sampled by the caller before the file is opened,
// because opening creates it.
func ensureSchema(bdb *bolt.DB, path string, preexisting bool, logger *zap.Logger) error {
	if logger == nil {
		logger = zap.NewNop()
	}
	logger.Debug("ensureSchema: checking schema", zap.String("path", path), zap.Bool("preexisting", preexisting))
	if err := validateMigrationChain(migrations); err != nil {
		logger.Debug("ensureSchema: migration chain invalid", zap.Error(err))
		return err
	}
	target := SchemaVersion()

	current, present, err := readSchemaVersion(bdb, path)
	if err != nil {
		logger.Debug("ensureSchema: reading schema version failed", zap.String("path", path), zap.Error(err))
		return err
	}
	logger.Debug("ensureSchema: versions",
		zap.Int("current_version", current), zap.Bool("present", present), zap.Int("target_version", target))

	switch {
	case present:
		// Nothing to infer.
		logger.Debug("ensureSchema: version record present", zap.Int("current_version", current))
	case preexisting:
		current = legacySchemaVersion
		logger.Info("Database has no schema version record; assuming the pre-versioning schema",
			zap.String("path", path),
			zap.Int("assumed_version", legacySchemaVersion),
			zap.Int("target_version", target))
	default:
		// A database this process just created is by definition in the current
		// shape, so it is stamped rather than migrated.
		logger.Debug("ensureSchema: new database, stamping current version", zap.Int("version", target))
		if err := bdb.Update(func(tx *bolt.Tx) error { return putSchemaVersion(tx, target) }); err != nil {
			logger.Debug("ensureSchema: stamping new database failed", zap.Error(err))
			return fmt.Errorf("failed to record schema version: %w", err)
		}
		logger.Info("Initialized database schema", zap.String("path", path), zap.Int("version", target))
		return nil
	}

	if current > target {
		logger.Debug("ensureSchema: database is newer than this binary, refusing",
			zap.Int("current_version", current), zap.Int("target_version", target))
		return &IncompatibleSchemaError{Path: path, Found: current, Supported: target}
	}

	if current == target {
		logger.Debug("ensureSchema: schema already current",
			zap.Int("version", current), zap.Bool("record_present", present))
		if !present {
			// An unversioned database that is already in the current shape
			// still needs the record, or every subsequent start re-derives the
			// version by inference instead of reading it.
			if err := bdb.Update(func(tx *bolt.Tx) error { return putSchemaVersion(tx, target) }); err != nil {
				logger.Debug("ensureSchema: stamping unversioned database failed", zap.Error(err))
				return fmt.Errorf("failed to record schema version: %w", err)
			}
		}
		return nil
	}

	logger.Info("Database schema upgrade required",
		zap.String("path", path),
		zap.Int("from_version", current),
		zap.Int("to_version", target))

	backupPath, err := backupBeforeMigration(bdb, path, current)
	if err != nil {
		logger.Debug("ensureSchema: pre-migration copy failed, refusing to migrate", zap.Error(err))
		// Refusing to start is the better failure. A migration that runs
		// without a fallback on a volume already too sick to hold a copy can
		// leave metadata that is neither the old shape nor the new one, and
		// nothing to restore from; a controller that will not start is a page.
		return fmt.Errorf("refusing to migrate the database without a pre-migration copy: %w", err)
	}
	logger.Info("Wrote pre-migration database copy",
		zap.String("path", backupPath),
		zap.Int("schema_version", current))

	for _, m := range migrations {
		if m.to <= current {
			logger.Debug("ensureSchema: migration already applied, skipping",
				zap.Int("version", m.to), zap.String("name", m.name))
			continue
		}
		logger.Debug("ensureSchema: applying migration", zap.Int("version", m.to), zap.String("name", m.name))
		started := time.Now()
		if err := bdb.Update(func(tx *bolt.Tx) error {
			if err := m.apply(tx); err != nil {
				return err
			}
			return putSchemaVersion(tx, m.to)
		}); err != nil {
			// The failed step rolled back with its version stamp, so the
			// database is still at whatever the last successful step left. That
			// is a real, consistent version, and the next start resumes from
			// it — there is no half-migrated state to clean up by hand.
			logger.Debug("ensureSchema: migration failed", zap.Int("version", m.to), zap.String("name", m.name),
				zap.Duration("took", time.Since(started)), zap.Error(err))
			return fmt.Errorf("database schema migration to version %d (%s) failed; "+
				"database is intact at version %d and a pre-migration copy is at %s: %w",
				m.to, m.name, current, backupPath, err)
		}
		current = m.to
		logger.Info("Applied database schema migration",
			zap.Int("version", m.to),
			zap.String("name", m.name),
			zap.Duration("took", time.Since(started)))
	}

	logger.Info("Database schema up to date", zap.String("path", path), zap.Int("version", current))
	return nil
}

// SchemaVersion returns the schema version recorded in this database.
func (db *DB) SchemaVersion() (int, error) {
	db.log().Debug("DB.SchemaVersion: reading schema version", zap.String("path", db.path))
	db.mu.RLock()
	defer db.mu.RUnlock()

	version, present, err := readSchemaVersion(db.db, db.path)
	if err != nil {
		db.log().Debug("DB.SchemaVersion: read failed", zap.Error(err))
		return 0, err
	}
	if !present {
		// Open always leaves a record behind, so its absence means something
		// removed it underneath us rather than an old database.
		db.log().Debug("DB.SchemaVersion: version record missing", zap.String("path", db.path))
		return 0, fmt.Errorf("database %s has no schema version record", db.path)
	}
	db.log().Debug("DB.SchemaVersion: done", zap.Int("version", version))
	return version, nil
}
