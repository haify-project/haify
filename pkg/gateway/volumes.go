package gateway

import (
	"context"
	"fmt"
	"os"
	"strings"

	"go.uber.org/zap"
)

// Resolving a DRBD resource down to the device paths a promoter config names,
// and deciding which of those devices is the gateway's own scratch space and
// which one the operator actually asked to export.
//
// These two jobs are in one file because they fail identically: the gateway
// comes up, mounts something, exports something, and reports success — while
// serving the wrong block device. Neither an operator nor a health check can
// see the difference from the outside, so the reasoning behind both the device
// lookup (real device first, minor arithmetic only as a fallback) and the
// volume roles (identified by name, never by position — see
// clusterPrivateAndPayload) has to travel together with the code.

// getDRBDDevice gets the DRBD device path for a resource
func (m *Manager) getDRBDDevice(ctx context.Context, resource string) (string, error) {
	log := m.log()
	// Try to get device from resource info
	resInfo, err := m.resources.GetResource(ctx, resource)
	if err == nil && len(resInfo.Volumes) > 0 && resInfo.Volumes[0].Device != "" {
		log.Debug("getDRBDDevice: device from resource info",
			zap.String("resource", resource), zap.String("device", resInfo.Volumes[0].Device))
		return resInfo.Volumes[0].Device, nil
	}
	log.Debug("getDRBDDevice: resource info has no device, reading the local .res", zap.String("resource", resource), zap.Error(err))

	// Fallback: read from DRBD config file
	configPath := fmt.Sprintf("/etc/drbd.d/%s.res", resource)
	content, err := os.ReadFile(configPath)
	if err == nil {
		deviceMinor := parseDeviceMinorFromConfig(string(content))
		if deviceMinor >= 0 {
			log.Debug("getDRBDDevice: device from local .res", zap.String("path", configPath), zap.Int("minor", deviceMinor))
			return fmt.Sprintf("/dev/drbd%d", deviceMinor), nil
		}
	}

	// Final fallback
	log.Debug("getDRBDDevice: no device found, using /dev/drbd0", zap.String("path", configPath), zap.Error(err))
	return "/dev/drbd0", nil
}

// parseDeviceMinorFromConfig extracts device minor number from DRBD config content
func parseDeviceMinorFromConfig(configContent string) int {
	lines := strings.Split(configContent, "\n")
	inVolumeBlock := false
	var deviceMinor string

	for _, line := range lines {
		line = strings.TrimSpace(line)

		if strings.Contains(line, "volume") && strings.Contains(line, "{") {
			inVolumeBlock = true
			continue
		}

		if line == "}" {
			if inVolumeBlock && deviceMinor != "" {
				// Found it
				var minor int
				if _, err := fmt.Sscanf(deviceMinor, "%d", &minor); err != nil {
					zap.L().Debug("parseDeviceMinorFromConfig: minor not a number", zap.String("minor", deviceMinor))
					return -1
				}
				zap.L().Debug("parseDeviceMinorFromConfig: minor found", zap.Int("minor", minor))
				return minor
			}
			inVolumeBlock = false
			deviceMinor = ""
			continue
		}

		if inVolumeBlock && strings.Contains(line, "device") && strings.Contains(line, "minor") {
			parts := strings.Fields(line)
			for i, part := range parts {
				if part == "minor" && i+1 < len(parts) {
					deviceMinor = strings.TrimSuffix(parts[i+1], ";")
				}
			}
		}
	}

	zap.L().Debug("parseDeviceMinorFromConfig: no volume with a device minor", zap.Int("count", len(lines)))
	return -1
}

// volumeDevice returns the device path for a volume, preferring the real
// device reported by the resource manager. The minor-arithmetic fallback
// (base minor + volume number) only holds when minors happen to be
// consecutive, which global minor allocation does not guarantee.
func volumeDevice(volumes []*ResourceVolumeInfo, baseDevice string, volumeNumber int) string {
	for _, v := range volumes {
		if int(v.VolumeID) == volumeNumber && v.Device != "" {
			return v.Device
		}
	}
	zap.L().Debug("volumeDevice: volume not reported, deriving the device from the base minor",
		zap.Int("volume", volumeNumber), zap.String("base_device", baseDevice))
	return getDRBDDeviceForVolume(baseDevice, volumeNumber)
}

// getDRBDDeviceForVolume returns the DRBD device path for a specific volume number
// Volume 0 uses the base device, volume N uses base minor + N
func getDRBDDeviceForVolume(baseDevice string, volumeNumber int) string {
	if volumeNumber == 0 {
		return baseDevice
	}
	// Extract minor number from base device (e.g., /dev/drbd0 -> 0)
	minor := 0
	if strings.HasPrefix(baseDevice, "/dev/drbd") {
		if _, err := fmt.Sscanf(baseDevice, "/dev/drbd%d", &minor); err != nil {
			zap.L().Debug("getDRBDDeviceForVolume: base device has no minor, using it as is", zap.String("base_device", baseDevice))
			// If parsing fails, return the base device
			return baseDevice
		}
	}
	return fmt.Sprintf("/dev/drbd%d", minor+volumeNumber)
}

// stateVolumePrefix marks a volume created by EnsureGatewayVolumes for the
// gateway's own bookkeeping (NFS's rpc state, LIO's target state). It is
// "<resource>_state<N>"; the operator's own volume is "<resource>_data".
const stateVolumeSuffix = "_state"

// clusterPrivateAndPayload decides which of a resource's volumes holds gateway
// bookkeeping and which one is actually exported.
//
// This is not a free choice, and getting it backwards is silent. The gateway
// templates were written against linstor-gateway's convention — volume 0 is
// cluster-private, volume 1 is the payload — which holds when the gateway
// creates the resource itself and reserves volume 0. Haify does the opposite:
// `resource create --size` puts the operator's data on volume 0 as
// "<resource>_data", and the state volume is APPENDED afterwards by
// EnsureGatewayVolumes. Following the template's convention on a Haify resource
// therefore exports the 1 GiB scratch volume and formats the operator's data
// volume as gateway scratch — a share that comes up, mounts, and is both the
// wrong size and not their storage.
//
// The volume numbers cannot be swapped instead: DRBD records them in metadata,
// so renumbering an existing resource means destroying and resyncing it.
//
// The state volume is identified by name rather than by position, because
// position is exactly what was wrong. A resource with no "_state" volume at all
// was built by hand in the linstor layout, so the original convention is kept
// for it.
func clusterPrivateAndPayload(volumes []*ResourceVolumeInfo, fallbackDevice string) (clusterPrivate string, payload []*ResourceVolumeInfo) {
	var state *ResourceVolumeInfo
	for _, v := range volumes {
		if v == nil || v.Device == "" {
			continue
		}
		if state == nil && strings.Contains(v.BackingVolume, stateVolumeSuffix) {
			state = v
			continue
		}
		payload = append(payload, v)
	}
	log := zap.L()
	if state != nil && len(payload) > 0 {
		log.Debug("clusterPrivateAndPayload: state volume found by name",
			zap.String("state_device", state.Device), zap.String("backing_volume", state.BackingVolume), zap.Int("payload", len(payload)))
		return state.Device, payload
	}

	// No state volume to go on: this resource was built by hand in the linstor
	// layout, so keep that convention rather than guessing.
	payload = nil
	for _, v := range volumes {
		if v != nil && v.VolumeID != 0 && v.Device != "" {
			payload = append(payload, v)
		}
	}
	if len(payload) == 0 {
		if dev := volumeDevice(volumes, fallbackDevice, 1); dev != "" {
			payload = []*ResourceVolumeInfo{{VolumeID: 1, Device: dev}}
		}
	}
	log.Debug("clusterPrivateAndPayload: no state volume, using the linstor layout",
		zap.String("state_device", fallbackDevice), zap.Int("payload", len(payload)))
	return fallbackDevice, payload
}

// payloadDevice returns the device for the first exported volume, or the
// fallback when a resource somehow has none.
func payloadDevice(payload []*ResourceVolumeInfo, fallback string) string {
	if len(payload) > 0 && payload[0].Device != "" {
		return payload[0].Device
	}
	zap.L().Debug("payloadDevice: no exported volume, using the fallback", zap.String("device", fallback))
	return fallback
}
