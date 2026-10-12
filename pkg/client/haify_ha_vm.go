package client

import (
	"fmt"
	"regexp"
	"strings"

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
