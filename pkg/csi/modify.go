package csi

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	haifypb "github.com/haify-project/haify/api/proto/v1"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Changing a provisioned volume through a Kubernetes VolumeAttributesClass.
//
// external-resizer calls ControllerModifyVolume with the class's parameters
// when a PVC's volumeAttributesClassName changes, and external-provisioner
// passes them as mutable_parameters to CreateVolume when a PVC names a class
// from the start. The keys are the StorageClass parameter names, so one name
// means the same thing in both places.
//
// The CSI spec says a key that is absent is left alone: only the keys a class
// names change anything. That is why a QoS limit is lifted with the explicit
// value "max" rather than by leaving the key out.

const (
	paramReplicas        = "replicas"
	paramResourceProfile = "resourceProfile"

	// qosUnlimited lifts a limit; it is the word io.max itself uses.
	qosUnlimited = "max"

	// qosLabelPrefix names the resource labels that carry the I/O limits set
	// through a VolumeAttributesClass, one per parameter
	// ("haify.csi/qos-readIOPS=5000"). A volume's context is fixed when it is
	// provisioned, so the node plugin reads these to learn a changed limit;
	// a label wins over the same key in the volume context.
	qosLabelPrefix = "haify.csi/qos-"
)

// volumeModifier is what ControllerModifyVolume needs beyond HaifyBackend.
// *client.HaifyClient has all of it; a backend without it can still
// provision, but cannot change a volume afterwards.
type volumeModifier interface {
	AddReplica(ctx context.Context, resource, node string) error
	RemoveReplica(ctx context.Context, resource, node string) error
	SetResourceLabels(ctx context.Context, resource string, set map[string]string, remove []string) (map[string]string, error)
	SetResourceProfile(ctx context.Context, resource, profile string) error
}

// mutableChange is a validated set of mutable parameters. A field left at its
// zero value (nil, 0) was not given and is not changed.
type mutableChange struct {
	replicas int
	profile  *string
	// qos maps a QoS parameter to its new value: a number, or qosUnlimited.
	qos map[string]string
}

func (c mutableChange) empty() bool {
	return c.replicas == 0 && c.profile == nil && len(c.qos) == 0
}

// mutableParameterNames lists the keys a VolumeAttributesClass may set, for
// error messages.
func mutableParameterNames() []string {
	names := []string{paramReplicas, paramResourceProfile}
	for p := range qosParams {
		names = append(names, p)
	}
	sort.Strings(names)
	return names
}

// parseMutableParameters validates a VolumeAttributesClass's parameters.
// Every error is the caller's to fix, so each maps to InvalidArgument, which
// external-resizer records as infeasible instead of retrying it forever.
func parseMutableParameters(p map[string]string) (mutableChange, error) {
	log := zap.L()
	log.Debug("parseMutableParameters: start", zap.Int("parameters", len(p)))
	var out mutableChange
	keys := make([]string, 0, len(p))
	for k := range p {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, key := range keys {
		raw := strings.TrimSpace(p[key])
		switch {
		case key == paramReplicas:
			n, err := strconv.Atoi(raw)
			if err != nil || n < 1 {
				log.Debug("parseMutableParameters: invalid replicas", zap.String("value", raw))
				return out, fmt.Errorf("invalid %s %q (want a whole number of at least 1)", paramReplicas, raw)
			}
			out.replicas = n
		case key == paramResourceProfile:
			v := raw
			out.profile = &v
		case qosParams[key] != "":
			if strings.EqualFold(raw, qosUnlimited) {
				log.Debug("parseMutableParameters: limit lifted", zap.String("param", key))
				if out.qos == nil {
					out.qos = map[string]string{}
				}
				out.qos[key] = qosUnlimited
				continue
			}
			parsed, err := parseQoS(map[string]string{key: raw})
			if err != nil || parsed[key] == "" {
				log.Debug("parseMutableParameters: invalid limit", zap.String("param", key), zap.String("value", raw))
				return out, fmt.Errorf("invalid %s %q (want a positive number, or %q for no limit)", key, raw, qosUnlimited)
			}
			if out.qos == nil {
				out.qos = map[string]string{}
			}
			out.qos[key] = parsed[key]
		default:
			log.Debug("parseMutableParameters: unsupported key", zap.String("key", key))
			return out, fmt.Errorf("parameter %q cannot be set by a VolumeAttributesClass; supported: %s",
				key, strings.Join(mutableParameterNames(), ", "))
		}
	}
	log.Debug("parseMutableParameters: parsed", zap.Int("replicas", out.replicas), zap.Bool("profile", out.profile != nil),
		zap.Int("qos", len(out.qos)))
	return out, nil
}

// mergeMutableParameters folds a VolumeAttributesClass's parameters into the
// StorageClass's for CreateVolume: the class wins, and a lifted limit ("max")
// leaves the key out, which is what no limit looks like at creation.
func mergeMutableParameters(sc, mutable map[string]string) (map[string]string, error) {
	zap.L().Debug("mergeMutableParameters: start", zap.Int("storageclass", len(sc)), zap.Int("mutable", len(mutable)))
	if len(mutable) == 0 {
		return sc, nil
	}
	if _, err := parseMutableParameters(mutable); err != nil {
		return nil, err
	}
	out := make(map[string]string, len(sc)+len(mutable))
	for k, v := range sc {
		out[k] = v
	}
	for k, v := range mutable {
		if qosParams[k] != "" && strings.EqualFold(strings.TrimSpace(v), qosUnlimited) {
			zap.L().Debug("mergeMutableParameters: no limit", zap.String("param", k))
			delete(out, k)
			continue
		}
		out[k] = v
	}
	return out, nil
}

// ControllerModifyVolume changes a provisioned volume: its number of diskful
// replicas, its resource profile, and its I/O limits. It is idempotent: each
// part compares what it wants with what the volume has and does only the
// difference, so a retry after a partial failure finishes the job.
func (s *controllerServer) ControllerModifyVolume(ctx context.Context, req *csi.ControllerModifyVolumeRequest) (*csi.ControllerModifyVolumeResponse, error) {
	id := req.GetVolumeId()
	s.log.Debug("controllerServer.ControllerModifyVolume: start", zap.String("resource", id),
		zap.Int("parameters", len(req.GetMutableParameters())), zap.Int("secrets", len(req.GetSecrets())))
	if id == "" {
		s.log.Debug("controllerServer.ControllerModifyVolume: rejected, no volume id")
		return nil, status.Error(codes.InvalidArgument, "volume id is required")
	}
	change, err := parseMutableParameters(req.GetMutableParameters())
	if err != nil {
		s.log.Debug("controllerServer.ControllerModifyVolume: invalid parameters", zap.String("resource", id), zap.Error(err))
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	start := time.Now()
	r, err := s.backend.GetResource(ctx, id)
	s.log.Debug("controllerServer.ControllerModifyVolume: looked up volume", zap.String("resource", id),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return nil, lookupError(id, err)
	}
	if change.empty() {
		s.log.Debug("controllerServer.ControllerModifyVolume: nothing to change", zap.String("resource", id))
		return &csi.ControllerModifyVolumeResponse{}, nil
	}
	mod, ok := s.backend.(volumeModifier)
	if !ok {
		s.log.Debug("controllerServer.ControllerModifyVolume: backend cannot modify volumes", zap.String("resource", id))
		return nil, status.Error(codes.Unimplemented, "this Haify backend cannot change a volume after provisioning")
	}
	if err := s.checkProfileExists(ctx, change.profile); err != nil {
		return nil, err
	}
	// Cheapest first: labels and profile are database writes on the
	// controller, a replica change copies the whole volume.
	if err := s.modifyQoS(ctx, mod, r, change.qos); err != nil {
		return nil, err
	}
	if err := s.modifyProfile(ctx, mod, r, change.profile); err != nil {
		return nil, err
	}
	if change.replicas > 0 {
		if err := s.modifyReplicas(ctx, mod, r, change.replicas); err != nil {
			return nil, err
		}
	}
	s.log.Debug("controllerServer.ControllerModifyVolume: done", zap.String("resource", id))
	return &csi.ControllerModifyVolumeResponse{}, nil
}

// lookupError turns a failed GetResource into a CSI status: a connection
// problem stays retryable, anything else means the volume is not there.
func lookupError(id string, err error) error {
	switch status.Code(err) {
	case codes.Unavailable, codes.DeadlineExceeded, codes.Canceled:
		zap.L().Debug("lookupError: controller unreachable", zap.String("resource", id), zap.Error(err))
		return status.Errorf(codes.Unavailable, "get volume %q: %v", id, err)
	}
	zap.L().Debug("lookupError: volume not found", zap.String("resource", id), zap.Error(err))
	return status.Errorf(codes.NotFound, "volume %q not found: %v", id, err)
}

// checkProfileExists refuses a profile the controller does not know before
// anything is changed, so a typo is reported as the class's fault.
func (s *controllerServer) checkProfileExists(ctx context.Context, profile *string) error {
	if profile == nil || *profile == "" {
		return nil
	}
	getter, ok := s.backend.(interface {
		GetResourceProfile(context.Context, string) (*haifypb.ResourceProfile, error)
	})
	if !ok {
		s.log.Debug("controllerServer.checkProfileExists: backend has no resource profiles")
		return status.Error(codes.Unimplemented, "this Haify backend does not support resource profiles")
	}
	start := time.Now()
	_, err := getter.GetResourceProfile(ctx, *profile)
	s.log.Debug("controllerServer.checkProfileExists: looked up profile", zap.String("profile", *profile),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "resource profile %q: %v", *profile, err)
	}
	return nil
}

// modifyProfile makes the volume a member of the profile, which applies the
// profile's DRBD options to it. An empty name takes it out of its profile.
// The profile's replica count is not applied here: set replicas for that.
func (s *controllerServer) modifyProfile(ctx context.Context, mod volumeModifier, r *haifypb.ResourceInfo, profile *string) error {
	if profile == nil {
		return nil
	}
	if r.GetProfile() == *profile {
		s.log.Debug("controllerServer.modifyProfile: already in place", zap.String("resource", r.GetName()),
			zap.String("profile", *profile))
		return nil
	}
	start := time.Now()
	err := mod.SetResourceProfile(ctx, r.GetName(), *profile)
	s.log.Debug("controllerServer.modifyProfile: set profile", zap.String("resource", r.GetName()),
		zap.String("from", r.GetProfile()), zap.String("to", *profile), zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return status.Errorf(codes.Internal, "set resource profile of %q to %q: %v", r.GetName(), *profile, err)
	}
	return nil
}

// modifyQoS records the new I/O limits as resource labels. The node plugins
// pick them up on their next pass (qos_reconcile.go); nothing on the nodes is
// touched from here.
func (s *controllerServer) modifyQoS(ctx context.Context, mod volumeModifier, r *haifypb.ResourceInfo, qos map[string]string) error {
	if len(qos) == 0 {
		return nil
	}
	set := map[string]string{}
	for param, v := range qos {
		key := qosLabelPrefix + param
		if r.GetLabels()[key] == v {
			continue
		}
		set[key] = v
	}
	if len(set) == 0 {
		s.log.Debug("controllerServer.modifyQoS: limits already in place", zap.String("resource", r.GetName()))
		return nil
	}
	start := time.Now()
	_, err := mod.SetResourceLabels(ctx, r.GetName(), set, nil)
	s.log.Debug("controllerServer.modifyQoS: set limit labels", zap.String("resource", r.GetName()), zap.Any("labels", set),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return status.Errorf(codes.Internal, "record I/O limits of %q: %v", r.GetName(), err)
	}
	return nil
}
