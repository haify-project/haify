// Package backup ships point-in-time copies of DRBD volumes to storage that is
// NOT part of the cluster — S3-compatible object stores, SMB shares, WebDAV.
//
// It is the third data-protection layer, and it exists because the other two
// are not backups:
//
//   - Snapshots (pkg/controller/schedule.go) live in the same pool as their
//     origin. They protect against "rm -rf", not against losing the machine.
//   - WAN DR (pkg/wanproxy) is a replica. A delete replicates; corruption
//     replicates. It protects against losing a site, not against losing data.
//
// A backup is a copy that nothing in the cluster can reach and nothing in the
// cluster can un-write.
//
// # Backend choice: rclone
//
// The user-visible requirement is S3 *and* SMB/WebDAV (a NAS). Two options were
// weighed:
//
// A pure-Go S3 client (aws-sdk-go-v2 / minio-go) is genuinely attractive: no
// external binary to install, no version skew, no "rclone: command not found"
// on the one node that was rebuilt, and credentials that never leave the
// controller's address space. It was rejected for two reasons, only the first
// of which is about protocols. It speaks S3 and nothing else, so SMB and WebDAV
// would each need a separate implementation. More importantly, the controller
// does not have the data: the snapshot is a block device on a storage node. A
// Go client linked into the controller means every byte travels node ->
// controller -> object store, which doubles WAN traffic and makes the
// controller a throughput bottleneck for a job measured in terabytes.
//
// rclone runs ON THE NODE, so the data path is node -> object store directly,
// and one dependency covers S3, SMB, WebDAV and dozens more. The cost is real
// and is not waved away: it is an external binary that must be present on every
// node that backs up. Preflight checks for it and fails with an actionable
// message rather than starting a job that cannot finish.
//
// The choice is confined behind Backend/Session below. Adding a pure-Go S3
// backend later — for a cluster that refuses to install rclone, or to back up
// from the controller in a single-node deployment — means implementing two
// interfaces, not editing the controller.
//
// # Incremental and compressed
//
// Images are gzip-compressed on the way out, and after the first backup of a
// thin volume only the blocks thin_delta reports as changed are shipped. Both
// live in pkg/controller (backup_transfer.go, backup_incremental.go), which
// composes the dd/gzip pipelines around PushCmd and PullCmd; this package only
// moves bytes to and from the target.
package backup

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"
)

// Kind is a target's storage protocol.
type Kind string

const (
	// KindS3 is any S3-compatible object store (AWS, MinIO, Ceph RGW, Aliyun OSS
	// in S3 mode, ...).
	KindS3 Kind = "s3"
	// KindSMB is a CIFS/SMB share, which is what most NAS boxes speak.
	KindSMB Kind = "smb"
	// KindWebDAV is a WebDAV endpoint (Nextcloud, and most NAS boxes again).
	KindWebDAV Kind = "webdav"
)

// ParseKind validates a target kind.
func ParseKind(s string) (Kind, error) {
	switch Kind(strings.ToLower(strings.TrimSpace(s))) {
	case KindS3:
		return KindS3, nil
	case KindSMB:
		return KindSMB, nil
	case KindWebDAV:
		return KindWebDAV, nil
	default:
		zap.L().Debug("ParseKind: unknown target kind", zap.String("kind", s))
		return "", fmt.Errorf("backup: unknown target kind %q (want s3, smb or webdav)", s)
	}
}

// TargetSpec fully describes one backup repository.
//
// Secret is the only sensitive field. It never appears in a command line: it
// reaches the node inside a 0600 file written over the SSH stream (see
// DeploymentClient.PutSecret), and it is never returned by the API.
type TargetSpec struct {
	// Name is the target's identifier in `haify backup target`.
	Name string
	Kind Kind

	// Prefix is a path prepended to every object this target stores, so one
	// bucket/share can hold several clusters. May be empty.
	Prefix string

	// S3: Bucket is required; Endpoint is required for anything that is not AWS;
	// Region defaults to us-east-1 because many S3 clones reject an empty one.
	Bucket   string
	Endpoint string
	Region   string

	// SMB: Host is the server (host or host:port) and Share the share name.
	// WebDAV: Endpoint is the collection URL.
	Host  string
	Share string

	// User is the access key id (S3) or the username (SMB/WebDAV).
	User string

	// Secret is the secret access key (S3) or the password (SMB/WebDAV).
	Secret string

	// SecretIsObscured says Secret is already in rclone's obscured form.
	// rclone refuses a plain-text password for SMB/WebDAV, so one is produced
	// for the operator; this flag is the escape hatch for an operator who would
	// rather run `rclone obscure` themselves and paste the result.
	SecretIsObscured bool

	// LockMode turns on S3 Object Lock for everything written to the target:
	// LockGovernance or LockCompliance, empty for none. See lock.go.
	LockMode LockMode
	// LockDays is how long every backup stays locked at the least.
	LockDays int
	// FullEveryDays is how long an incremental chain may grow before the next
	// backup is a full one; it bounds how much longer than LockDays the
	// chain's earliest backups stay locked. Defaults to DefaultFullEveryDays.
	FullEveryDays int
}

// Validate checks a target is usable before it is stored or dialled.
func (t TargetSpec) Validate() error {
	log := zap.L()
	log.Debug("TargetSpec.Validate: checking target",
		zap.String("target", t.Name), zap.String("kind", string(t.Kind)),
		zap.String("bucket", t.Bucket), zap.String("endpoint_host", endpointHost(t.Endpoint)),
		zap.String("host", t.Host), zap.String("share", t.Share),
		zap.Bool("has_user", t.User != ""), zap.Bool("has_secret", t.Secret != ""),
		zap.Bool("secret_obscured", t.SecretIsObscured), zap.String("lock_mode", string(t.LockMode)))
	if strings.TrimSpace(t.Name) == "" {
		log.Debug("TargetSpec.Validate: rejected, no name")
		return fmt.Errorf("backup: target name is required")
	}
	if strings.ContainsAny(t.Name, "/ \t\n") {
		log.Debug("TargetSpec.Validate: rejected, name has spaces or slashes", zap.String("target", t.Name))
		return fmt.Errorf("backup: target name %q must not contain spaces or slashes", t.Name)
	}
	switch t.Kind {
	case KindS3:
		if strings.TrimSpace(t.Bucket) == "" {
			log.Debug("TargetSpec.Validate: rejected, s3 without bucket", zap.String("target", t.Name))
			return fmt.Errorf("backup: s3 target %q needs a bucket", t.Name)
		}
		if strings.TrimSpace(t.User) == "" || t.Secret == "" {
			log.Debug("TargetSpec.Validate: rejected, s3 without credentials", zap.String("target", t.Name),
				zap.Bool("has_user", strings.TrimSpace(t.User) != ""), zap.Bool("has_secret", t.Secret != ""))
			return fmt.Errorf("backup: s3 target %q needs an access key and a secret key", t.Name)
		}
	case KindSMB:
		if strings.TrimSpace(t.Host) == "" || strings.TrimSpace(t.Share) == "" {
			log.Debug("TargetSpec.Validate: rejected, smb without host or share", zap.String("target", t.Name),
				zap.String("host", t.Host), zap.String("share", t.Share))
			return fmt.Errorf("backup: smb target %q needs a host and a share", t.Name)
		}
		// A port is optional, but a malformed one must be rejected here rather
		// than reaching rclone, which would report it as a connection failure
		// at backup time with nothing pointing back at the typo.
		if _, port := splitSMBHost(t.Host); port != "" {
			if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
				log.Debug("TargetSpec.Validate: rejected, invalid smb port", zap.String("target", t.Name),
					zap.String("host", t.Host), zap.String("port", port))
				return fmt.Errorf("backup: smb target %q has an invalid port in host %q", t.Name, t.Host)
			}
		}
	case KindWebDAV:
		if strings.TrimSpace(t.Endpoint) == "" {
			log.Debug("TargetSpec.Validate: rejected, webdav without endpoint", zap.String("target", t.Name))
			return fmt.Errorf("backup: webdav target %q needs an endpoint URL", t.Name)
		}
	default:
		log.Debug("TargetSpec.Validate: rejected, unknown kind", zap.String("target", t.Name),
			zap.String("kind", string(t.Kind)))
		return fmt.Errorf("backup: target %q has unknown kind %q", t.Name, t.Kind)
	}
	err := t.validateLock()
	log.Debug("TargetSpec.Validate: done", zap.String("target", t.Name), zap.Error(err))
	return err
}

// Describe renders a target for display. It never includes the secret.
func (t TargetSpec) Describe() string {
	switch t.Kind {
	case KindS3:
		ep := t.Endpoint
		if ep == "" {
			ep = "aws"
		}
		desc := fmt.Sprintf("s3 %s/%s (%s)", ep, t.Bucket, joinPrefix(t.Prefix))
		if t.Locked() {
			desc += fmt.Sprintf(", object lock %s %dd, full every %dd", t.LockMode, t.LockDays, t.FullEvery())
		}
		return desc
	case KindSMB:
		return fmt.Sprintf("smb //%s/%s (%s)", t.Host, t.Share, joinPrefix(t.Prefix))
	case KindWebDAV:
		return fmt.Sprintf("webdav %s (%s)", t.Endpoint, joinPrefix(t.Prefix))
	default:
		return string(t.Kind)
	}
}

func joinPrefix(p string) string {
	p = strings.Trim(strings.TrimSpace(p), "/")
	if p == "" {
		return "prefix: <root>"
	}
	return "prefix: " + p
}

// HostResult is one node's outcome.
type HostResult struct {
	Host    string
	Output  string
	Success bool
	Err     error
}

// Result aggregates per-host outcomes. It mirrors the AllSuccess/FailedHosts
// contract of pkg/deployment so this package checks results the same way
// pkg/controller does, without importing the deployment package's concrete
// types — which is what keeps DeploymentClient trivially fakeable.
type Result struct {
	Hosts map[string]*HostResult
}

// AllSuccess reports whether every host succeeded.
func (r *Result) AllSuccess() bool {
	if r == nil {
		return false
	}
	for _, h := range r.Hosts {
		if h == nil || !h.Success {
			return false
		}
	}
	return len(r.Hosts) > 0
}

// Output returns one host's combined output.
func (r *Result) Output(host string) string {
	if r == nil {
		return ""
	}
	if h := r.Hosts[host]; h != nil {
		return h.Output
	}
	return ""
}

// FailureDetails names the failed hosts with the reason each one gave.
func (r *Result) FailureDetails() string {
	if r == nil {
		return "no result"
	}
	var failed []string
	for host, h := range r.Hosts {
		if h == nil || !h.Success {
			failed = append(failed, host)
		}
	}
	if len(failed) == 0 {
		return ""
	}
	sort.Strings(failed)
	zap.L().Debug("Result.FailureDetails: hosts failed", zap.Strings("hosts", failed), zap.Int("total", len(r.Hosts)))
	parts := make([]string, 0, len(failed))
	for _, host := range failed {
		reason := ""
		if h := r.Hosts[host]; h != nil {
			reason = strings.TrimSpace(withoutNotices(h.Output))
			if reason == "" && h.Err != nil {
				reason = strings.TrimSpace(h.Err.Error())
			}
		}
		if reason == "" {
			reason = "no output"
		}
		parts = append(parts, fmt.Sprintf("%s: %s", host, strings.Join(strings.Fields(reason), " ")))
	}
	return strings.Join(parts, "; ")
}

// DeploymentClient is the minimal node-facing surface this package needs. It
// follows the isolation pattern of pkg/gateway and pkg/wanproxy: depend on a
// small interface, not on *deployment.Client.
type DeploymentClient interface {
	// Exec runs cmd on host.
	Exec(ctx context.Context, hosts []string, cmd string) (*Result, error)

	// PutSecret writes content to relPath — interpreted relative to the SSH
	// login user's home directory — with mode 0600, WITHOUT the content ever
	// appearing in a command line.
	//
	// This is not the same primitive as DistributeConfig, which base64-encodes
	// the content into the remote command string: perfectly fine for a DRBD
	// config, fatal for an object-store secret key, which would then be visible
	// in `ps` on the node and in every layer that logs commands.
	PutSecret(ctx context.Context, hosts []string, content, relPath string) (*Result, error)
}

// Session is a prepared, node-local handle on one target. It owns whatever
// credential material was staged on the node, so callers MUST Close it.
//
// The push/pull halves are shell commands rather than Go readers/writers on
// purpose: the bytes must go straight from the snapshot device to the network
// without being staged in a file (there is no room for a second copy of the
// volume) and without passing through the controller. The caller composes them
// into one pipeline with its own dd.
type Session interface {
	// PushCmd renders a command that reads an object's bytes from stdin and
	// stores them at objectPath. sizeBytes is the exact number of bytes the
	// caller will write; backends may need it to size a multipart upload.
	PushCmd(objectPath string, sizeBytes uint64) string

	// PullCmd renders a command that writes objectPath's bytes to stdout.
	PullCmd(objectPath string) string

	// SizeBytes returns the stored size of objectPath.
	SizeBytes(ctx context.Context, objectPath string) (uint64, error)

	// Remove deletes objectPath. Removing something that is already gone is not
	// an error, so cleanup after a failed backup is idempotent.
	Remove(ctx context.Context, objectPath string) error

	// PutText stores a small text object (the manifest). Only for content that
	// is not secret: it travels in the command line.
	PutText(ctx context.Context, objectPath, content string) error

	// List returns every object under the target's root, with its size, as
	// paths relative to that root. It is how a controller that has lost its
	// database finds out what a target holds.
	List(ctx context.Context) ([]Object, error)

	// GetText reads a small text object (a manifest) back.
	GetText(ctx context.Context, objectPath string) (string, error)

	// SetLock makes every object pushed from now on locked in mode until
	// until (S3 Object Lock). A zero until turns it off.
	SetLock(mode LockMode, until time.Time)

	// SetReadAt makes every read from now on — pulls, listings, text — see
	// the target as it was at t: the object versions current then, before any
	// later overwrite or delete. A zero t reads the current versions. Writes
	// are refused while it is set.
	SetReadAt(t time.Time)

	// LockOf reports the Object Lock mode and retain-until date the target
	// holds for objectPath; an empty mode means none.
	LockOf(ctx context.Context, objectPath string) (LockMode, time.Time, error)

	// Close removes the node-local credential material.
	Close(ctx context.Context) error
}

// Object is one entry of a target listing.
type Object struct {
	Path  string
	Bytes uint64
}

// Backend opens sessions against a target from a given node.
type Backend interface {
	// Name identifies the implementation, and is recorded on each backup so a
	// future restore knows what wrote it.
	Name() string

	// Preflight verifies the backend's prerequisites on host for target. It
	// runs before any snapshot is taken, so a missing dependency costs
	// nothing.
	Preflight(ctx context.Context, dep DeploymentClient, host string, target TargetSpec) error

	// Prepare stages credentials for target on host.
	Prepare(ctx context.Context, dep DeploymentClient, host string, target TargetSpec) (Session, error)
}

// ObjectPath joins path elements into an object key, dropping empty ones and
// collapsing separators, so a target with no prefix does not produce "//".
func ObjectPath(parts ...string) string {
	clean := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.Trim(strings.TrimSpace(p), "/")
		if p != "" {
			clean = append(clean, p)
		}
	}
	return strings.Join(clean, "/")
}

// withoutNotices drops rclone's NOTICE lines, which come first and push the
// actual error out of sight ("Streaming uploads using chunk size 5Mi ..."
// ahead of "connection refused"). Output that is nothing but notices is kept.
func withoutNotices(out string) string {
	var kept []string
	for _, line := range strings.Split(out, "\n") {
		if !strings.Contains(line, "NOTICE:") {
			kept = append(kept, line)
		}
	}
	if strings.TrimSpace(strings.Join(kept, "")) == "" {
		return out
	}
	return strings.Join(kept, "\n")
}
