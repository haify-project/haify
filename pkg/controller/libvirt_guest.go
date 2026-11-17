package controller

import (
	"context"
	"regexp"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/haify-project/haify/pkg/deployment"
	"github.com/haify-project/haify/pkg/event"
)

// libvirt guests on Haify volumes.
//
// The libvirt hook and `ha create --vm` label every resource a guest's disks
// are on with the guest's name. That label is all the controller knows about
// a guest, and it is enough to act on one from the storage side: the guest
// runs on the node where the resource is Primary, so that is where virsh is
// asked to show it a grown disk (resize_guest.go) or to freeze its
// filesystems for a snapshot (guest_quiesce.go).
//
// Whatever is done to a guest here is an addition to a storage operation
// that stands on its own. A guest that is not running, a node without virsh
// or a label naming a guest that no longer exists never fails the resize or
// the snapshot; it changes what the operation reports.

// libvirtDomainLabel is the resource label carrying the guest's name. The
// libvirt hook and the client package set it under the same key.
const libvirtDomainLabel = "haify.libvirt/domain"

// libvirtDomainRe is what a guest name may be. The name ends up inside a
// root script on the node, so a label that is anything else is not acted on.
var libvirtDomainRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,62}$`)

// guestDomain is the libvirt guest resource belongs to, or "" when it has
// none.
func (rm *ResourceManager) guestDomain(ctx context.Context, resource string) string {
	log := rm.resourcesLog()
	if rm.controller == nil || rm.controller.db == nil || resource == "" {
		log.Debug("guestDomain: no database or resource; no guest", zap.String("resource", resource))
		return ""
	}
	dbRes, err := rm.controller.db.GetResource(ctx, resource)
	if err != nil || dbRes == nil {
		log.Debug("guestDomain: resource not read; no guest", zap.String("resource", resource), zap.Error(err))
		return ""
	}
	domain := strings.TrimSpace(dbRes.Labels[libvirtDomainLabel])
	if domain == "" {
		log.Debug("guestDomain: resource has no guest label", zap.String("resource", resource))
		return ""
	}
	if !libvirtDomainRe.MatchString(domain) {
		rm.controller.logger.Warn("Ignoring a libvirt guest label that is not a guest name",
			zap.String("resource", resource), zap.String("label", libvirtDomainLabel), zap.String("value", domain))
		return ""
	}
	log.Debug("guestDomain: resource belongs to a guest", zap.String("resource", resource), zap.String("domain", domain))
	return domain
}

// guestHosts returns the hosts on which resource is Primary: where its guest
// runs, if it runs. A node holding no replica counts, because a KVM host can
// run a guest as a diskless client. During a live migration there are two.
func (rm *ResourceManager) guestHosts(ctx context.Context, resource string) []string {
	log := rm.resourcesLog()
	if rm.deployment == nil {
		log.Debug("guestHosts: no deployment client", zap.String("resource", resource))
		return nil
	}
	hosts, err := rm.resourceHosts(ctx, resource)
	if err != nil {
		log.Debug("guestHosts: no hosts", zap.String("resource", resource), zap.Error(err))
		return nil
	}
	for _, h := range rm.disklessParticipantHosts(ctx, resource) {
		if !contains(hosts, h) {
			hosts = append(hosts, h)
		}
	}
	start := time.Now()
	res, err := rm.deployment.DRBDStatus(ctx, hosts, resource)
	log.Debug("guestHosts: read roles", zap.String("resource", resource), zap.Strings("hosts", hosts),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil || res == nil {
		return nil
	}
	var primaries []string
	for _, h := range hosts {
		out, ok := appHostOutput(res, h)
		if !ok {
			log.Debug("guestHosts: no status from host", zap.String("resource", resource), zap.String("host", h))
			continue
		}
		role := parseRoleFromStatus(out)
		log.Debug("guestHosts: host role", zap.String("resource", resource), zap.String("host", h), zap.String("role", role))
		if role == "Primary" {
			primaries = append(primaries, h)
		}
	}
	log.Debug("guestHosts: Primary hosts", zap.String("resource", resource), zap.Strings("hosts", primaries))
	return primaries
}

// runGuestScript runs a root script on host and returns the key=value lines
// it printed. The scripts here always exit 0 and say what happened in their
// output, so a script that printed nothing recognisable did not run to its
// end: the caller must then assume the worst about what it left behind.
//
// The script is base64-wrapped because dispatch runs commands through
// sh -c "...", whose quoting empties every $variable a plain command carries.
// timeout replaces dispatch's default of 30 seconds, which a guest agent that
// is slow to answer would overrun.
func (rm *ResourceManager) runGuestScript(ctx context.Context, host, script string, timeout time.Duration) (map[string]string, error) {
	log := rm.resourcesLog()
	start := time.Now()
	res, err := rm.deployment.Exec(ctx, []string{host}, "echo "+base64Std(script)+" | base64 -d | sudo /bin/bash",
		deployment.WithExecTimeout(timeout))
	out, ok := appHostOutput(res, host)
	kv := keyValues(out)
	log.Debug("runGuestScript: ran", zap.String("host", host), zap.Int("script_bytes", len(script)), zap.Bool("success", ok),
		zap.Any("result", kv), zap.Duration("timeout", timeout), zap.Duration("took", time.Since(start)), zap.Error(err))
	return kv, err
}

// publishGuestEvent raises an event about resource's guest.
func (rm *ResourceManager) publishGuestEvent(t event.Type, sev event.Severity, resource, host, domain, msg string) {
	log := rm.resourcesLog()
	if rm.controller == nil || rm.controller.events == nil {
		log.Debug("publishGuestEvent: notifications off, not published", zap.String("type", string(t)), zap.String("resource", resource))
		return
	}
	log.Debug("publishGuestEvent: publishing", zap.String("type", string(t)), zap.String("resource", resource),
		zap.String("domain", domain), zap.String("host", host))
	rm.controller.events.Publish(event.Event{
		Type: t, Severity: sev, Status: event.StatusInfo,
		Resource: resource, Node: rm.nodeLabel(host), Message: msg,
		Details: map[string]string{"domain": domain},
	})
}

// keyedLocks is one lock per key, taken with a context so a caller whose
// request is cancelled stops waiting.
type keyedLocks struct {
	mu   sync.Mutex
	held map[string]chan struct{}
}

// lock waits for key's lock and returns what releases it.
func (k *keyedLocks) lock(ctx context.Context, key string) (func(), error) {
	for {
		k.mu.Lock()
		if k.held == nil {
			k.held = map[string]chan struct{}{}
		}
		busy, taken := k.held[key]
		if !taken {
			mine := make(chan struct{})
			k.held[key] = mine
			k.mu.Unlock()
			return func() {
				k.mu.Lock()
				delete(k.held, key)
				k.mu.Unlock()
				close(mine)
			}, nil
		}
		k.mu.Unlock()
		select {
		case <-busy:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}
