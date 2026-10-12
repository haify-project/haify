package gateway

import (
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	"go.uber.org/zap"
)

// Identifiers that get baked into a promoter config and then have to stay put.
//
// The values below are not cosmetic. A LUN serial, an NVMe namespace UUID and
// an NFS FSID are what an already-connected client uses to recognise the
// storage it is talking to across a failover. If one of them is regenerated —
// because it was derived from something node-local, or from the clock — the
// client sees the device it had open replaced by a different one: an initiator
// drops the LUN, an NFS mount goes stale rather than recovering. That is why
// every generator here is either a pure function of inputs that survive
// failover (the IQN, the resource and volume UUIDs) or, for generateUUID, a
// value that is generated once at create time and then only ever read back out
// of the config file.
//
// They are collected in one file so that the next identifier a gateway needs is
// written next to this reasoning instead of next to the template that happens
// to interpolate it.

// generateUUID generates a proper UUID v4 for use in configurations
func generateUUID() string {
	// Generate 16 random bytes
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		zap.L().Debug("generateUUID: crypto/rand failed, deriving from the clock", zap.Error(err))
		// Fallback to MD5-based UUID if rand.Read fails
		hash := md5.Sum([]byte(fmt.Sprintf("%d", time.Now().UnixNano())))
		return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
			hash[:4], hash[4:6], hash[6:8], hash[8:10], hash[10:16])
	}

	// Set version 4 (random UUID) and variant
	b[6] = (b[6] & 0x0F) | 0x40
	b[8] = (b[8] & 0x3F) | 0x80

	return fmt.Sprintf("%s-%s-%s-%s-%s",
		hex.EncodeToString(b[0:4]),
		hex.EncodeToString(b[4:6]),
		hex.EncodeToString(b[6:8]),
		hex.EncodeToString(b[8:10]),
		hex.EncodeToString(b[10:16]))
}

// generateSerialFromIQN generates a unique serial from IQN and volume number
// Matches linstor-gateway behavior for iSCSI LUNs
func generateSerialFromIQN(iqn string, volumeNumber int) string {
	hash := md5.Sum([]byte(fmt.Sprintf("%s-%d", iqn, volumeNumber)))
	return hex.EncodeToString(hash[:8])
}

// generateFSID generates a proper filesystem ID as UUID (matches linstor-gateway)
func generateFSID(resourceUUID, volumeUUID string) string {
	// SHA1 hash of resource UUID + volume UUID to create unique FSID
	hash := md5.Sum([]byte(resourceUUID + ":" + volumeUUID))
	uuid := fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		hash[:4], hash[4:6], hash[6:8], hash[8:10], hash[10:16])
	return uuid
}
