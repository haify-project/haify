package controller

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	haifypb "github.com/haify-project/haify/api/proto/v1"
	"github.com/haify-project/haify/pkg/database"
)

// A promoter on a resource another system promotes fights it for Primary:
// ha create and every gateway are refused before they touch a node.
func TestPromoterRefusedOnExternallyPromotedResources(t *testing.T) {
	dep := &fakeDeploymentClient{}
	ctrl := newBasicTestController(dep)
	ctrl.db = newTestDB(t)
	ctx := context.Background()
	for _, r := range []*database.Resource{
		{Name: "vmdisk", Nodes: "n1,n2", Labels: map[string]string{pveManagedByLabel: "pve"}},
		{Name: "pve-100-0", Nodes: "n1,n2"},
		{Name: "pvc-1", Nodes: "n1,n2", Labels: map[string]string{csiManagedByLabel: "csi"}},
		{Name: "attached", Nodes: "n1,n2", DisklessClients: "n3"},
	} {
		require.NoError(t, ctrl.db.SaveResource(ctx, r))
	}

	for resource, want := range map[string]string{
		"vmdisk":    "Proxmox VE decides",
		"pve-100-0": "Proxmox VE decides",
		"pvc-1":     "the CSI driver decides",
		"attached":  "diskless clients ([n3])",
	} {
		_, err := ctrl.resources.MakeHa(ctx, resource, []string{"app.service"}, "", "", "", nil, nil)
		require.Error(t, err, resource)
		assert.Contains(t, err.Error(), want, resource)
	}

	srv := &Server{ctrl: ctrl, resources: ctrl.resources}
	_, err := srv.CreateNFSGateway(ctx, &haifypb.CreateNFSGatewayRequest{Resource: "vmdisk"})
	assert.Equal(t, codes.FailedPrecondition, status.Code(err))
	_, err = srv.CreateISCSIGateway(ctx, &haifypb.CreateISCSIGatewayRequest{Resource: "attached"})
	assert.Equal(t, codes.FailedPrecondition, status.Code(err))
	_, err = srv.CreateNVMeGateway(ctx, &haifypb.CreateNVMeGatewayRequest{Resource: "pvc-1"})
	assert.Equal(t, codes.FailedPrecondition, status.Code(err))
	assert.Empty(t, dep.execCalls, "nothing reached a node")
}

func TestExternallyPromoted(t *testing.T) {
	assert.Empty(t, externallyPromoted("data", nil))
	assert.Empty(t, externallyPromoted("pve-data", nil), "only the plugin's own naming is recognised")
	assert.Equal(t, "Proxmox VE", externallyPromoted("pve-100-cloudinit", nil))
	assert.Empty(t, externallyPromoted("pve-100-0", map[string]string{pveManagedByLabel: "other"}),
		"an explicit label wins over the name")
	assert.Equal(t, "Proxmox VE", externallyPromoted("disk", map[string]string{pveManagedByLabel: "pve"}))
	assert.Equal(t, "OpenStack Cinder", externallyPromoted("os-0d6f", map[string]string{cinderManagedByLabel: "cinder"}))
	assert.Equal(t, "the CSI driver", externallyPromoted("pvc-1", map[string]string{csiManagedByLabel: "csi"}))
	assert.Empty(t, externallyPromoted("pvc-2", map[string]string{csiManagedByLabel: "csi", csiSharedLabel: "nfs"}),
		"a shared CSI volume is served by its NFS gateway, whose promoter decides the Primary")
}

func TestResourceOwner(t *testing.T) {
	assert.Equal(t, "PVC db/data-postgres-0",
		resourceOwner(map[string]string{csiManagedByLabel: "csi", csiPVCLabel: "db/data-postgres-0"}))
	assert.Empty(t, resourceOwner(map[string]string{csiManagedByLabel: "csi"}), "a volume from before the claim was recorded")
	assert.Empty(t, resourceOwner(map[string]string{csiPVCLabel: "db/x"}), "a label the CSI driver did not set")
	assert.Empty(t, resourceOwner(nil))
}
