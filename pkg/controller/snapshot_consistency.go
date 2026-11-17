package controller

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"go.uber.org/zap"
)

// What kind of consistency a snapshot of a guest's volume has, and where
// that is kept.
//
// The answer is known only after the thaw (guest_quiesce.go), so it is
// written onto the snapshots once they exist: an LVM tag, or a ZFS user
// property. It stays with the snapshot for as long as the snapshot exists and
// can be read on the node:
//
//	lvs -o lv_name,lv_tags <pool>
//	zfs get -H -o value haify:consistency <dataset>@<snapshot>
//
// It is also in the reply of the call that took the snapshot, and in every
// list of snapshots and of backups. Snapshots of resources that belong to no
// guest are not marked, and the lists show nothing for them: they are crash
// consistent, as every snapshot of a volume in use is unless something froze
// its writer, and "crash" next to each would read as something having gone
// wrong.

// snapshotConsistency is what a snapshot is known to hold.
type snapshotConsistency string

const (
	// consistencyCrash: what a power cut at that instant would have left.
	consistencyCrash snapshotConsistency = "crash"
	// consistencyFilesystem: the guest's filesystems were flushed and frozen
	// for the whole of the snapshot.
	consistencyFilesystem snapshotConsistency = "filesystem"
)

// The names the mark has on a snapshot. LVM tags have no values, so the
// value is part of the tag.
const (
	consistencyLVMTagPrefix = "haify.consistency="
	consistencyZFSProperty  = "haify:consistency"
)

// takenSnapshot is one snapshot taken under a guestQuiesce.
type takenSnapshot struct {
	resource string
	host     string
	// name is "<vg>/<snapshot lv>", or for ZFS "<dataset>@<snapshot>".
	name string
	zfs  bool
}

// took notes a snapshot taken while q's guests were frozen, to be marked by
// finish. Snapshots of resources without a guest are not kept.
func (q *guestQuiesce) took(resource, host, name string, zfs bool) {
	if q == nil || q.byResource[resource] == nil {
		return
	}
	q.taken = append(q.taken, takenSnapshot{resource: resource, host: host, name: name, zfs: zfs})
}

// discard forgets the snapshots noted so far: they were removed again.
func (q *guestQuiesce) discard() {
	if q != nil {
		q.taken = nil
	}
}

// consistency is what a snapshot of resource taken under q holds. It is
// final only after finish.
func (q *guestQuiesce) consistency(resource string) snapshotConsistency {
	if q == nil {
		return consistencyCrash
	}
	if g := q.byResource[resource]; g != nil && g.frozen {
		return consistencyFilesystem
	}
	return consistencyCrash
}

// recorded is the mark the snapshots of resource taken under q carry: its
// consistency for a guest's resource, "" for any other, which gets none. It
// is final only after finish.
func (q *guestQuiesce) recorded(resource string) string {
	if q == nil || q.byResource[resource] == nil {
		return ""
	}
	return string(q.consistency(resource))
}

// consistencyOfTags reads the mark from an LV's tags as lvs prints them,
// comma separated. It is "" for a snapshot that carries none.
func consistencyOfTags(tags string) string {
	for _, tag := range strings.Split(tags, ",") {
		if c, ok := strings.CutPrefix(strings.TrimSpace(tag), consistencyLVMTagPrefix); ok {
			return knownConsistency(c)
		}
	}
	return ""
}

// knownConsistency passes a mark through when it is one this controller
// writes, so that whatever else ended up in a tag or a property is not shown
// as a statement about the snapshot. zfs prints "-" for a property not set.
func knownConsistency(v string) string {
	switch c := snapshotConsistency(strings.TrimSpace(v)); c {
	case consistencyCrash, consistencyFilesystem:
		return string(c)
	}
	return ""
}

// note says, for the reply, which consistency a snapshot of resource has and
// why. It is "" for a resource that belongs to no guest. Call it after
// finish.
func (q *guestQuiesce) note(resource string) string {
	if q == nil {
		return ""
	}
	g := q.byResource[resource]
	if g == nil {
		return ""
	}
	if g.frozen {
		return fmt.Sprintf("filesystem-consistent: guest %s was frozen on %s while it was taken", g.domain, q.rm.nodeLabel(g.host))
	}
	return "crash-consistent: " + g.why
}

// notes is note for each of resources that has one, without repeats: two
// resources of one guest say the same thing.
func (q *guestQuiesce) notes(resources ...string) string {
	var out []string
	seen := map[string]bool{}
	for _, r := range resources {
		if n := q.note(r); n != "" && !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	return strings.Join(out, "; ")
}

// withNote appends a consistency note to a reply.
func withNote(msg, note string) string {
	if note == "" {
		return msg
	}
	return msg + " (" + note + ")"
}

// recordConsistency marks every snapshot noted with took. A mark that cannot
// be written is logged and nothing more: the snapshot is as good without it,
// and the reply has already said what it is.
func (q *guestQuiesce) recordConsistency() {
	if len(q.taken) == 0 {
		return
	}
	rm := q.rm
	log := rm.resourcesLog()
	byHost := map[string][]string{}
	for _, t := range q.taken {
		c := q.consistency(t.resource)
		cmd := fmt.Sprintf("lvchange --addtag %s%s %s", consistencyLVMTagPrefix, c, t.name)
		if t.zfs {
			cmd = fmt.Sprintf("zfs set %s=%s %s", consistencyZFSProperty, c, t.name)
		}
		log.Debug("recordConsistency: marking snapshot", zap.String("resource", t.resource), zap.String("host", t.host),
			zap.String("snapshot", t.name), zap.Bool("zfs", t.zfs), zap.String("consistency", string(c)))
		byHost[t.host] = append(byHost[t.host], cmd+" >/dev/null || rc=1")
	}
	hosts := make([]string, 0, len(byHost))
	for h := range byHost {
		hosts = append(hosts, h)
	}
	sort.Strings(hosts)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	for _, h := range hosts {
		start := time.Now()
		// Every mark is attempted, and the script fails if any did. It is
		// base64-wrapped: dispatch's sh -c quoting would empty $rc.
		script := "rc=0\n" + strings.Join(byHost[h], "\n") + "\nexit $rc\n"
		err := execFailure(rm.deployment.Exec(ctx, []string{h}, "echo "+base64Std(script)+" | base64 -d | sudo /bin/bash"))
		log.Debug("recordConsistency: marked snapshots on host", zap.String("host", h), zap.Int("snapshots", len(byHost[h])),
			zap.Duration("took", time.Since(start)), zap.Error(err))
		if err != nil {
			rm.controller.logger.Warn("Could not record a snapshot's consistency on it",
				zap.String("node", rm.nodeLabel(h)), zap.Error(err))
		}
	}
}

// resourceOfVolume names the resource whose volume is backed by
// <pool>/<backing>, or "" when there is none on record. One-node snapshots
// address a volume by its backing store; the guest is the resource's.
func (c *Controller) resourceOfVolume(ctx context.Context, pool, backing string) string {
	log := c.traceLogger()
	if c.db == nil || backing == "" {
		log.Debug("resourceOfVolume: no database or volume", zap.String("pool", pool), zap.String("volume", backing))
		return ""
	}
	resources, err := c.db.ListResources(ctx)
	if err != nil {
		log.Debug("resourceOfVolume: resource list failed", zap.Error(err))
		return ""
	}
	for _, r := range resources {
		// Only a guest's resource is of interest, and few resources are one:
		// the label is checked first to leave the others' volumes unread.
		if r.Labels[libvirtDomainLabel] == "" {
			continue
		}
		vols, err := c.db.ListVolumes(ctx, r.Name)
		if err != nil {
			log.Debug("resourceOfVolume: volume list failed", zap.String("resource", r.Name), zap.Error(err))
			continue
		}
		for _, v := range vols {
			if v.VolumeName == backing && normalizeManagedName(v.Pool) == normalizeManagedName(pool) {
				log.Debug("resourceOfVolume: found", zap.String("pool", pool), zap.String("volume", backing), zap.String("resource", r.Name))
				return r.Name
			}
		}
	}
	log.Debug("resourceOfVolume: no guest's resource has the volume", zap.String("pool", pool), zap.String("volume", backing))
	return ""
}

// snapshotVolumeQuiesced takes a one-node snapshot of <pool>/<backing> with
// take, with the guest of the volume's resource frozen around it, and returns
// the consistency note for the reply. host is where the snapshot is taken and
// snapshot its name there ("<vg>/<lv>" or "<dataset>@<name>").
func (c *Controller) snapshotVolumeQuiesced(ctx context.Context, pool, backing, host, snapshot string, zfs bool, take func() error) (string, error) {
	log := c.traceLogger()
	resource := c.resourceOfVolume(ctx, pool, backing)
	if resource == "" {
		log.Debug("snapshotVolumeQuiesced: no guest; taking the snapshot as it is", zap.String("pool", pool), zap.String("volume", backing))
		return "", take()
	}
	q := c.resources.quiesceGuests(ctx, resource)
	defer q.finish()
	start := time.Now()
	err := take()
	log.Debug("snapshotVolumeQuiesced: snapshot taken", zap.String("resource", resource), zap.String("snapshot", snapshot),
		zap.String("host", host), zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return "", err
	}
	q.took(resource, host, snapshot, zfs)
	q.finish()
	return q.note(resource), nil
}
