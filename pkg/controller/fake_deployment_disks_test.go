package controller

import (
	"context"
	"fmt"
	"strings"

	"github.com/haify-project/haify/pkg/deployment"
)

// ListBlockDevices defaults to a node whose sdb..sdh, vdb..vdd and nvme0n1
// are empty 100 GiB disks, so tests that are not about disk checks can name
// any of them.
func (f *fakeDeploymentClient) ListBlockDevices(_ context.Context, host string) (*deployment.ExecResult, error) {
	if f.listBlockDevicesFunc != nil {
		return f.listBlockDevicesFunc(host)
	}
	return successExecResult([]string{host}, freeDisksListing()), nil
}

func freeDisksListing() string {
	var b strings.Builder
	devs := []string{"/dev/nvme0n1"}
	for _, c := range "bcdefgh" {
		devs = append(devs, "/dev/sd"+string(c))
	}
	for _, c := range "bcd" {
		devs = append(devs, "/dev/vd"+string(c))
	}
	for _, d := range devs {
		fmt.Fprintf(&b, "D|%s||107374182400|disk|0|0||||\nB|%s||\n", d, d)
	}
	return b.String()
}

func (f *fakeDeploymentClient) WipeDevice(_ context.Context, host, device string, _ []string) (*deployment.ExecResult, error) {
	f.wipeCalls = append(f.wipeCalls, device)
	return successExecResult([]string{host}, ""), nil
}

func (f *fakeDeploymentClient) ZFSAddVdev(_ context.Context, host, pool, device string) (*deployment.ExecResult, error) {
	f.zfsAddCalls = append(f.zfsAddCalls, pool+" "+device)
	return successExecResult([]string{host}, ""), nil
}

func (f *fakeDeploymentClient) ProbeBlockDevice(ctx context.Context, host, device string) (*deployment.ExecResult, error) {
	if f.probeBlockDeviceFunc != nil {
		return f.probeBlockDeviceFunc(ctx, host, device)
	}
	return successExecResult([]string{host}, ""), nil
}

func (f *fakeDeploymentClient) VGExtend(ctx context.Context, hosts []string, vgName, device string) (*deployment.ExecResult, error) {
	if f.vgExtendFunc != nil {
		return f.vgExtendFunc(ctx, hosts, vgName, device)
	}
	return successExecResult(hosts, ""), nil
}
