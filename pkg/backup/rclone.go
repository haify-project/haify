package backup

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"go.uber.org/zap"
)

// remoteName is the rclone remote defined in every config this package renders.
// It is fixed because the config file is written per operation and holds
// exactly one remote.
const remoteName = "haifybackup"

// nodeSecretDir is where a rendered rclone config lives on the node, relative
// to the SSH login user's home.
//
// Home rather than /etc or /run: the config is written over the SSH stream by
// the login user, who is not necessarily root, and a 0600 file in that user's
// home is both writable without a privilege escalation and unreadable by
// anyone else. The directory is created 0700 first, so the file is never
// exposed even for the instant between create and chmod.
const nodeSecretDir = ".haify-backup"

// s3MaxParts is the number of multipart chunks S3 allows for one object.
// `rclone rcat` streams an object of unknown length, so it cannot grow the
// chunk size on its own the way a file upload does; left at the 5 MiB default
// that caps a single object at ~48 GiB, which a storage volume passes easily.
// Every push therefore sizes the chunk from the exact byte count it is about
// to write. Getting this wrong does not corrupt anything — it fails the upload
// partway through, which is worse: it wastes the whole transfer.
const s3MaxParts = 9000

// s3MinChunkMiB is S3's minimum multipart chunk size.
const s3MinChunkMiB = 5

// RcloneBackend runs rclone on the storage node. See the package doc for why
// this, and not a pure-Go S3 client, is the default.
type RcloneBackend struct{}

// NewRclone returns the rclone-backed Backend.
func NewRclone() *RcloneBackend { return &RcloneBackend{} }

// Name implements Backend.
func (b *RcloneBackend) Name() string { return "rclone" }

// Preflight implements Backend. It runs before anything is snapshotted, so a
// node without rclone costs an SSH round trip rather than a snapshot, an
// upload attempt and a confusing partial failure.
//
// A target that locks objects needs an rclone that can lock them: an older one
// accepts the upload and stores it unlocked, which is the one outcome that
// must never look like success.
func (b *RcloneBackend) Preflight(ctx context.Context, dep DeploymentClient, host string, target TargetSpec) error {
	log := zap.L()
	log.Debug("RcloneBackend.Preflight: checking rclone on node", zap.String("host", host),
		zap.String("target", target.Name), zap.String("kind", string(target.Kind)), zap.Bool("locked", target.Locked()))
	if dep == nil {
		log.Debug("RcloneBackend.Preflight: no deployment client", zap.String("host", host))
		return fmt.Errorf("backup: deployment client is nil")
	}
	start := time.Now()
	res, err := dep.Exec(ctx, []string{host},
		`command -v rclone >/dev/null 2>&1 || { echo "rclone is not installed"; exit 1; }; rclone version | head -n 1`)
	log.Debug("RcloneBackend.Preflight: rclone version probe returned", zap.String("host", host),
		zap.Bool("success", err == nil && res.AllSuccess()), zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return fmt.Errorf("backup: rclone preflight on %s: %w", host, err)
	}
	if !res.AllSuccess() {
		log.Debug("RcloneBackend.Preflight: rclone missing", zap.String("host", host),
			zap.String("details", res.FailureDetails()))
		return fmt.Errorf(
			"backup: rclone is required on %s and was not found (install it, e.g. `curl https://rclone.org/install.sh | sudo bash`): %s",
			host, res.FailureDetails())
	}
	if target.Locked() {
		line := strings.TrimSpace(withoutNotices(res.Output(host)))
		log.Debug("RcloneBackend.Preflight: locked target, checking rclone version", zap.String("host", host),
			zap.String("version", line), zap.String("min", MinRcloneForLock))
		if !rcloneVersionAtLeast(line, MinRcloneForLock) {
			log.Debug("RcloneBackend.Preflight: rclone too old to lock", zap.String("host", host),
				zap.String("version", line))
			return fmt.Errorf("backup: target %q locks its objects, which needs rclone %s or later on %s (found %q)",
				target.Name, MinRcloneForLock, host, line)
		}
	}
	log.Debug("RcloneBackend.Preflight: node ready", zap.String("host", host), zap.String("target", target.Name))
	return nil
}

// Prepare implements Backend: it renders the target's rclone config and writes
// it to the node at mode 0600 without the secret ever entering a command line.
func (b *RcloneBackend) Prepare(ctx context.Context, dep DeploymentClient, host string, target TargetSpec) (Session, error) {
	log := zap.L()
	log.Debug("RcloneBackend.Prepare: staging credentials", zap.String("host", host),
		zap.String("target", target.Name), zap.String("kind", string(target.Kind)))
	if dep == nil {
		log.Debug("RcloneBackend.Prepare: no deployment client", zap.String("host", host))
		return nil, fmt.Errorf("backup: deployment client is nil")
	}
	if err := target.Validate(); err != nil {
		log.Debug("RcloneBackend.Prepare: target invalid", zap.String("target", target.Name), zap.Error(err))
		return nil, err
	}
	conf, err := renderRcloneConfig(target)
	if err != nil {
		log.Debug("RcloneBackend.Prepare: rendering config failed", zap.String("target", target.Name), zap.Error(err))
		return nil, err
	}

	id, err := randomID()
	if err != nil {
		log.Debug("RcloneBackend.Prepare: random id failed", zap.Error(err))
		return nil, err
	}
	rel := nodeSecretDir + "/" + id + ".conf"

	// The directory must be 0700 before the file lands in it: PutSecret sets the
	// file's own mode, but a private parent is what makes the window between the
	// two irrelevant.
	start := time.Now()
	_, err = dep.Exec(ctx, []string{host},
		fmt.Sprintf(`mkdir -p "$HOME/%s" && chmod 0700 "$HOME/%s"`, nodeSecretDir, nodeSecretDir))
	log.Debug("RcloneBackend.Prepare: credential directory created", zap.String("host", host),
		zap.String("dir", nodeSecretDir), zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return nil, fmt.Errorf("backup: prepare credential directory on %s: %w", host, err)
	}
	start = time.Now()
	res, err := dep.PutSecret(ctx, []string{host}, conf, rel)
	log.Debug("RcloneBackend.Prepare: config written", zap.String("host", host), zap.String("path", rel),
		zap.Int("config_bytes", len(conf)), zap.Bool("success", err == nil && res.AllSuccess()),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return nil, fmt.Errorf("backup: stage credentials on %s: %w", host, err)
	}
	if !res.AllSuccess() {
		return nil, fmt.Errorf("backup: stage credentials on %s failed: %s", host, res.FailureDetails())
	}

	return &rcloneSession{
		dep:     dep,
		host:    host,
		target:  target,
		cfgPath: `"$HOME/` + nodeSecretDir + `/` + id + `.conf"`,
		relPath: rel,
	}, nil
}

// rcloneSession is one node's prepared handle on a target.
type rcloneSession struct {
	dep    DeploymentClient
	host   string
	target TargetSpec

	// cfgPath is the shell expression naming the config, already quoted. It
	// contains no secret — only a path — so it is safe in a command line.
	cfgPath string
	relPath string

	// lockMode and lockUntil lock every object pushed (SetLock); readAt
	// pins every read to the versions current at that time (SetReadAt).
	lockMode  LockMode
	lockUntil time.Time
	readAt    time.Time
}

// remotePath maps an object key to the rclone remote path. The bucket (S3) or
// share (SMB) is part of the path rather than the config so one target can be
// re-pointed without re-staging credentials.
func (s *rcloneSession) remotePath(objectPath string) string {
	key := ObjectPath(s.target.Prefix, objectPath)
	switch s.target.Kind {
	case KindS3:
		return remoteName + ":" + ObjectPath(s.target.Bucket, key)
	case KindSMB:
		return remoteName + ":" + ObjectPath(s.target.Share, key)
	default: // WebDAV: the collection URL is the root.
		return remoteName + ":" + key
	}
}

// rcloneEnv prefixes a command with the config location. RCLONE_CONFIG carries
// a path, never a credential.
func (s *rcloneSession) rcloneEnv() string {
	return "RCLONE_CONFIG=" + s.cfgPath + " rclone --retries 3 --low-level-retries 10 --stats 0"
}

// PushCmd implements Session.
func (s *rcloneSession) PushCmd(objectPath string, sizeBytes uint64) string {
	cmd := s.rcloneEnv()
	if s.target.Kind == KindS3 {
		chunk := s3ChunkMiB(sizeBytes)
		zap.L().Debug("rcloneSession.PushCmd: s3 chunk size chosen", zap.String("object", objectPath),
			zap.Uint64("size_bytes", sizeBytes), zap.Uint64("chunk_mib", chunk))
		cmd += fmt.Sprintf(" --s3-chunk-size %dM", chunk)
	}
	zap.L().Debug("rcloneSession.PushCmd: rendering push", zap.String("host", s.host),
		zap.String("target", s.target.Name), zap.String("object", objectPath), zap.Uint64("size_bytes", sizeBytes))
	return cmd + s.lockFlags() + " rcat " + shellQuote(s.remotePath(objectPath))
}

// PullCmd implements Session.
func (s *rcloneSession) PullCmd(objectPath string) string {
	zap.L().Debug("rcloneSession.PullCmd: rendering pull", zap.String("host", s.host),
		zap.String("target", s.target.Name), zap.String("object", objectPath))
	return s.rcloneEnv() + s.readAtFlag() + " cat " + shellQuote(s.remotePath(objectPath))
}

// PutText implements Session. Content travels in the command line, so this is
// only for the manifest, which is metadata and holds no credentials. It is
// base64-encoded so arbitrary JSON survives the shell intact.
func (s *rcloneSession) PutText(ctx context.Context, objectPath, content string) error {
	log := zap.L()
	log.Debug("rcloneSession.PutText: writing text object", zap.String("host", s.host),
		zap.String("target", s.target.Name), zap.String("object", objectPath), zap.Int("bytes", len(content)))
	enc := base64.StdEncoding.EncodeToString([]byte(content))
	cmd := fmt.Sprintf("set -o pipefail; printf %%s %s | base64 -d | %s",
		shellQuote(enc), s.PushCmd(objectPath, uint64(len(content))))
	start := time.Now()
	res, err := s.dep.Exec(ctx, []string{s.host}, bash(cmd))
	log.Debug("rcloneSession.PutText: write returned", zap.String("host", s.host), zap.String("object", objectPath),
		zap.Bool("success", err == nil && res.AllSuccess()), zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return fmt.Errorf("backup: write %s: %w", objectPath, err)
	}
	if !res.AllSuccess() {
		return fmt.Errorf("backup: write %s failed: %s", objectPath, res.FailureDetails())
	}
	return nil
}

// SizeBytes implements Session by asking the remote how big the object it
// stored actually is. This is the end-to-end check that turns "the upload
// command exited 0" into "the far end holds every byte we sent".
func (s *rcloneSession) SizeBytes(ctx context.Context, objectPath string) (uint64, error) {
	log := zap.L()
	log.Debug("rcloneSession.SizeBytes: stat object", zap.String("host", s.host),
		zap.String("target", s.target.Name), zap.String("object", objectPath))
	cmd := s.rcloneEnv() + " size --json " + shellQuote(s.remotePath(objectPath))
	start := time.Now()
	res, err := s.dep.Exec(ctx, []string{s.host}, cmd)
	log.Debug("rcloneSession.SizeBytes: rclone size returned", zap.String("host", s.host), zap.String("object", objectPath),
		zap.Bool("success", err == nil && res.AllSuccess()), zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return 0, fmt.Errorf("backup: stat %s: %w", objectPath, err)
	}
	if !res.AllSuccess() {
		return 0, fmt.Errorf("backup: stat %s failed: %s", objectPath, res.FailureDetails())
	}
	n, err := parseRcloneSize(res.Output(s.host))
	log.Debug("rcloneSession.SizeBytes: object size", zap.String("object", objectPath),
		zap.Uint64("size_bytes", n), zap.Error(err))
	return n, err
}

// Remove implements Session. Deleting something already gone is a no-op, so
// cleanup after a failed backup can be retried freely.
func (s *rcloneSession) Remove(ctx context.Context, objectPath string) error {
	remote := shellQuote(s.remotePath(objectPath))
	// `lsf` of a missing object exits 0 with no output on some S3 servers
	// (RustFS), so its exit status says nothing; whether it printed a name is
	// what tells an object that is there from one that is already gone.
	cmd := fmt.Sprintf("if [ -n \"$(%s lsf %s 2>/dev/null)\" ]; then %s deletefile %s; fi",
		s.rcloneEnv(), remote, s.rcloneEnv(), remote)
	start := time.Now()
	res, err := s.dep.Exec(ctx, []string{s.host}, bash(cmd))
	zap.L().Debug("rcloneSession.Remove: delete returned", zap.String("host", s.host),
		zap.String("target", s.target.Name), zap.String("object", objectPath),
		zap.Bool("success", err == nil && res.AllSuccess()), zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return fmt.Errorf("backup: remove %s: %w", objectPath, err)
	}
	if !res.AllSuccess() {
		return fmt.Errorf("backup: remove %s failed: %s", objectPath, res.FailureDetails())
	}
	return nil
}

// List implements Session. One recursive listing of the target root rather
// than a walk, so a bucket of a thousand backups costs one round trip.
func (s *rcloneSession) List(ctx context.Context) ([]Object, error) {
	log := zap.L()
	log.Debug("rcloneSession.List: listing target", zap.String("host", s.host),
		zap.String("target", s.target.Name), zap.Time("read_at", s.readAt))
	cmd := s.rcloneEnv() + s.readAtFlag() + " lsjson -R --files-only " + shellQuote(s.remotePath(""))
	start := time.Now()
	res, err := s.dep.Exec(ctx, []string{s.host}, cmd)
	log.Debug("rcloneSession.List: rclone lsjson returned", zap.String("host", s.host),
		zap.Bool("success", err == nil && res.AllSuccess()), zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return nil, fmt.Errorf("backup: list target: %w", err)
	}
	if !res.AllSuccess() {
		return nil, fmt.Errorf("backup: list target failed: %s", res.FailureDetails())
	}
	objects, err := parseRcloneList(res.Output(s.host))
	log.Debug("rcloneSession.List: objects listed", zap.String("target", s.target.Name),
		zap.Int("count", len(objects)), zap.Error(err))
	return objects, err
}

// GetText implements Session.
func (s *rcloneSession) GetText(ctx context.Context, objectPath string) (string, error) {
	start := time.Now()
	res, err := s.dep.Exec(ctx, []string{s.host}, s.PullCmd(objectPath))
	zap.L().Debug("rcloneSession.GetText: read returned", zap.String("host", s.host),
		zap.String("target", s.target.Name), zap.String("object", objectPath),
		zap.Bool("success", err == nil && res.AllSuccess()), zap.Int("bytes", len(res.Output(s.host))),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return "", fmt.Errorf("backup: read %s: %w", objectPath, err)
	}
	if !res.AllSuccess() {
		return "", fmt.Errorf("backup: read %s failed: %s", objectPath, res.FailureDetails())
	}
	return res.Output(s.host), nil
}

// parseRcloneList reads `rclone lsjson` output. rclone may print notices on
// the lines before the JSON array, so the array is found rather than assumed
// to be the whole output.
func parseRcloneList(out string) ([]Object, error) {
	log := zap.L()
	start := strings.Index(out, "[")
	if start < 0 {
		log.Debug("parseRcloneList: no json array in output", zap.Int("output_bytes", len(out)))
		return nil, fmt.Errorf("backup: could not read the target listing: %q", strings.TrimSpace(out))
	}
	var entries []struct {
		Path  string `json:"Path"`
		Size  int64  `json:"Size"`
		IsDir bool   `json:"IsDir"`
	}
	if err := json.Unmarshal([]byte(out[start:]), &entries); err != nil {
		log.Debug("parseRcloneList: json unreadable", zap.Error(err))
		return nil, fmt.Errorf("backup: could not read the target listing: %w", err)
	}
	objects := make([]Object, 0, len(entries))
	skipped := 0
	for _, e := range entries {
		if e.IsDir || e.Size < 0 {
			skipped++
			continue
		}
		objects = append(objects, Object{Path: e.Path, Bytes: uint64(e.Size)})
	}
	log.Debug("parseRcloneList: parsed", zap.Int("entries", len(entries)), zap.Int("objects", len(objects)),
		zap.Int("skipped", skipped))
	return objects, nil
}

// Close implements Session by removing the staged credentials. A leftover file
// would be 0600 in a 0700 directory and therefore not a disclosure, but it
// would be a credential outliving its use, which is its own problem.
func (s *rcloneSession) Close(ctx context.Context) error {
	start := time.Now()
	res, err := s.dep.Exec(ctx, []string{s.host}, fmt.Sprintf(`rm -f "$HOME/%s"`, s.relPath))
	zap.L().Debug("rcloneSession.Close: staged credentials removed", zap.String("host", s.host),
		zap.String("path", s.relPath), zap.Bool("success", err == nil && res.AllSuccess()),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return fmt.Errorf("backup: remove staged credentials on %s: %w", s.host, err)
	}
	if !res.AllSuccess() {
		return fmt.Errorf("backup: remove staged credentials on %s failed: %s", s.host, res.FailureDetails())
	}
	return nil
}

// s3ChunkMiB sizes a multipart chunk so an object of sizeBytes fits inside
// S3's part limit. See s3MaxParts.
func s3ChunkMiB(sizeBytes uint64) uint64 {
	const miB = 1024 * 1024
	need := (sizeBytes/s3MaxParts + miB - 1) / miB
	if need < s3MinChunkMiB {
		return s3MinChunkMiB
	}
	return need
}

// parseRcloneSize reads `rclone size --json` output, which is
// {"count":1,"bytes":N}. Anything else is reported as an error rather than as
// a zero-byte object, because "the size is unknown" and "the object is empty"
// must never be confused on a verification path.
func parseRcloneSize(out string) (uint64, error) {
	out = strings.TrimSpace(out)
	// rclone may print notices before the JSON; take the last line that parses.
	lines := strings.Split(out, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		var v struct {
			Count int64  `json:"count"`
			Bytes uint64 `json:"bytes"`
		}
		if err := json.Unmarshal([]byte(strings.TrimSpace(lines[i])), &v); err != nil {
			continue
		}
		if v.Count == 0 {
			zap.L().Debug("parseRcloneSize: object not found (count 0)", zap.Int("line", i))
			return 0, fmt.Errorf("backup: object not found at the target")
		}
		zap.L().Debug("parseRcloneSize: parsed", zap.Int64("count", v.Count), zap.Uint64("size_bytes", v.Bytes))
		return v.Bytes, nil
	}
	zap.L().Debug("parseRcloneSize: no parsable json line", zap.Int("lines", len(lines)))
	return 0, fmt.Errorf("backup: could not read object size from rclone output: %q", out)
}

// bash wraps a command that relies on pipefail or `if`, which are bash
// features; the deployment layer's default shell is not guaranteed to be bash.
func bash(cmd string) string {
	return "bash -c " + shellQuote(cmd)
}

// shellQuote renders s as a single-quoted shell word.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// randomID returns a short random identifier for a per-operation file name.
func randomID() (string, error) {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		zap.L().Debug("randomID: reading random bytes failed", zap.Error(err))
		return "", fmt.Errorf("backup: generate id: %w", err)
	}
	return hex.EncodeToString(buf), nil
}
