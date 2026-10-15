package csi

import (
	"fmt"
	"strconv"
	"strings"

	"go.uber.org/zap"
)

// paramAllowRemoteVolumeAccess is the StorageClass parameter (LINSTOR-style)
// that opts a volume into diskless-client access. It is also carried forward in
// the volume's VolumeContext so the node service knows a Pod scheduled onto a
// non-replica node may attach the volume diskless instead of being rejected.
const paramAllowRemoteVolumeAccess = "allowRemoteVolumeAccess"

// The claim a volume is provisioned for, which external-provisioner adds to
// the parameters when it runs with --extra-create-metadata.
const (
	paramPVCName      = "csi.storage.k8s.io/pvc/name"
	paramPVCNamespace = "csi.storage.k8s.io/pvc/namespace"
)

// VolumeParams is the parsed StorageClass.parameters for CreateVolume.
type VolumeParams struct {
	Pool        string // VG (lvm) or zpool (zfs) name; required
	Replicas    int    // diskful copies; default 2
	StorageType string // "lvm" or "zfs"; default "lvm"
	// AllowRemoteVolumeAccess lets a Pod mount the volume from a node with no
	// local replica via a diskless client (I/O then flows over the DRBD
	// network). Default false: Pods are pinned to replica nodes for local I/O.
	AllowRemoteVolumeAccess bool
	ResourceProfile         string
	ResourceLabels          map[string]string
	// FaultDomainLabel is the node label naming what fails together (default
	// "host"); replicas are spread across its values when they can be.
	FaultDomainLabel string
	// QoS are the volume's I/O limits, carried to the node in the volume
	// context (qos.go).
	QoS map[string]string
	// PVC is the claim the volume is for, as namespace/name; empty when the
	// provisioner does not pass it.
	PVC string
	// Encryption puts a LUKS2 container beneath DRBD on every replica
	// (encryption at rest); ReplicationTLS switches the DRBD connections to
	// kernel TLS (encryption on the wire). See encryption.go.
	Encryption     bool
	ReplicationTLS bool
}

// ParseVolumeParams validates and defaults the StorageClass parameters.
func ParseVolumeParams(p map[string]string) (VolumeParams, error) {
	log := zap.L()
	// Only known keys are logged by name: a StorageClass can carry arbitrary
	// parameters, so the map itself is not.
	log.Debug("ParseVolumeParams: start", zap.Int("parameters", len(p)), zap.String("pool", p["pool"]),
		zap.String("profile", p["resourceProfile"]))
	out := VolumeParams{Pool: p["pool"], Replicas: 2, StorageType: "lvm", FaultDomainLabel: "host"}
	if v := strings.TrimSpace(p["faultDomainLabel"]); v != "" {
		log.Debug("ParseVolumeParams: fault domain label set", zap.String("fault_domain_label", v))
		out.FaultDomainLabel = v
	}
	out.ResourceProfile = strings.TrimSpace(p["resourceProfile"])
	if out.Pool == "" && out.ResourceProfile == "" {
		log.Debug("ParseVolumeParams: neither pool nor profile given")
		return out, fmt.Errorf("storageclass parameter \"pool\" is required")
	}
	if v, ok := p["replicas"]; ok {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			log.Debug("ParseVolumeParams: invalid replicas", zap.String("replicas", v))
			return out, fmt.Errorf("invalid replicas %q", v)
		}
		out.Replicas = n
	}
	if v, ok := p["storageType"]; ok {
		if v != "lvm" && v != "zfs" {
			log.Debug("ParseVolumeParams: invalid storage type", zap.String("storage_type", v))
			return out, fmt.Errorf("invalid storageType %q (want lvm or zfs)", v)
		}
		out.StorageType = v
	}
	if v, ok := p[paramAllowRemoteVolumeAccess]; ok {
		b, err := strconv.ParseBool(v)
		if err != nil {
			log.Debug("ParseVolumeParams: invalid remote access flag", zap.String("allow_remote", v))
			return out, fmt.Errorf("invalid %s %q (want true or false)", paramAllowRemoteVolumeAccess, v)
		}
		out.AllowRemoteVolumeAccess = b
	}
	if raw := strings.TrimSpace(p["resourceLabels"]); raw != "" {
		out.ResourceLabels = make(map[string]string)
		for _, item := range strings.Split(raw, ",") {
			key, value, ok := strings.Cut(strings.TrimSpace(item), "=")
			key = strings.TrimSpace(key)
			value = strings.TrimSpace(value)
			if !ok || key == "" || value == "" {
				log.Debug("ParseVolumeParams: invalid resource label entry", zap.String("entry", item))
				return out, fmt.Errorf("invalid resourceLabels entry %q (want key=value)", item)
			}
			out.ResourceLabels[key] = value
		}
	}
	if name, ns := p[paramPVCName], p[paramPVCNamespace]; name != "" && ns != "" {
		out.PVC = ns + "/" + name
	}
	encrypt, tls, err := parseSecurityParams(p)
	if err != nil {
		log.Debug("ParseVolumeParams: invalid encryption parameters", zap.Error(err))
		return out, err
	}
	out.Encryption, out.ReplicationTLS = encrypt, tls
	qos, err := parseQoS(p)
	if err != nil {
		log.Debug("ParseVolumeParams: invalid I/O limits", zap.Error(err))
		return out, err
	}
	out.QoS = qos
	log.Debug("ParseVolumeParams: parsed", zap.String("pool", out.Pool), zap.Int("replicas", out.Replicas),
		zap.String("storage_type", out.StorageType), zap.Bool("allow_remote", out.AllowRemoteVolumeAccess),
		zap.Int("labels", len(out.ResourceLabels)), zap.Int("qos", len(out.QoS)), zap.String("pvc", out.PVC),
		zap.Bool("encryption", out.Encryption), zap.Bool("replication_tls", out.ReplicationTLS))
	return out, nil
}
