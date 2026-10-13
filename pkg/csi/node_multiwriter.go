package csi

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// A ReadWriteMany raw block volume exists for one thing: live migration of a
// virtual machine (KubeVirt / OpenShift Virtualization). The target node
// stages the volume while the source still runs the guest, so for a short
// window both nodes are DRBD Primary; the hypervisor keeps only one of them
// writing. DRBD allows that only with allow-two-primaries, which is opened
// here when a second Primary is actually needed and closed again as soon as
// one Primary is left. A filesystem cannot be shared like this, so the
// controller refuses ReadWriteMany for filesystem volumes.

// multiWriterMarker, in a node's staging directory, records that the node
// staged the volume as ReadWriteMany block, so its unstage knows to close
// dual-primary once a single Primary remains.
const multiWriterMarker = ".haify-multi-writer"

func isMultiWriterBlock(c *csi.VolumeCapability) bool {
	return c.GetBlock() != nil && c.GetAccessMode().GetMode() == csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER
}

// primariesOf lists the nodes that are DRBD Primary for resource, by Haify
// node name where the controller gives one.
func (s *nodeServer) primariesOf(ctx context.Context, resource string) ([]string, error) {
	start := time.Now()
	r, err := s.backend.GetResource(ctx, resource)
	if err != nil {
		s.log.Debug("nodeServer.primariesOf: get resource failed", zap.String("resource", resource),
			zap.Duration("took", time.Since(start)), zap.Error(err))
		return nil, status.Errorf(codes.Internal, "get resource %q: %v", resource, err)
	}
	var primaries []string
	for host, st := range r.GetNodeStates() {
		if !strings.EqualFold(st.GetRole(), "Primary") {
			continue
		}
		name := st.GetNode()
		if name == "" {
			name = host
		}
		primaries = append(primaries, name)
	}
	s.log.Debug("nodeServer.primariesOf: done", zap.String("resource", resource), zap.Strings("primaries", primaries),
		zap.Duration("took", time.Since(start)))
	return primaries, nil
}

// openMultiWriter runs before this node promotes a ReadWriteMany block volume.
// When another node is already Primary — the source of a live migration — it
// allows two Primaries; otherwise nothing changes. Either way it marks the
// staging directory so this node's unstage closes the window again.
func (s *nodeServer) openMultiWriter(ctx context.Context, resource, staging string) error {
	primaries, err := s.primariesOf(ctx, resource)
	if err != nil {
		return err
	}
	var others []string
	for _, p := range primaries {
		if p != s.nodeName {
			others = append(others, p)
		}
	}
	if err := s.mounter.EnsureDir(staging); err != nil {
		s.log.Debug("nodeServer.openMultiWriter: mkdir staging failed", zap.String("staging", staging), zap.Error(err))
		return status.Errorf(codes.Internal, "mkdir staging: %v", err)
	}
	if err := os.WriteFile(filepath.Join(staging, multiWriterMarker), []byte(resource+"\n"), 0o600); err != nil {
		s.log.Debug("nodeServer.openMultiWriter: writing marker failed", zap.String("staging", staging), zap.Error(err))
		return status.Errorf(codes.Internal, "mark staging: %v", err)
	}
	if len(others) == 0 {
		s.log.Debug("nodeServer.openMultiWriter: no other Primary, single-Primary attach", zap.String("resource", resource))
		return nil
	}
	start := time.Now()
	err = s.backend.SetDualPrimary(ctx, resource, true)
	s.log.Debug("nodeServer.openMultiWriter: allowed two Primaries", zap.String("resource", resource),
		zap.Strings("other_primaries", others), zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return status.Errorf(codes.Internal, "allow two primaries for %q (Primary on %s): %v", resource, strings.Join(others, ", "), err)
	}
	s.log.Info("ReadWriteMany block volume Primary on two nodes for a live migration",
		zap.String("resource", resource), zap.String("node", s.nodeName), zap.Strings("other_primaries", others))
	return nil
}

// closeMultiWriter runs after this node demoted a volume it staged as
// ReadWriteMany block: once at most one Primary is left, two Primaries are no
// longer allowed. It does not fail the unstage — the volume is already
// released here — but says so when the window stays open.
func (s *nodeServer) closeMultiWriter(ctx context.Context, resource, staging string) {
	marker := filepath.Join(staging, multiWriterMarker)
	if _, err := os.Stat(marker); err != nil {
		s.log.Debug("nodeServer.closeMultiWriter: not a ReadWriteMany block staging", zap.String("resource", resource),
			zap.Bool("missing", errors.Is(err, os.ErrNotExist)))
		return
	}
	// kubelet removes the staging directory after unstage; leaving the marker
	// would make that fail.
	rmErr := os.Remove(marker)
	s.log.Debug("nodeServer.closeMultiWriter: marker removed", zap.String("marker", marker), zap.Error(rmErr))
	primaries, err := s.primariesOf(ctx, resource)
	if err != nil {
		s.log.Warn("could not check the Primaries of a ReadWriteMany volume; two Primaries may stay allowed",
			zap.String("resource", resource), zap.Error(err))
		return
	}
	if len(primaries) > 1 {
		s.log.Debug("nodeServer.closeMultiWriter: still two Primaries, leaving dual-primary open", zap.String("resource", resource),
			zap.Strings("primaries", primaries))
		return
	}
	start := time.Now()
	err = s.backend.SetDualPrimary(ctx, resource, false)
	s.log.Debug("nodeServer.closeMultiWriter: disallowed two Primaries", zap.String("resource", resource),
		zap.Strings("primaries", primaries), zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		s.log.Warn("could not disallow two Primaries after a live migration", zap.String("resource", resource), zap.Error(err))
	}
}
