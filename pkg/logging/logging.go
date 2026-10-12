// Package logging builds every binary's zap logger the same way and installs
// it as the process-wide logger, so a package that is not handed a logger
// still reaches the binary's one through zap.L().
//
// Tracing is logged at Debug: it costs a level check and nothing more until
// the level is lowered to debug ([log] level in controller.toml for the
// controller, HAIFY_LOG_LEVEL or --debug elsewhere).
package logging

import (
	"os"
	"strings"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// EnvLevel sets the level of the binaries configured by environment rather
// than a file: haify, csi-controller, csi-node and haify-docker. Values:
// debug, info, warn, error.
const EnvLevel = "HAIFY_LOG_LEVEL"

// ParseLevel reads debug, info, warn or error, and returns fallback for
// anything else, the empty string included.
func ParseLevel(name string, fallback zapcore.Level) zapcore.Level {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "debug":
		return zapcore.DebugLevel
	case "info":
		return zapcore.InfoLevel
	case "warn", "warning":
		return zapcore.WarnLevel
	case "error":
		return zapcore.ErrorLevel
	}
	return fallback
}

// Install makes l the process-wide logger and returns it.
func Install(l *zap.Logger) *zap.Logger {
	zap.ReplaceGlobals(l)
	return l
}

// Production returns a JSON logger at the level from HAIFY_LOG_LEVEL (info
// when unset) and installs it.
func Production() *zap.Logger {
	cfg := zap.NewProductionConfig()
	cfg.Level = zap.NewAtomicLevelAt(ParseLevel(os.Getenv(EnvLevel), zapcore.InfoLevel))
	l, err := cfg.Build()
	if err != nil {
		l = zap.NewNop()
	}
	return Install(l)
}

// Console returns a human-readable logger on stderr at level and installs it.
func Console(level zapcore.Level) *zap.Logger {
	cfg := zap.NewDevelopmentConfig()
	cfg.Level = zap.NewAtomicLevelAt(level)
	cfg.OutputPaths = []string{"stderr"}
	cfg.ErrorOutputPaths = []string{"stderr"}
	l, err := cfg.Build()
	if err != nil {
		l = zap.NewNop()
	}
	return Install(l)
}
