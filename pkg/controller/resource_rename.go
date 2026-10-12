package controller

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/haify-project/haify/pkg/database"
)

// Renaming a resource.
//
// A resource's name is in its DRBD config file name and first line, in its
// backing volumes' names ("<name>_data", "<name>_vol<K>") and in its
// database records. The Proxmox plugin needs to change it: PVE reassigns a
// disk to another VM by renaming the volume, and turns a VM into a template
// by renaming its disks to base-*.
//
// It is done offline: the resource goes down on every participant, the
// backing volumes and the config are renamed, and it comes up under the new
// name, with its data, node ids and port untouched. So it is refused while
// the resource is Primary anywhere, and refused for everything that refers
// to the name from elsewhere — an HA config, a gateway, backups and their
// schedules, a snapshot schedule, existing snapshots (named after the old
// backing volume), WAN replication and encryption (the LUKS container is
// named after the backing volume) — rather than chasing every reference.

var resourceNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,62}$`)

// RenameResource renames resource to newName.
func (rm *ResourceManager) RenameResource(ctx context.Context, resource, newName string) error {
	log := rm.resourcesLog()
	log.Debug("RenameResource: start", zap.String("resource", resource), zap.String("new_name", newName))
	if rm.deployment == nil || rm.controller.db == nil {
		log.Debug("RenameResource: no deployment client or database", zap.String("resource", resource))
		return fmt.Errorf("deployment client or database not available")
	}
	if !resourceNameRe.MatchString(newName) {
		log.Debug("RenameResource: invalid new name", zap.String("new_name", newName))
		return fmt.Errorf("%q is not a valid resource name (letters, digits, _ and -, at most 63)", newName)
	}
	dbRes, vols, err := rm.renamePreconditions(ctx, resource, newName)
	if err != nil {
		log.Debug("RenameResource: refused", zap.String("resource", resource), zap.Error(err))
		return err
	}
	hosts, err := rm.resourceHosts(ctx, resource)
	if err != nil {
		log.Debug("RenameResource: no hosts", zap.String("resource", resource), zap.Error(err))
		return err
	}
	diskless := rm.disklessParticipantHosts(ctx, resource)
	all := append(append([]string(nil), hosts...), diskless...)
	if err := rm.assertNoSnapshots(ctx, hosts, vols); err != nil {
		log.Debug("RenameResource: has snapshots", zap.String("resource", resource), zap.Error(err))
		return err
	}

	oldPath, newPath := "/etc/drbd.d/"+resource+".res", "/etc/drbd.d/"+newName+".res"
	conf, err := rm.readResourceConfig(ctx, hosts, oldPath, resource)
	if err != nil {
		log.Debug("RenameResource: could not read config", zap.String("resource", resource), zap.Error(err))
		return err
	}
	renames := make(map[string]string, len(vols))
	for _, v := range vols {
		renames[v.VolumeName] = renamedBacking(v.VolumeName, resource, newName)
		log.Debug("RenameResource: backing volume rename", zap.String("volume", v.VolumeName), zap.String("to", renames[v.VolumeName]))
	}
	newConf, err := renameInConfig(conf, resource, newName, renames)
	if err != nil {
		log.Debug("RenameResource: could not rename config", zap.String("resource", resource), zap.Error(err))
		return err
	}

	rm.controller.logger.Info("Renaming resource", zap.String("from", resource), zap.String("to", newName))
	if err := rm.execAllSuccess(ctx, all, "sudo drbdadm down "+resource, "take "+resource+" down for the rename"); err != nil {
		log.Debug("RenameResource: down failed", zap.String("resource", resource), zap.Strings("hosts", all), zap.Error(err))
		return err
	}
	log.Debug("RenameResource: resource down", zap.String("resource", resource), zap.Strings("hosts", all))
	undo := func(renamed []*database.Volume) {
		log.Debug("RenameResource: undoing", zap.String("resource", resource), zap.Int("renamed", len(renamed)))
		for _, v := range renamed {
			_, uerr := rm.deployment.Exec(ctx, hosts, renameBackingCmd(v, renames[v.VolumeName], v.VolumeName))
			log.Debug("RenameResource: undid backing rename", zap.String("volume", v.VolumeName), zap.Error(uerr))
		}
		_, derr := rm.deployment.DistributeConfig(ctx, all, conf, oldPath)
		_, xerr := rm.deployment.Exec(ctx, all, "sudo rm -f "+newPath+"; sudo drbdadm up "+resource)
		log.Debug("RenameResource: restored old config and brought it up", zap.String("resource", resource),
			zap.NamedError("config_error", derr), zap.NamedError("up_error", xerr))
	}
	var renamed []*database.Volume
	for _, v := range vols {
		if err := rm.execAllSuccess(ctx, hosts, renameBackingCmd(v, v.VolumeName, renames[v.VolumeName]),
			"rename backing volume "+v.VolumeName); err != nil {
			log.Debug("RenameResource: backing rename failed", zap.String("volume", v.VolumeName), zap.Error(err))
			undo(renamed)
			return err
		}
		log.Debug("RenameResource: renamed backing volume", zap.String("volume", v.VolumeName), zap.String("to", renames[v.VolumeName]))
		renamed = append(renamed, v)
	}
	start := time.Now()
	_, err = rm.deployment.DistributeConfig(ctx, all, newConf, newPath)
	log.Debug("RenameResource: wrote renamed config", zap.String("path", newPath), zap.Strings("hosts", all),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		undo(renamed)
		return fmt.Errorf("write the renamed config: %w", err)
	}
	if err := rm.execAllSuccess(ctx, all, "sudo rm -f "+oldPath+" && sudo drbdadm up "+newName,
		"bring "+newName+" up"); err != nil {
		log.Debug("RenameResource: up under new name failed", zap.String("resource", newName), zap.Error(err))
		undo(renamed)
		return err
	}
	log.Debug("RenameResource: up under new name", zap.String("resource", newName))

	// The records follow the nodes: from here the resource is newName.
	dbRes.Name = newName
	err = rm.controller.db.SaveResource(ctx, dbRes)
	log.Debug("RenameResource: saved resource record", zap.String("resource", newName), zap.Error(err))
	if err != nil {
		return fmt.Errorf("renamed on the nodes, but the database could not record it: %w", err)
	}
	for _, v := range vols {
		oldVol := v.VolumeName
		if err := rm.controller.db.DeleteVolume(ctx, resource, oldVol); err != nil {
			rm.controller.logger.Warn("Rename: could not drop an old volume record", zap.String("volume", oldVol), zap.Error(err))
		}
		v.ResourceName, v.VolumeName = newName, renames[oldVol]
		v.Device = strings.Replace(v.Device, "/"+oldVol, "/"+v.VolumeName, 1)
		err := rm.controller.db.SaveVolume(ctx, v)
		log.Debug("RenameResource: re-recorded volume", zap.String("volume", v.VolumeName), zap.String("device", v.Device), zap.Error(err))
		if err != nil {
			return fmt.Errorf("renamed on the nodes, but volume %s could not be recorded: %w", v.VolumeName, err)
		}
	}
	err = rm.controller.db.DeleteResource(ctx, resource)
	log.Debug("RenameResource: dropped old resource record", zap.String("resource", resource), zap.Error(err))
	return err
}

// renamePreconditions loads the resource and refuses what a rename would
// leave pointing at the old name.
func (rm *ResourceManager) renamePreconditions(ctx context.Context, resource, newName string) (*database.Resource, []*database.Volume, error) {
	log := rm.resourcesLog()
	db := rm.controller.db
	dbRes, err := db.GetResource(ctx, resource)
	if err != nil || dbRes == nil {
		log.Debug("renamePreconditions: resource not found", zap.String("resource", resource), zap.Error(err))
		return nil, nil, fmt.Errorf("resource %q not found", resource)
	}
	if existing, err := db.GetResource(ctx, newName); err == nil && existing != nil {
		log.Debug("renamePreconditions: new name taken", zap.String("new_name", newName))
		return nil, nil, fmt.Errorf("resource %q already exists", newName)
	}
	refuse := func(why string) (*database.Resource, []*database.Volume, error) {
		log.Debug("renamePreconditions: refusing", zap.String("resource", resource), zap.String("reason", why))
		return nil, nil, fmt.Errorf("cannot rename %s: %s", resource, why)
	}
	switch {
	case dbRes.WANMode:
		return refuse("it is WAN-replicated")
	case dbRes.Encrypted:
		return refuse("it is encrypted, and its LUKS containers are named after its backing volumes")
	}
	if ha, err := db.GetHaConfig(ctx, resource); err == nil && ha != nil {
		return refuse("it has an HA config; delete it first")
	}
	if gws, err := db.ListGateways(ctx); err == nil {
		for _, g := range gws {
			if g.Resource == resource {
				return refuse("it has a gateway; delete it first")
			}
		}
	}
	if s, err := db.GetSnapshotSchedule(ctx, resource); err == nil && s != nil {
		return refuse("it has a snapshot schedule")
	}
	if backups, err := db.ListBackups(ctx, resource, ""); err == nil && len(backups) > 0 {
		return refuse("it has backups, which are recorded under its name")
	}
	if info, err := rm.GetResource(ctx, resource); err == nil {
		for node, st := range info.NodeStates {
			if strings.EqualFold(st.Role, "Primary") {
				return refuse("it is Primary on " + node + "; stop what uses it first")
			}
		}
	} else {
		log.Debug("renamePreconditions: live status unavailable; not checking roles", zap.String("resource", resource), zap.Error(err))
	}
	vols, err := db.ListVolumes(ctx, resource)
	log.Debug("renamePreconditions: listed volumes", zap.String("resource", resource), zap.Int("count", len(vols)), zap.Error(err))
	if err != nil {
		return nil, nil, err
	}
	return dbRes, vols, nil
}

// assertNoSnapshots refuses a rename while a backing volume has snapshots:
// they are named after the old backing volume and would be orphaned.
func (rm *ResourceManager) assertNoSnapshots(ctx context.Context, hosts []string, vols []*database.Volume) error {
	log := rm.resourcesLog()
	for _, v := range vols {
		cmd := fmt.Sprintf("sudo lvs --noheadings -o origin %s 2>/dev/null | grep -qw %s && echo has-snapshots || true", v.Pool, v.VolumeName)
		if strings.HasPrefix(v.Device, "/dev/zvol/") {
			cmd = fmt.Sprintf("sudo zfs list -H -t snapshot -d 1 -o name %s/%s 2>/dev/null | grep -q @ && echo has-snapshots || true", v.Pool, v.VolumeName)
		}
		start := time.Now()
		res, err := rm.deployment.Exec(ctx, hosts, cmd)
		log.Debug("assertNoSnapshots: checked volume", zap.String("volume", v.VolumeName), zap.String("pool", v.Pool),
			zap.Strings("hosts", hosts), zap.Duration("took", time.Since(start)), zap.Error(err))
		if err != nil {
			return fmt.Errorf("check %s for snapshots: %w", v.VolumeName, err)
		}
		for host, hr := range res.Hosts {
			if strings.Contains(hr.Output, "has-snapshots") {
				log.Debug("assertNoSnapshots: volume has snapshots", zap.String("volume", v.VolumeName), zap.String("host", host))
				return fmt.Errorf("cannot rename: %s has snapshots on %s, which are named after it; delete them first", v.VolumeName, host)
			}
		}
	}
	return nil
}

// renamedBacking is a backing volume's name under the new resource name.
func renamedBacking(backing, oldName, newName string) string {
	if strings.HasPrefix(backing, oldName+"_") {
		return newName + backing[len(oldName):]
	}
	return backing
}

func renameBackingCmd(v *database.Volume, from, to string) string {
	if strings.HasPrefix(v.Device, "/dev/zvol/") {
		return fmt.Sprintf("sudo zfs rename %s/%s %s/%s", v.Pool, from, v.Pool, to)
	}
	return fmt.Sprintf("sudo lvrename %s %s %s", v.Pool, from, to)
}

// renameInConfig renames the resource and its backing devices in its DRBD
// config. Backing paths are matched whole, up to the terminating ';'.
func renameInConfig(conf, oldName, newName string, backings map[string]string) (string, error) {
	header := regexp.MustCompile(`(?m)^(\s*resource\s+)"?` + regexp.QuoteMeta(oldName) + `"?(\s*\{)`)
	if !header.MatchString(conf) {
		zap.L().Debug("renameInConfig: config does not declare resource", zap.String("resource", oldName))
		return "", fmt.Errorf("the config does not declare resource %s", oldName)
	}
	out := header.ReplaceAllString(conf, "${1}"+newName+"${2}")
	for from, to := range backings {
		out = strings.ReplaceAll(out, "/"+from+";", "/"+to+";")
	}
	zap.L().Debug("renameInConfig: renamed", zap.String("resource", oldName), zap.String("new_name", newName),
		zap.Int("backings", len(backings)))
	return out, nil
}
