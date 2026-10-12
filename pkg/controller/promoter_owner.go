package controller

import (
	"context"
	"fmt"
	"regexp"
	"time"

	"go.uber.org/zap"
)

// Resources whose Primary another system decides.
//
// A drbd-reactor promoter — what `ha create` and every gateway write — makes a
// resource Primary on whichever replica it can and keeps it there. Some
// resources already have a manager deciding that:
//
//   - a Proxmox VM disk is made Primary by the storage plugin on the node PVE
//     starts the VM on, and moved by PVE live migration and PVE HA;
//   - a CSI volume is made Primary by the CSI node plugin on the node the pod
//     is scheduled to;
//   - a Cinder volume is made Primary by the OpenStack driver on the compute
//     host Nova runs the instance on, and moved by Nova live migration;
//   - an XCP-ng virtual disk is made Primary by the SR driver on the host the
//     VM runs on, and moved by XAPI live migration;
//   - a resource with diskless clients is used from nodes the promoter never
//     runs on (the Proxmox plugin attaches one where a VM runs without a local
//     replica).
//
// A promoter on any of them fights that manager for the role: it promotes a
// replica while the VM or pod runs elsewhere, or demotes the node it runs on
// when its own start chain fails. So both are refused up front.

const (
	// pveManagedByLabel marks a resource the Proxmox plugin created.
	pveManagedByLabel = "haify.pve/managed-by"
	// csiManagedByLabel marks a resource the CSI driver created.
	csiManagedByLabel = "haify.csi/managed-by"
	// csiPVCLabel names the claim a CSI volume was provisioned for.
	csiPVCLabel = "haify.csi/pvc"
	// cinderManagedByLabel marks a resource the OpenStack Cinder driver created.
	cinderManagedByLabel = "haify.openstack/managed-by"
	// xcpManagedByLabel marks a resource the XCP-ng SR driver created.
	xcpManagedByLabel = "haify.xcp/managed-by"
)

// pveDefaultNameRe matches what the Proxmox plugin names a volume under its
// default prefix ("pve-<vmid>-<n>", "pve-<vmid>-cloudinit"). Disks created
// before the plugin labelled them are recognised by it.
var pveDefaultNameRe = regexp.MustCompile(`^pve-\d+-[A-Za-z0-9][A-Za-z0-9_-]*$`)

// externallyPromoted names the system that decides where a resource is
// Primary, or "" when haify itself may.
func externallyPromoted(name string, labels map[string]string) string {
	owner := ""
	switch {
	case labels[pveManagedByLabel] == "pve", labels[pveManagedByLabel] == "" && pveDefaultNameRe.MatchString(name):
		owner = "Proxmox VE"
	case labels[csiManagedByLabel] == "csi":
		owner = "the CSI driver"
	case labels[cinderManagedByLabel] == "cinder":
		owner = "OpenStack Cinder"
	case labels[xcpManagedByLabel] == "xcp-ng":
		owner = "XCP-ng"
	}
	zap.L().Debug("externallyPromoted: decided", zap.String("resource", name), zap.String("owner", owner))
	return owner
}

// assertPromoterAllowed refuses to put a drbd-reactor promoter on resource;
// what names the operation for the error ("ha create", "an NFS gateway").
func (c *Controller) assertPromoterAllowed(ctx context.Context, resource, what string) error {
	log := c.traceLogger()
	log.Debug("Controller.assertPromoterAllowed: start", zap.String("resource", resource), zap.String("what", what))
	if c.db == nil {
		log.Debug("Controller.assertPromoterAllowed: no database, allowed", zap.String("resource", resource))
		return nil
	}
	start := time.Now()
	res, err := c.db.GetResource(ctx, resource)
	log.Debug("Controller.assertPromoterAllowed: database read resource", zap.String("resource", resource),
		zap.Bool("found", err == nil && res != nil), zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil || res == nil {
		// Not found is the operation's own error to report.
		return nil
	}
	if owner := externallyPromoted(res.Name, res.Labels); owner != "" {
		log.Debug("Controller.assertPromoterAllowed: refused, promoted externally", zap.String("resource", resource),
			zap.String("owner", owner))
		return fmt.Errorf("%s is refused on %s: %s decides where it is Primary, and a drbd-reactor promoter "+
			"would fight it for the role", what, resource, owner)
	}
	if clients := splitCSV(res.DisklessClients); len(clients) > 0 {
		log.Debug("Controller.assertPromoterAllowed: refused, has diskless clients", zap.String("resource", resource),
			zap.Strings("clients", clients))
		return fmt.Errorf("%s is refused on %s: it has diskless clients (%v) that use it from nodes a promoter "+
			"does not run on; detach them first", what, resource, clients)
	}
	// drbd-reactor runs one promoter per resource; an app's is already there.
	start = time.Now()
	app, err := c.db.GetAppByResource(ctx, resource)
	log.Debug("Controller.assertPromoterAllowed: database read app", zap.String("resource", resource),
		zap.Bool("found", err == nil && app != nil), zap.Duration("took", time.Since(start)), zap.Error(err))
	if err == nil && app != nil {
		log.Debug("Controller.assertPromoterAllowed: refused, an app runs on it", zap.String("resource", resource),
			zap.String("app", app.Name), zap.String("engine", app.Engine))
		return fmt.Errorf("%s is refused on %s: the %s app %s runs on it; delete the app first (haify app delete %s)",
			what, resource, app.Engine, app.Name, app.Name)
	}
	log.Debug("Controller.assertPromoterAllowed: allowed", zap.String("resource", resource))
	return nil
}

// resourceOwner names the resource as the system that created it knows it,
// for messages: the claim of a CSI volume. Empty when no label says.
func resourceOwner(labels map[string]string) string {
	if pvc := labels[csiPVCLabel]; pvc != "" && labels[csiManagedByLabel] == "csi" {
		return "PVC " + pvc
	}
	return ""
}
