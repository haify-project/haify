package csi

import (
	"context"
	"fmt"

	"go.uber.org/zap"
)

// What ControllerModifyVolume needs of the sanity backend (volumeModifier),
// so the suite's VolumeAttributesClass tests run against the real handler.

func (b *sanityBackend) AddReplica(_ context.Context, resource, node string) error {
	zap.L().Debug("sanityBackend.AddReplica: adding", zap.String("resource", resource), zap.String("node", node))
	b.mu.Lock()
	defer b.mu.Unlock()
	r, ok := b.resources[resource]
	if !ok {
		return fmt.Errorf("resource %q not found", resource)
	}
	r.Nodes = append(append([]string{}, r.Nodes...), node)
	return nil
}

func (b *sanityBackend) RemoveReplica(_ context.Context, resource, node string) error {
	zap.L().Debug("sanityBackend.RemoveReplica: removing", zap.String("resource", resource), zap.String("node", node))
	b.mu.Lock()
	defer b.mu.Unlock()
	r, ok := b.resources[resource]
	if !ok {
		return fmt.Errorf("resource %q not found", resource)
	}
	var kept []string
	for _, n := range r.Nodes {
		if n != node {
			kept = append(kept, n)
		}
	}
	r.Nodes = kept
	return nil
}

func (b *sanityBackend) SetResourceLabels(_ context.Context, resource string, set map[string]string, remove []string) (map[string]string, error) {
	zap.L().Debug("sanityBackend.SetResourceLabels: updating", zap.String("resource", resource), zap.Int("set", len(set)),
		zap.Strings("remove", remove))
	b.mu.Lock()
	defer b.mu.Unlock()
	r, ok := b.resources[resource]
	if !ok {
		return nil, fmt.Errorf("resource %q not found", resource)
	}
	labels := map[string]string{}
	for k, v := range r.Labels {
		labels[k] = v
	}
	for k, v := range set {
		if v == "" {
			delete(labels, k)
			continue
		}
		labels[k] = v
	}
	for _, k := range remove {
		delete(labels, k)
	}
	r.Labels = labels
	return labels, nil
}

func (b *sanityBackend) SetResourceProfile(_ context.Context, resource, profile string) error {
	zap.L().Debug("sanityBackend.SetResourceProfile: setting", zap.String("resource", resource), zap.String("profile", profile))
	b.mu.Lock()
	defer b.mu.Unlock()
	r, ok := b.resources[resource]
	if !ok {
		return fmt.Errorf("resource %q not found", resource)
	}
	r.Profile = profile
	return nil
}
