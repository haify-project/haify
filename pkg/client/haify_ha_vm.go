package client

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	haifypb "github.com/haify-project/haify/api/proto/v1"
	"go.uber.org/zap"
)

// LibvirtDomainLabel marks the resources a libvirt guest's disks are on, with
// the guest's name. The libvirt hook sets it when it starts a guest, and
// `ha create --vm` when it hands one to drbd-reactor; the web UI lists guests
// by it.
const LibvirtDomainLabel = "haify.libvirt/domain"

// A libvirt domain name as the VirtualDomain agent and a promoter line can
// carry it unquoted: it becomes part of a file path and of an OCF instance id.
var vmDomainRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.+-]{0,62}$`)

// VirtualDomainAgent is the promoter entry that runs a libvirt guest where the
// resource is Primary: `ha create --vm`. The guest must be defined on every
// diskful replica under the same name (virsh define, autostart off), with its
// disks on /dev/drbd/by-res/<resource>/<volume>; drbd-reactor promotes the
// resource, the agent starts the guest, and a node failure restarts it on
// another replica. The agent reads the definition from the file libvirt keeps
// for a defined guest.
func VirtualDomainAgent(domain string) (*haifypb.OcfAgent, error) {
	log := zap.L()
	log.Debug("VirtualDomainAgent: building VirtualDomain agent", zap.String("domain", domain))
	domain = strings.TrimSpace(domain)
	if !vmDomainRe.MatchString(domain) {
		log.Debug("VirtualDomainAgent: invalid libvirt domain name", zap.String("domain", domain))
		return nil, fmt.Errorf("VM %q: a libvirt domain name is 1-63 letters, digits and . _ + -, starting with a letter or digit", domain)
	}
	return &haifypb.OcfAgent{
		Provider: "heartbeat",
		Name:     "VirtualDomain",
		Instance: "vm_" + strings.NewReplacer(".", "_", "+", "_", "-", "_").Replace(domain),
		Params: map[string]string{
			"config":     "/etc/libvirt/qemu/" + domain + ".xml",
			"hypervisor": "qemu:///system",
		},
	}, nil
}

// EvictHaLive starts a live move of an HA guest to target ("" lets the
// controller pick) and returns once it is under way; GetHaMigration follows
// it. timeout bounds the memory copy, 0 for the controller's default.
func (c *HaifyClient) EvictHaLive(ctx context.Context, resource, target string, timeout time.Duration) (*haifypb.EvictHaResponse, error) {
	log := zap.L()
	log.Debug("HaifyClient.EvictHaLive: sending request", zap.String("resource", resource), zap.String("target", target),
		zap.Duration("timeout", timeout))
	resp, err := c.client.EvictHa(ctx, &haifypb.EvictHaRequest{Resource: resource, Live: true, TargetNode: target,
		TimeoutSeconds: uint32(timeout / time.Second)})
	if err != nil {
		log.Debug("HaifyClient.EvictHaLive: EvictHa failed", zap.Error(err))
		return nil, err
	}
	if !resp.Success {
		log.Debug("HaifyClient.EvictHaLive: controller refused", zap.String("message", resp.Message))
		return nil, fmt.Errorf("%s", resp.Message)
	}
	log.Debug("HaifyClient.EvictHaLive: started", zap.String("source", resp.SourceNode), zap.String("target", resp.TargetNode))
	return resp, nil
}

// GetHaMigration returns the live move of resource's guest that is running,
// or the last one that ran; nil when there never was one.
func (c *HaifyClient) GetHaMigration(ctx context.Context, resource string) (*haifypb.HaMigration, error) {
	log := zap.L()
	log.Debug("HaifyClient.GetHaMigration: sending request", zap.String("resource", resource))
	resp, err := c.client.GetHaMigration(ctx, &haifypb.GetHaMigrationRequest{Resource: resource})
	if err != nil {
		log.Debug("HaifyClient.GetHaMigration: GetHaMigration failed", zap.Error(err))
		return nil, err
	}
	if !resp.Success {
		log.Debug("HaifyClient.GetHaMigration: controller reported failure", zap.String("message", resp.Message))
		return nil, fmt.Errorf("%s", resp.Message)
	}
	log.Debug("HaifyClient.GetHaMigration: done", zap.Bool("found", resp.Migration != nil))
	return resp.Migration, nil
}

// WaitHaMigration follows a live move until it is done or ctx ends, calling
// progress (which may be nil) whenever what it would show changes.
func (c *HaifyClient) WaitHaMigration(ctx context.Context, resource string, every time.Duration, progress func(*haifypb.HaMigration)) (*haifypb.HaMigration, error) {
	log := zap.L()
	log.Debug("HaifyClient.WaitHaMigration: following", zap.String("resource", resource), zap.Duration("every", every))
	last := ""
	var m *haifypb.HaMigration
	for {
		got, err := c.GetHaMigration(ctx, resource)
		switch {
		case err != nil && ctx.Err() != nil:
			return m, ctx.Err()
		case err != nil:
			// The controller may be moving itself; the move goes on without
			// this client, so keep asking.
			log.Debug("HaifyClient.WaitHaMigration: status unavailable; retrying", zap.Error(err))
		case got != nil:
			m = got
			if seen := fmt.Sprintf("%s|%s|%d|%d", m.GetPhase(), m.GetMessage(), m.GetProgressPercent(), len(m.GetSteps())); seen != last {
				last = seen
				if progress != nil {
					progress(m)
				}
			}
			if m.GetDone() {
				log.Debug("HaifyClient.WaitHaMigration: done", zap.String("outcome", m.GetOutcome()))
				return m, nil
			}
		}
		select {
		case <-ctx.Done():
			return m, ctx.Err()
		case <-time.After(every):
		}
	}
}

// GetVmDefinition reports how each replica's copy of an HA guest's
// definition compares with the one the controller keeps.
func (c *HaifyClient) GetVmDefinition(ctx context.Context, resource string) (*haifypb.VmDefinitionResponse, error) {
	log := zap.L()
	log.Debug("HaifyClient.GetVmDefinition: sending request", zap.String("resource", resource))
	resp, err := c.client.GetVmDefinition(ctx, &haifypb.GetVmDefinitionRequest{Resource: resource})
	if err != nil {
		log.Debug("HaifyClient.GetVmDefinition: GetVmDefinition failed", zap.Error(err))
		return nil, err
	}
	if !resp.Success {
		log.Debug("HaifyClient.GetVmDefinition: controller reported failure", zap.String("message", resp.Message))
		return nil, fmt.Errorf("%s", resp.Message)
	}
	log.Debug("HaifyClient.GetVmDefinition: done", zap.Bool("consistent", resp.Consistent))
	return resp, nil
}

// SyncVmDefinition takes an HA guest's definition from one node (from, or
// the controller's choice when empty) and defines it on the other replicas.
// On a partial failure it returns both the per-node result and the error.
func (c *HaifyClient) SyncVmDefinition(ctx context.Context, resource, from string) (*haifypb.VmDefinitionResponse, error) {
	log := zap.L()
	log.Debug("HaifyClient.SyncVmDefinition: sending request", zap.String("resource", resource), zap.String("from", from))
	resp, err := c.client.SyncVmDefinition(ctx, &haifypb.SyncVmDefinitionRequest{Resource: resource, FromNode: from})
	if err != nil {
		log.Debug("HaifyClient.SyncVmDefinition: SyncVmDefinition failed", zap.Error(err))
		return nil, err
	}
	if !resp.Success {
		log.Debug("HaifyClient.SyncVmDefinition: controller reported failure", zap.String("message", resp.Message))
		return resp, fmt.Errorf("%s", resp.Message)
	}
	log.Debug("HaifyClient.SyncVmDefinition: done", zap.Bool("consistent", resp.Consistent))
	return resp, nil
}
