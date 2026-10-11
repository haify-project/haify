package controller

import (
	"context"
	"fmt"
	"regexp"
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
	switch {
	case labels[pveManagedByLabel] == "pve", labels[pveManagedByLabel] == "" && pveDefaultNameRe.MatchString(name):
		return "Proxmox VE"
	case labels[csiManagedByLabel] == "csi":
		return "the CSI driver"
	case labels[cinderManagedByLabel] == "cinder":
		return "OpenStack Cinder"
	case labels[xcpManagedByLabel] == "xcp-ng":
		return "XCP-ng"
	}
	return ""
}

// assertPromoterAllowed refuses to put a drbd-reactor promoter on resource;
// what names the operation for the error ("ha create", "an NFS gateway").
func (c *Controller) assertPromoterAllowed(ctx context.Context, resource, what string) error {
	if c.db == nil {
		return nil
	}
	res, err := c.db.GetResource(ctx, resource)
	if err != nil || res == nil {
		// Not found is the operation's own error to report.
		return nil
	}
	if owner := externallyPromoted(res.Name, res.Labels); owner != "" {
		return fmt.Errorf("%s is refused on %s: %s decides where it is Primary, and a drbd-reactor promoter "+
			"would fight it for the role", what, resource, owner)
	}
	if clients := splitCSV(res.DisklessClients); len(clients) > 0 {
		return fmt.Errorf("%s is refused on %s: it has diskless clients (%v) that use it from nodes a promoter "+
			"does not run on; detach them first", what, resource, clients)
	}
	// drbd-reactor runs one promoter per resource; an app's is already there.
	if app, err := c.db.GetAppByResource(ctx, resource); err == nil && app != nil {
		return fmt.Errorf("%s is refused on %s: the %s app %s runs on it; delete the app first (haify app delete %s)",
			what, resource, app.Engine, app.Name, app.Name)
	}
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
