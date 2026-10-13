package wanproxy

import (
	"fmt"
	"strconv"
	"strings"

	"go.uber.org/zap"
)

// Tuning is what an operator can change about a WAN resource's proxies: how
// much RAM may queue writes the link has not taken yet, and whether, and how
// hard, the stream is compressed. The zero value is the defaults, so a record
// written before tuning existed renders exactly as it always did.
type Tuning struct {
	// BufferBytes is the proxy's bulk_cap_bytes: how much each DRBD connection
	// of a leg may queue in RAM before the proxy stops reading from DRBD and
	// DRBD falls back to Ahead (pull-ahead) and a bitmap resync. 0 is
	// DefaultBulkCapBytes.
	BufferBytes int64
	// ZstdLevel is 0 for the default level, CompressionOff to send the stream
	// uncompressed, or a zstd level from 1 to MaxZstdLevel.
	ZstdLevel int
}

const (
	// CompressionOff stores the stream uncompressed. A distinct value because 0
	// already means "the default" in a record.
	CompressionOff = -1

	// MaxZstdLevel is the highest level accepted. zstd goes to 22, but levels
	// past 19 need far more memory per frame for little gain on block data.
	MaxZstdLevel = 19

	// MinBufferBytes is the smallest buffer accepted. Below it a single DRBD
	// resync request batch no longer fits, and the buffer stops smoothing
	// anything.
	MinBufferBytes = 16 << 20

	// MaxBufferBytes is the largest buffer DRBD can use. DRBD goes Ahead once
	// congestion-fill is in flight, and accepts at most 10 GiB for it; RAM
	// past that would never fill.
	MaxBufferBytes = 10 << 30
)

// Effective returns the values the proxy config gets: the buffer in bytes, and
// the zstd level in the proxy's own terms, where 0 means off.
func (t Tuning) Effective() (bufferBytes int64, zstdLevel int) {
	bufferBytes = t.BufferBytes
	if bufferBytes <= 0 {
		bufferBytes = DefaultBulkCapBytes
	}
	switch {
	case t.ZstdLevel == CompressionOff:
		zstdLevel = 0
	case t.ZstdLevel <= 0:
		zstdLevel = DefaultZstdLevel
	default:
		zstdLevel = t.ZstdLevel
	}
	return bufferBytes, zstdLevel
}

// Validate rejects a buffer or level the proxy should not be given.
func (t Tuning) Validate() error {
	if t.BufferBytes != 0 && (t.BufferBytes < MinBufferBytes || t.BufferBytes > MaxBufferBytes) {
		zap.L().Debug("Tuning.Validate: buffer out of range", zap.Int64("buffer_bytes", t.BufferBytes))
		return fmt.Errorf("buffer must be between 16 MiB and 10 GiB")
	}
	if t.ZstdLevel < CompressionOff || t.ZstdLevel > MaxZstdLevel {
		zap.L().Debug("Tuning.Validate: zstd level out of range", zap.Int("zstd_level", t.ZstdLevel))
		return fmt.Errorf("zstd level must be between 1 and %d", MaxZstdLevel)
	}
	return nil
}

// ParseCompression reads an operator's compression setting into a ZstdLevel:
// "off", "default", "zstd" (the default level) or "zstd:N".
func ParseCompression(s string) (int, error) {
	v := strings.ToLower(strings.TrimSpace(s))
	switch v {
	case "off", "none":
		return CompressionOff, nil
	case "default", "zstd":
		return 0, nil
	}
	n, err := strconv.Atoi(strings.TrimPrefix(v, "zstd:"))
	if err != nil || !strings.HasPrefix(v, "zstd:") || n < 1 || n > MaxZstdLevel {
		zap.L().Debug("ParseCompression: not a compression setting", zap.String("value", s))
		return 0, fmt.Errorf("compression %q: use off, default or zstd:<1-%d>", s, MaxZstdLevel)
	}
	return n, nil
}

// CompressionString renders a ZstdLevel the way ParseCompression reads it.
func CompressionString(level int) string {
	switch {
	case level == CompressionOff:
		return "off"
	case level <= 0:
		return fmt.Sprintf("zstd:%d", DefaultZstdLevel)
	default:
		return fmt.Sprintf("zstd:%d", level)
	}
}
