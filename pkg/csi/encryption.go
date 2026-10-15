package csi

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	haifypb "github.com/haify-project/haify/api/proto/v1"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Encrypted volumes, per StorageClass.
//
// Two independent switches, because they protect against different things and
// Haify implements them at different layers:
//
//   - encryption: "true" creates the resource with a LUKS2 container beneath
//     DRBD on every replica (CreateResourceRequest.encrypt). The pool disks hold
//     ciphertext. DRBD sits ABOVE the crypt layer, so this protects a disk that
//     leaves the building and nothing on the wire.
//   - replicationTLS: "true" switches the new resource's DRBD connections to
//     kernel TLS once it exists (SetResourceTLS), so replication traffic is
//     encrypted between nodes. It says nothing about the disks.
//
// Keys: every node makes its own LUKS key and keeps it under /etc/haify/luks;
// the controller never has it and neither does Kubernetes. There is
// deliberately no Secret-based scheme here: the only channel from the
// controller to a node is the SSH session behind deployment.Exec, which puts
// the command line in the remote argv and logs it at Debug, so a passphrase
// shipped from a Secret would be readable on the node and in the controller's
// log. Not having a secret to transport is what keeps it safe.

// The StorageClass parameters, also carried into the volume's context so
// `kubectl get pv -o yaml` shows what the volume was made with.
const (
	paramEncryption     = "encryption"
	paramReplicationTLS = "replicationTLS"
)

// parseSecurityParams reads the encryption and replicationTLS parameters.
func parseSecurityParams(p map[string]string) (encrypt, tls bool, err error) {
	log := zap.L()
	parse := func(key string) (bool, error) {
		v, ok := p[key]
		if !ok || strings.TrimSpace(v) == "" {
			return false, nil
		}
		b, perr := strconv.ParseBool(strings.TrimSpace(v))
		if perr != nil {
			log.Debug("parseSecurityParams: invalid flag", zap.String("parameter", key), zap.String("value", v))
			return false, fmt.Errorf("invalid %s %q (want true or false)", key, v)
		}
		return b, nil
	}
	if encrypt, err = parse(paramEncryption); err != nil {
		return false, false, err
	}
	if tls, err = parse(paramReplicationTLS); err != nil {
		return false, false, err
	}
	log.Debug("parseSecurityParams: parsed", zap.Bool("encryption", encrypt), zap.Bool("replication_tls", tls))
	return encrypt, tls, nil
}

// checkEncryptable refuses encryption where the controller would refuse it,
// so the reason reaches the PVC's events before anything is created. The
// crypt layer is LVM only (ZFS has its own encryption at the dataset level),
// and a VDO pool would be handed ciphertext it can neither deduplicate nor
// compress.
func checkEncryptable(params VolumeParams, pools []*haifypb.PoolInfo) error {
	log := zap.L()
	log.Debug("checkEncryptable: start", zap.Bool("encryption", params.Encryption), zap.String("pool", params.Pool),
		zap.String("storage_type", params.StorageType), zap.Int("pools", len(pools)))
	if !params.Encryption {
		return nil
	}
	if params.StorageType == "zfs" {
		log.Debug("checkEncryptable: refused, zfs storage type")
		return fmt.Errorf("encryption is supported on LVM pools only, not storageType zfs; ZFS provides its own encryption at the dataset level")
	}
	want := managedPoolName(params.Pool)
	for _, p := range pools {
		if p.GetName() != want {
			continue
		}
		if p.GetType() == "zfs" {
			log.Debug("checkEncryptable: refused, zfs pool", zap.String("pool", want), zap.String("node", p.GetNode()))
			return fmt.Errorf("encryption is supported on LVM pools only, and pool %q is a ZFS pool on %s", params.Pool, p.GetNode())
		}
		if p.GetHasVdo() {
			log.Debug("checkEncryptable: refused, vdo pool", zap.String("pool", want), zap.String("node", p.GetNode()))
			return fmt.Errorf("pool %q is VDO-backed on %s: encryption above VDO defeats its deduplication and compression; use a plain LVM pool for encrypted volumes",
				params.Pool, p.GetNode())
		}
	}
	log.Debug("checkEncryptable: encryptable", zap.String("pool", want))
	return nil
}

// checkSourceEncryption refuses to fill a new volume from an encrypted source
// unless the new volume is encrypted as well: the copy would otherwise sit in
// plaintext on the new volume's disks. A plaintext source may be copied into
// an encrypted volume; it is encrypted on the way in.
func checkSourceEncryption(src *haifypb.ResourceInfo, encrypt bool) error {
	zap.L().Debug("checkSourceEncryption: comparing", zap.String("resource", src.GetName()),
		zap.Bool("source_encrypted", src.GetEncrypted()), zap.Bool("target_encrypted", encrypt))
	if !src.GetEncrypted() || encrypt {
		return nil
	}
	return status.Errorf(codes.InvalidArgument,
		"source volume %q is encrypted, so a copy of it has to be encrypted too: use a StorageClass with %s: \"true\"",
		src.GetName(), paramEncryption)
}

// securityContext adds the encryption flags to a volume context, so they show
// in the PV's volumeAttributes. Absent flags are left out, matching how the
// other parameters are carried.
func securityContext(vc map[string]string, encrypt, tls bool) map[string]string {
	zap.L().Debug("securityContext: adding flags", zap.Bool("encryption", encrypt), zap.Bool("replication_tls", tls))
	if !encrypt && !tls {
		return vc
	}
	if vc == nil {
		vc = map[string]string{}
	}
	if encrypt {
		vc[paramEncryption] = "true"
	}
	if tls {
		vc[paramReplicationTLS] = "true"
	}
	return vc
}

// replicationTLSBackend is what replicationTLS needs from the controller.
// *client.HaifyClient has both calls; a backend without them cannot honour the
// parameter, and says so rather than creating a volume that replicates in
// the clear.
type replicationTLSBackend interface {
	ReplicationTLSStatus(ctx context.Context, nodes []string) ([]*haifypb.NodeTLSInfo, error)
	SetResourceTLS(ctx context.Context, resource string, enabled bool) (string, error)
}

func (s *controllerServer) tlsBackend() (replicationTLSBackend, error) {
	b, ok := s.backend.(replicationTLSBackend)
	s.log.Debug("controllerServer.tlsBackend: checked", zap.Bool("supported", ok))
	if !ok {
		return nil, status.Errorf(codes.Internal, "Haify backend does not support %s", paramReplicationTLS)
	}
	return b, nil
}

// tlsSetupHint is the remedy every not-ready message ends with.
const tlsSetupHint = "prepare the nodes with `haify replication-tls setup` (see `haify replication-tls status`)"

// tlsReadyCandidates keeps the placement candidates that can carry an
// encrypted DRBD connection, so a volume asking for replicationTLS lands where
// the switch will succeed.
//
// A pinned node that is not ready is an error rather than silently passed
// over: under WaitForFirstConsumer it is the node the Pod is going to, and for
// a clone it is the node holding the source data. ResourceExhausted makes
// external-provisioner let the scheduler try another node. Too few ready nodes
// overall is FailedPrecondition: nothing changes until an operator prepares
// them, which the message says how to do.
func (s *controllerServer) tlsReadyCandidates(ctx context.Context, candidates []replicaCandidate, pinned []string, replicas int) ([]replicaCandidate, error) {
	names := make([]string, 0, len(candidates))
	for _, c := range candidates {
		names = append(names, c.node)
	}
	s.log.Debug("controllerServer.tlsReadyCandidates: start", zap.Strings("candidates", names), zap.Strings("pinned", pinned),
		zap.Int("replicas", replicas))
	backend, err := s.tlsBackend()
	if err != nil {
		return nil, err
	}
	if len(names) == 0 {
		s.log.Debug("controllerServer.tlsReadyCandidates: no candidates, placement decides")
		return candidates, nil
	}
	start := time.Now()
	states, err := backend.ReplicationTLSStatus(ctx, names)
	s.log.Debug("controllerServer.tlsReadyCandidates: read replication TLS status", zap.Int("nodes", len(states)),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "%s: read replication TLS status: %v", paramReplicationTLS, err)
	}
	problems := map[string]string{}
	ready := map[string]bool{}
	for _, st := range states {
		s.log.Debug("controllerServer.tlsReadyCandidates: node", zap.String("node", st.GetNode()), zap.Bool("ready", st.GetReady()),
			zap.String("problem", st.GetProblem()))
		if st.GetReady() {
			ready[st.GetNode()] = true
		} else {
			problems[st.GetNode()] = orUnknownProblem(st.GetProblem())
		}
	}
	for _, n := range pinned {
		if !ready[n] && containsNode(names, n) {
			s.log.Debug("controllerServer.tlsReadyCandidates: pinned node not ready", zap.String("node", n))
			return nil, status.Errorf(codes.ResourceExhausted,
				"%s: node %q must hold a replica but cannot carry an encrypted replication link (%s); %s",
				paramReplicationTLS, n, orUnknownProblem(problems[n]), tlsSetupHint)
		}
	}
	var out []replicaCandidate
	var notReady []string
	for _, c := range candidates {
		if ready[c.node] {
			out = append(out, c)
			continue
		}
		notReady = append(notReady, fmt.Sprintf("%s (%s)", c.node, orUnknownProblem(problems[c.node])))
	}
	s.log.Debug("controllerServer.tlsReadyCandidates: filtered", zap.Int("ready", len(out)), zap.Strings("not_ready", notReady))
	if len(out) < replicas {
		return nil, status.Errorf(codes.FailedPrecondition,
			"%s: %d replica(s) requested but only %d node(s) with the pool can carry an encrypted replication link; not ready: %s; %s",
			paramReplicationTLS, replicas, len(out), strings.Join(notReady, "; "), tlsSetupHint)
	}
	return out, nil
}

// orUnknownProblem keeps a not-ready message readable when the controller did
// not say why.
func orUnknownProblem(p string) string {
	if strings.TrimSpace(p) == "" {
		return "not ready"
	}
	return p
}

// enableReplicationTLS switches every DRBD connection of resource to TLS. The
// controller checks each member, the automatic quorum tiebreaker included,
// before it touches a link, and cycles the links one at a time.
func (s *controllerServer) enableReplicationTLS(ctx context.Context, resource string) error {
	s.log.Debug("controllerServer.enableReplicationTLS: start", zap.String("resource", resource))
	backend, err := s.tlsBackend()
	if err != nil {
		return err
	}
	start := time.Now()
	msg, err := backend.SetResourceTLS(ctx, resource, true)
	s.log.Debug("controllerServer.enableReplicationTLS: set resource TLS returned", zap.String("resource", resource),
		zap.String("message", msg), zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return fmt.Errorf("%s: switch %q to encrypted replication: %v; %s", paramReplicationTLS, resource, err, tlsSetupHint)
	}
	return nil
}

// reconcileExisting checks a volume that already exists under the requested
// name against the encryption the StorageClass asks for, and finishes the
// replication TLS switch if an earlier attempt stopped before it. It returns
// the volume context to report.
//
// A retry lands here when a previous CreateVolume created the resource but did
// not return: the switch to TLS may never have happened, and returning the
// volume as it is would hand out one that replicates in the clear.
func (s *controllerServer) reconcileExisting(ctx context.Context, existing *haifypb.ResourceInfo, p map[string]string) (map[string]string, error) {
	s.log.Debug("controllerServer.reconcileExisting: start", zap.String("resource", existing.GetName()),
		zap.Bool("encrypted", existing.GetEncrypted()))
	params, err := ParseVolumeParams(p)
	if err != nil {
		s.log.Debug("controllerServer.reconcileExisting: invalid parameters", zap.String("resource", existing.GetName()), zap.Error(err))
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if params.Encryption && !existing.GetEncrypted() {
		s.log.Debug("controllerServer.reconcileExisting: exists unencrypted", zap.String("resource", existing.GetName()))
		return nil, status.Errorf(codes.AlreadyExists,
			"volume %q already exists unencrypted, and the StorageClass asks for %s; encryption cannot be added to an existing volume",
			existing.GetName(), paramEncryption)
	}
	if params.ReplicationTLS {
		if err := s.enableReplicationTLS(ctx, existing.GetName()); err != nil {
			s.log.Debug("controllerServer.reconcileExisting: TLS switch failed", zap.String("resource", existing.GetName()), zap.Error(err))
			return nil, status.Error(codes.Internal, err.Error())
		}
	}
	vc := securityContext(volumeContextFor(params.AllowRemoteVolumeAccess, params.QoS), existing.GetEncrypted(), params.ReplicationTLS)
	s.log.Debug("controllerServer.reconcileExisting: done", zap.String("resource", existing.GetName()), zap.Int("context", len(vc)))
	return vc, nil
}
