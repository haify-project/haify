package controller

import (
	"context"
	"fmt"
	"hash/crc32"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/haify-project/haify/pkg/gateway"
)

// Directory quotas for NFS exports.
//
// An NFS gateway exports one filesystem; several exports are directories in
// it, and nothing stopped one of them filling the whole volume. ext4 project
// quotas cap a directory tree: the tree is tagged with a project id (inherited
// by everything created in it) and the id gets a block limit.
//
// It needs the filesystem made with the project feature and mounted with
// prjquota. Gateways created from this version on have both. An older one
// does not, and the feature can only be added to an unmounted filesystem
// (`tune2fs -O quota,project <device>`, then recreate the gateway), so the
// error says that. The id is derived from the export path, so it is the same
// whichever node serves the gateway after a failover; it lives in the
// filesystem, which moves with it.

// projectID is an export directory's ext4 project id: stable, nonzero, and
// clear of the small ids people assign by hand.
func projectID(dir string) uint32 {
	return crc32.ChecksumIEEE([]byte(dir))%4000000000 + 100000
}

// SetNFSExportQuota limits an NFS export directory of resource to limitBytes;
// zero removes the limit.
func (rm *ResourceManager) SetNFSExportQuota(ctx context.Context, resource, exportPath string, limitBytes uint64) error {
	log := rm.controller.traceLogger()
	log.Debug("SetNFSExportQuota: setting quota", zap.String("resource", resource),
		zap.String("export_path", exportPath), zap.Uint64("limit_bytes", limitBytes))
	dir, err := gateway.ResolveNFSExportPath(resource, exportPath)
	if err != nil {
		log.Debug("SetNFSExportQuota: export path invalid", zap.String("export_path", exportPath), zap.Error(err))
		return err
	}
	hosts, err := rm.failoverHosts(ctx, resource)
	if err != nil {
		log.Debug("SetNFSExportQuota: failover hosts unknown", zap.String("resource", resource), zap.Error(err))
		return err
	}
	id := projectID(dir)
	log.Debug("SetNFSExportQuota: resolved", zap.String("dir", dir), zap.Uint32("project_id", id), zap.Strings("hosts", hosts))
	// Run where the export's filesystem is mounted from DRBD; the others skip.
	script := fmt.Sprintf(`set -e
dir=%[1]q
[ -d "$dir" ] || { echo SKIP; exit 0; }
dev=$(findmnt -n -o SOURCE -T "$dir" 2>/dev/null || true)
case "$dev" in /dev/drbd*) ;; *) echo SKIP; exit 0 ;; esac
mp=$(findmnt -n -o TARGET -T "$dir")
findmnt -n -o OPTIONS -T "$dir" | grep -qw prjquota || { echo NOPRJQUOTA; exit 0; }
command -v setquota >/dev/null 2>&1 || { echo NOSETQUOTA; exit 0; }
chattr -R -p %[2]d +P "$dir"
setquota -P %[2]d 0 %[3]d 0 0 "$mp"
echo DONE
`, dir, id, (limitBytes+1023)/1024)
	start := time.Now()
	res, err := rm.deployment.Exec(ctx, hosts, "echo "+base64Std(script)+" | base64 -d | sudo /bin/bash")
	if ce := log.Check(zap.DebugLevel, "SetNFSExportQuota: ran chattr/setquota script"); ce != nil {
		var failed []string
		if res != nil {
			failed = res.FailedHosts()
		}
		ce.Write(zap.Strings("hosts", hosts), zap.Strings("failed_hosts", failed),
			zap.Duration("took", time.Since(start)), zap.Error(err))
	}
	if err != nil {
		return err
	}
	var seen []string
	for _, h := range hosts {
		hr := res.Hosts[h]
		if hr == nil {
			log.Debug("SetNFSExportQuota: no result from host", zap.String("host", h))
			continue
		}
		out := strings.TrimSpace(hr.Output)
		log.Debug("SetNFSExportQuota: host answered", zap.String("host", h), zap.Bool("success", hr.Success),
			zap.String("output", out))
		switch {
		case !hr.Success:
			return fmt.Errorf("setting the quota on %s failed: %s", rm.nodeLabel(h), out)
		case strings.HasSuffix(out, "DONE"):
			log.Debug("SetNFSExportQuota: quota set", zap.String("host", h), zap.String("dir", dir))
			return nil
		case strings.HasSuffix(out, "NOPRJQUOTA"):
			return fmt.Errorf("the filesystem of %s is not mounted with project quotas: it predates them. Add the "+
				"feature while it is unmounted (`tune2fs -O quota,project <device>`) and recreate the gateway", dir)
		case strings.HasSuffix(out, "NOSETQUOTA"):
			return fmt.Errorf("%s has no setquota; install the quota package on the gateway nodes", rm.nodeLabel(h))
		}
		seen = append(seen, rm.nodeLabel(h))
	}
	log.Debug("SetNFSExportQuota: export not mounted on any host", zap.String("dir", dir), zap.Strings("hosts", seen))
	return fmt.Errorf("%s is not mounted on any of %s; is the NFS gateway of %s running?", dir, strings.Join(seen, ", "), resource)
}
