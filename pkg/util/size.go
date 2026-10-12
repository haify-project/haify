package util

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"go.uber.org/zap"
)

var (
	sizeRegex = regexp.MustCompile(`(?i)^(\d+(?:\.\d+)?)\s*([KMGTPE]?(?:i)?B?)$`)
)

// ParseSize parses a size string with various units (B, KB, KiB, MB, MiB, etc.)
// Returns size in bytes
// Supported units:
//   - B: bytes
//   - KB, KiB: kilobytes (1000 or 1024)
//   - MB, MiB: megabytes (1000^2 or 1024^2)
//   - GB, GiB: gigabytes (1000^3 or 1024^3)
//   - TB, TiB: terabytes (1000^4 or 1024^4)
//   - PB, PiB: petabytes (1000^5 or 1024^5)
//   - EB, EiB: exabytes (1000^6 or 1024^6)
func ParseSize(sizeStr string) (uint64, error) {
	sizeStr = strings.TrimSpace(sizeStr)

	// Handle pure number as bytes
	if sizeStr == "" {
		zap.L().Debug("ParseSize: empty size string")
		return 0, errors.New("size string is empty")
	}

	// Check if it's just a number (bytes)
	if num, err := strconv.ParseUint(sizeStr, 10, 64); err == nil {
		return num, nil
	}

	// Use regex to parse number and unit
	matches := sizeRegex.FindStringSubmatch(sizeStr)
	if matches == nil {
		zap.L().Debug("ParseSize: not a number with a unit", zap.String("size", sizeStr))
		return 0, fmt.Errorf("invalid size format: %s", sizeStr)
	}

	value, err := strconv.ParseFloat(matches[1], 64)
	if err != nil {
		zap.L().Debug("ParseSize: number does not parse", zap.String("size", sizeStr), zap.String("number", matches[1]), zap.Error(err))
		return 0, fmt.Errorf("invalid number: %s", matches[1])
	}

	unit := strings.ToUpper(matches[2])

	// Determine multiplier based on unit
	var multiplier float64
	switch unit {
	case "B":
		multiplier = 1
	case "KB":
		multiplier = 1000
	case "K", "KIB":
		multiplier = 1024
	case "MB":
		multiplier = 1000 * 1000
	case "M", "MIB":
		multiplier = 1024 * 1024
	case "GB":
		multiplier = 1000 * 1000 * 1000
	case "G", "GIB":
		multiplier = 1024 * 1024 * 1024
	case "TB":
		multiplier = 1000 * 1000 * 1000 * 1000
	case "T", "TIB":
		multiplier = 1024 * 1024 * 1024 * 1024
	case "PB":
		multiplier = 1000 * 1000 * 1000 * 1000 * 1000
	case "P", "PIB":
		multiplier = 1024 * 1024 * 1024 * 1024 * 1024
	case "EB":
		multiplier = 1000 * 1000 * 1000 * 1000 * 1000 * 1000
	case "E", "EIB":
		multiplier = 1024 * 1024 * 1024 * 1024 * 1024 * 1024
	default:
		zap.L().Debug("ParseSize: unknown unit", zap.String("size", sizeStr), zap.String("unit", matches[2]))
		return 0, fmt.Errorf("unknown unit: %s", matches[2])
	}

	bytes := uint64(value * multiplier)
	if float64(bytes) != value*multiplier {
		zap.L().Debug("ParseSize: size overflows or is fractional bytes", zap.String("size", sizeStr),
			zap.Float64("value", value), zap.Float64("multiplier", multiplier))
		return 0, fmt.Errorf("size overflow: %s", sizeStr)
	}

	return bytes, nil
}

// BytesToGB converts bytes to GB (decimal)
func BytesToGB(bytes uint64) uint64 {
	return bytes / (1000 * 1000 * 1000)
}

// BytesToGiB converts bytes to GiB (binary)
func BytesToGiB(bytes uint64) uint64 {
	return bytes / (1024 * 1024 * 1024)
}

// FormatBytes formats bytes to human-readable string (using binary units)
func FormatBytes(bytes uint64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}

	div, exp := uint64(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}

	units := []string{"B", "KiB", "MiB", "GiB", "TiB", "PiB", "EiB"}
	if exp >= len(units) {
		exp = len(units) - 1
	}

	return fmt.Sprintf("%.2f %s", float64(bytes)/float64(div), units[exp+1])
}

// SizeToGBString converts size string to GB value
// Returns the GB value as an integer
func SizeToGB(sizeStr string) (uint64, error) {
	bytes, err := ParseSize(sizeStr)
	if err != nil {
		return 0, err
	}
	return BytesToGB(bytes), nil
}

// SizeToGiBString converts size string to GiB value
// Returns the GiB value as an integer
func SizeToGiBString(sizeStr string) (uint64, error) {
	bytes, err := ParseSize(sizeStr)
	if err != nil {
		return 0, err
	}
	return BytesToGiB(bytes), nil
}
