package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/haify-project/haify/pkg/config"
	"github.com/haify-project/haify/pkg/controller"
	"github.com/haify-project/haify/pkg/logbuf"
	"github.com/haify-project/haify/pkg/logging"
)

var (
	version = "dev"
)

func main() {
	configPath := flag.String("config", "", "Path to configuration file")
	flag.Parse()

	// Load configuration
	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load config: %v\n", err)
		os.Exit(1)
	}

	// The log ring must exist before the logger, which tees into it.
	logRing := logbuf.New(logbuf.DefaultSize)

	// Initialize logger
	logger, err := initLogger(cfg, logRing)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to initialize logger: %v\n", err)
		os.Exit(1)
	}
	defer func() { _ = logger.Sync() }()

	logger.Info("Starting Haify controller",
		zap.String("version", version),
		zap.String("config", *configPath),
		zap.Bool("metrics_enabled", cfg.Metrics.Enabled),
		zap.String("metrics_address", fmt.Sprintf("%s:%d", cfg.Metrics.ListenAddress, cfg.Metrics.Port)))

	// Create controller
	ctrl, err := controller.New(cfg, logger)
	if err != nil {
		logger.Fatal("Failed to create controller", zap.Error(err))
	}
	ctrl.SetLogRing(logRing)

	// Start controller
	if err := ctrl.Start(); err != nil {
		logger.Fatal("Failed to start controller", zap.Error(err))
	}

	// Wait for interrupt signal
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan

	logger.Info("Shutting down...")
	ctrl.Stop()
	logger.Info("Shutdown complete")
}

// initLogger initializes the logger
func initLogger(cfg *config.Config, ring *logbuf.Ring) (*zap.Logger, error) {
	var zapConfig zap.Config

	if cfg.Log.Format == "json" {
		zapConfig = zap.NewProductionConfig()
	} else {
		zapConfig = zap.NewDevelopmentConfig()
	}

	zapConfig.Level = zap.NewAtomicLevelAt(logging.ParseLevel(cfg.Log.Level, zapcore.InfoLevel))

	// Tee into the ring so the API can serve recent lines, without changing
	// where the log actually goes: stderr, and thus the journal, stays the
	// system of record.
	logger, err := zapConfig.Build(zap.WrapCore(func(c zapcore.Core) zapcore.Core {
		return zapcore.NewTee(c, ring.Core(zapConfig.Level))
	}))
	if err != nil {
		return nil, err
	}
	return logging.Install(logger), nil
}
