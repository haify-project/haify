// Package controller provides the Haify controller
package controller

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/haify-project/haify/pkg/alert"
	"go.uber.org/zap"
	"google.golang.org/grpc"

	"github.com/haify-project/haify/pkg/config"
	"github.com/haify-project/haify/pkg/database"
	"github.com/haify-project/haify/pkg/deployment"
	"github.com/haify-project/haify/pkg/event"
	"github.com/haify-project/haify/pkg/gateway"
	"github.com/haify-project/haify/pkg/logbuf"
	"github.com/haify-project/haify/pkg/metrics"
	"github.com/haify-project/haify/pkg/rbac"
	"github.com/haify-project/haify/pkg/wanproxy"
)

// Controller represents the Haify controller
type Controller struct {
	config     *config.Config
	logger     *zap.Logger
	db         *database.DB
	deployment deploymentClient
	hosts      []string
	hostsMap   map[string]string // hostname -> address mapping
	hostsLock  sync.RWMutex
	server     *grpc.Server
	restServer *http.Server
	ctx        context.Context
	cancel     context.CancelFunc
	// Metrics
	metrics       *metrics.Metrics
	metricsServer *http.Server
	// UI
	uiServer *UIServer
	// restLoopbackTLS is how the UI's proxy dials the REST gateway when it is
	// served over TLS ([tls] rest); nil for plain HTTP.
	restLoopbackTLS *tls.Config
	// events carries operational notifications (degrade, failover, node loss)
	// from the health detector to Webhook receivers and watch streams. Nil when
	// notifications are disabled, which is what the API surfaces report on.
	events       *event.Bus
	alertMonitor *alert.Monitor
	// logRing holds the controller's recent log output for the API to serve.
	// Nil when the process was started without one, in which case the log view
	// reports that rather than showing an empty buffer.
	logRing *logbuf.Ring
	// rbac is the authorization engine, nil when [rbac] is off. Built when the
	// gRPC server starts; the RBAC RPCs and REST routes both read it.
	rbac *rbac.Engine
	// approvals is the two-person approval gate; nil unless [rbac.approval].
	approvals *approvalGate
	// Managers
	storage   *StorageManager
	resources *ResourceManager
	snapshots *SnapshotManager
	nodes     *NodeManager
	gateway   *gateway.Manager
	schedules *ScheduleManager
	backups   *BackupManager
	notify    *NotifyManager
	// inspections runs the scheduled and on-demand cluster inspection.
	inspections *InspectionManager
	// writeHistory keeps each resource's write rates for WAN sizing
	// (write_history.go); fed by the health poll.
	writeHistory *writeHistory
	// view is the cluster as the last health poll saw it, and history its
	// totals over time (cluster_view.go, cluster_history.go); both feed the
	// dashboard and the resource list.
	view    *clusterView
	history *clusterHistory
}

// SetLogRing attaches the buffer the log view reads from. The ring has to exist
// before the logger that tees into it, which happens in main, so it is handed
// over here rather than built by New.
func (c *Controller) SetLogRing(r *logbuf.Ring) { c.logRing = r }

// traceLogger is the logger for Debug traces: the controller's own, or the
// global one for a Controller (or a nil one) built without a logger, as tests
// and bare managers do, so a trace never turns into a nil dereference.
func (c *Controller) traceLogger() *zap.Logger {
	if c == nil || c.logger == nil {
		return zap.L()
	}
	return c.logger
}

// closeDBAfterFailedStart hands the metadata database back when New gives up
// part way through.
//
// It matters because bolt holds an exclusive flock on the file: a controller
// that aborts with the database still open leaves the next start attempt to
// fail at "timeout" on a file nothing is really using — a far more confusing
// error than the one New is about to return. The close error itself is logged
// rather than returned, because the caller needs to see why the controller
// could not be built, not how the cleanup went.
func closeDBAfterFailedStart(logger *zap.Logger, db *database.DB) {
	if db == nil {
		zap.L().Debug("closeDBAfterFailedStart: no database to close")
		return
	}
	if logger == nil {
		logger = zap.NewNop()
	}
	logger.Debug("closeDBAfterFailedStart: closing database after aborted start")
	if err := db.Close(); err != nil {
		logger.Warn("Failed to close database after an aborted controller start", zap.Error(err))
	}
}

// New creates a new controller
func New(cfg *config.Config, logger *zap.Logger) (*Controller, error) {
	ctx, cancel := context.WithCancel(context.Background())
	logger.Debug("New: building controller",
		zap.String("db_path", cfg.Database.Path),
		zap.String("dispatch_config", cfg.Dispatch.ConfigPath),
		zap.Int("dispatch_parallel", cfg.Dispatch.Parallel),
		zap.Bool("metrics_enabled", cfg.Metrics.Enabled))

	// Open database
	start := time.Now()
	db, err := database.Open(&database.Config{
		Path:           cfg.Database.Path,
		AuditRetention: cfg.Audit.MaxEntries,
		AuditMaxAge:    time.Duration(cfg.Audit.RetentionDays) * 24 * time.Hour,
	}, logger)
	logger.Debug("New: opened database", zap.String("path", cfg.Database.Path),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		// A schema this binary cannot safely write is the one open failure that
		// must not degrade into "continue without persistence". Running on with
		// db == nil means the controller reports an empty cluster and then
		// starts writing that view back over a database whose real content it
		// never understood — which is precisely the data loss the version check
		// exists to prevent. Refuse to start and let the operator roll forward
		// or restore.
		if database.IsSchemaIncompatible(err) {
			logger.Debug("New: database schema incompatible, refusing to start", zap.Error(err))
			cancel()
			return nil, fmt.Errorf("cannot open database: %w", err)
		}
		logger.Warn("Failed to open database, continuing without persistence", zap.Error(err))
		db = nil
	}

	// WAN mTLS material lives next to the rest of the controller's state.
	// Without this the PKI cache is pinned to /var/lib/haify, so a non-root
	// controller fails resource creation at "mkdir /var/lib/haify: permission
	// denied" — after it has already created the backing volumes.
	if cfg.WAN.PKIDir != "" {
		logger.Debug("New: using configured WAN PKI directory", zap.String("pki_dir", cfg.WAN.PKIDir))
		wanproxy.PKIDir = cfg.WAN.PKIDir
	}

	// Create deployment client. The dispatch config path comes from
	// controller.toml so a controller running under a different HOME (systemd,
	// non-root operator) still finds the operator's SSH settings.
	deploymentClient, err := deployment.NewWithOptions(logger, deployment.Options{
		ConfigPath: cfg.Dispatch.ConfigPath,
		Parallel:   cfg.Dispatch.Parallel,
	})
	if err != nil {
		logger.Debug("New: deployment client creation failed", zap.Error(err))
		cancel()
		closeDBAfterFailedStart(logger, db)
		return nil, fmt.Errorf("failed to create deployment client: %w", err)
	}

	ctrl := &Controller{
		config:   cfg,
		logger:   logger,
		db:       db,
		hosts:    []string{},
		hostsMap: make(map[string]string),
		ctx:      ctx,
		cancel:   cancel,
	}
	// Every DRBD config written or read goes through the replication-paths
	// layer (replication_paths.go).
	// Commands for local-agent nodes are dropped before they reach SSH
	// (local_agent_deployment.go).
	ctrl.deployment = &localAgentDeployment{
		deploymentClient: &resPathsDeployment{deploymentClient: deploymentClient, extra: func() map[string][]string {
			if ctrl.nodes == nil {
				return nil
			}
			return ctrl.nodes.replicationPathsByDRBDName()
		}},
		isAgent: func(host string) bool {
			return ctrl.nodes != nil && ctrl.nodes.isLocalAgentHost(host)
		},
	}

	// Initialize managers
	ctrl.storage = NewStorageManager(ctrl)
	ctrl.resources = NewResourceManager(ctrl)
	ctrl.snapshots = NewSnapshotManager(ctrl)
	ctrl.nodes = NewNodeManager(ctrl)
	ctrl.schedules = NewScheduleManager(ctrl)
	ctrl.backups = NewBackupManager(ctrl)
	ctrl.inspections = NewInspectionManager(ctrl)

	// Initialize gateway with adapters
	gwResourceManager := NewGatewayResourceManager(ctrl.resources,
		cfg.Gateway.AutoStateVolume, cfg.Gateway.StateVolumeSizeGB)
	gwDeploymentClient := NewGatewayDeploymentClient(deploymentClient)
	ctrl.gateway = gateway.New(gwResourceManager, gwDeploymentClient, logger, []string{})

	// Initialize metrics
	if cfg.Metrics.Enabled {
		metricsInstance, err := metrics.New(logger)
		if err != nil {
			logger.Debug("New: metrics initialization failed", zap.Error(err))
			cancel()
			closeDBAfterFailedStart(logger, db)
			return nil, fmt.Errorf("failed to initialize metrics: %w", err)
		}
		ctrl.metrics = metricsInstance
	}

	// Initialize hosts mapping
	ctrl.initHostsMapping()

	// Load data from database
	if db != nil {
		if err := ctrl.loadFromDatabase(ctx); err != nil {
			logger.Warn("Failed to load data from database", zap.Error(err))
		}
	} else {
		logger.Debug("New: no database, skipping load")
	}

	logger.Debug("New: controller built", zap.Bool("has_db", db != nil), zap.Bool("has_metrics", ctrl.metrics != nil))
	return ctrl, nil
}

// Start starts the controller
func (c *Controller) Start() error {
	c.logger.Info("Starting Haify controller")
	log := c.traceLogger()

	// Load hosts from registered nodes in database
	if c.db != nil {
		if err := c.loadHostsFromDatabase(context.Background()); err != nil {
			c.logger.Warn("Failed to load hosts from database", zap.Error(err))
		}
	} else {
		log.Debug("Controller.Start: no database, hosts not loaded")
	}
	log.Debug("Controller.Start: handing hosts to managers", zap.Strings("hosts", c.hosts))

	// Initialize deployment client with hosts
	c.resources.SetDeployment(c.deployment)
	c.resources.SetHosts(c.hosts)
	c.gateway.SetHosts(c.hosts)

	// Start metrics server if enabled
	if c.config.Metrics.Enabled && c.metrics != nil {
		log.Debug("Controller.Start: starting metrics server")
		if err := c.startMetricsServer(); err != nil {
			log.Debug("Controller.Start: metrics server failed", zap.Error(err))
			return fmt.Errorf("failed to start metrics server: %w", err)
		}
	} else {
		log.Debug("Controller.Start: metrics server disabled",
			zap.Bool("enabled", c.config.Metrics.Enabled), zap.Bool("has_metrics", c.metrics != nil))
	}

	// Bring up the event bus, health detector and Webhook receivers before the
	// API, so a watcher that connects the instant the port opens does not miss
	// the first poll's findings.
	c.startNotifications()

	// Start gRPC server
	if err := c.startGRPCServer(); err != nil {
		log.Debug("Controller.Start: gRPC server failed", zap.Error(err))
		return fmt.Errorf("failed to start gRPC server: %w", err)
	}

	// Start UI server on whatever [ui] asks for.
	if c.config.UI.UIEnabled() {
		uiAddr, uiPort := c.config.UI.UIAddress(c.config.Server.ListenAddress)
		log.Debug("Controller.Start: starting UI server", zap.String("address", uiAddr), zap.Int("port", uiPort))
		// The UI proxies its own-origin /v1 and /ai to these, so that publishing
		// the UI port alone is enough to use it from outside the LAN.
		uiServer, err := NewUIServer(c.logger, uiAddr, uiPort, c.restPort(), defaultAIPort, c.restLoopbackTLS)
		if err != nil {
			log.Debug("Controller.Start: UI server creation failed", zap.Error(err))
			return fmt.Errorf("failed to create UI server: %w", err)
		}
		c.uiServer = uiServer
		if err := c.uiServer.Start(); err != nil {
			log.Debug("Controller.Start: UI server start failed", zap.Error(err))
			return fmt.Errorf("failed to start UI server: %w", err)
		}
	} else {
		log.Debug("Controller.Start: UI disabled")
	}

	// Start the snapshot scheduler. Only the active controller reaches here
	// (self-HA promotes a single node), so no distributed coordination is
	// needed. Schedules persist in the DB and reload on the next active node
	// after failover.
	log.Debug("Controller.Start: snapshot scheduler decision",
		zap.Bool("schedule_enabled", c.config.Schedule.Enabled), zap.Bool("has_db", c.db != nil))
	if c.config.Schedule.Enabled && c.db != nil {
		if err := c.schedules.Start(context.Background()); err != nil {
			c.logger.Warn("Failed to start snapshot scheduler", zap.Error(err))
		}
	}

	// The audit trail is sent off the cluster and pruned by the active
	// controller only; its cursor moves with the database on failover.
	log.Debug("Controller.Start: starting background loops")
	c.startAuditShipper(c.ctx)
	go c.watchLockClock(c.ctx)
	c.startAutoEvict(c.ctx)
	if c.resources != nil {
		c.resources.resumeMoves(c.ctx)
	}
	c.startStorageUpkeep(c.ctx)

	// A backup left "running" belongs to a controller that died mid-transfer;
	// only the active controller ships backups, so nothing can still be in
	// flight here. Resolve it now so an incomplete copy is never listed as
	// something that could be restored.
	log.Debug("Controller.Start: reconciling interrupted backups and self-HA services",
		zap.Bool("has_db", c.db != nil), zap.Bool("has_backups", c.backups != nil))
	if c.db != nil && c.backups != nil {
		if err := c.backups.ReconcileInterrupted(context.Background()); err != nil {
			c.logger.Warn("Failed to reconcile interrupted backups", zap.Error(err))
		}
	}

	if c.resources != nil {
		c.resources.reconcileSelfHaServices(context.Background())
	}

	c.logger.Info("Haify controller started",
		zap.String("address", c.config.Server.ListenAddress),
		zap.Int("port", c.config.Server.Port),
		zap.Strings("hosts", c.hosts))

	return nil
}

// Events returns the notification bus, or nil when notifications are disabled.
func (c *Controller) Events() *event.Bus { return c.events }

// Stop stops the controller
func (c *Controller) Stop() {
	c.logger.Info("Stopping Haify controller")
	log := c.traceLogger()
	log.Debug("Controller.Stop: stopping components",
		zap.Bool("has_schedules", c.schedules != nil), zap.Bool("has_alert_monitor", c.alertMonitor != nil),
		zap.Bool("has_metrics_server", c.metricsServer != nil), zap.Bool("has_grpc_server", c.server != nil),
		zap.Bool("has_rest_server", c.restServer != nil), zap.Bool("has_ui_server", c.uiServer != nil))

	c.cancel()

	// Stop the snapshot scheduler
	if c.schedules != nil {
		c.schedules.Stop()
	}

	// Stop the health detector. Subscribers (Webhooks, watch streams) unwind on
	// their own once c.cancel above propagates.
	if c.alertMonitor != nil {
		c.alertMonitor.Stop()
	}
	// The history is saved every few minutes; what came since goes now, so a
	// restart or a move to another node does not leave a gap in the charts.
	if c.history != nil {
		c.history.flush()
	}

	// Stop metrics server
	if c.metricsServer != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		start := time.Now()
		if err := c.metricsServer.Shutdown(ctx); err != nil {
			c.logger.Error("Failed to shutdown metrics server", zap.Error(err))
		}
		log.Debug("Controller.Stop: metrics server shut down", zap.Duration("took", time.Since(start)))
	}

	// Stop gRPC server
	if c.server != nil {
		start := time.Now()
		c.server.GracefulStop()
		log.Debug("Controller.Stop: gRPC server stopped", zap.Duration("took", time.Since(start)))
	}

	// Stop REST gateway server
	if c.restServer != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		start := time.Now()
		if err := c.restServer.Shutdown(ctx); err != nil {
			c.logger.Error("Failed to shutdown REST gateway server", zap.Error(err))
		}
		log.Debug("Controller.Stop: REST gateway server shut down", zap.Duration("took", time.Since(start)))
	}

	// Stop UI server
	if c.uiServer != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		start := time.Now()
		if err := c.uiServer.Shutdown(ctx); err != nil {
			c.logger.Error("Failed to shutdown UI server", zap.Error(err))
		}
		log.Debug("Controller.Stop: UI server shut down", zap.Duration("took", time.Since(start)))
	}

	c.logger.Info("Haify controller stopped")
}

// GetHosts returns the list of hosts
func (c *Controller) GetHosts() []string {
	return c.hosts
}

// GetDeployment returns the deployment client
func (c *Controller) GetDeployment() deploymentClient {
	return c.deployment
}

// GetMetrics returns the metrics instance
func (c *Controller) GetMetrics() *metrics.Metrics {
	return c.metrics
}

// Close closes the controller and its resources
func (c *Controller) Close() error {
	c.logger.Info("Closing controller")
	log := c.traceLogger()
	log.Debug("Controller.Close: closing", zap.Bool("has_grpc_server", c.server != nil), zap.Bool("has_db", c.db != nil))

	// Stop gRPC server
	if c.server != nil {
		c.server.GracefulStop()
	}

	// Close database
	if c.db != nil {
		start := time.Now()
		if err := c.db.Close(); err != nil {
			c.logger.Error("Failed to close database", zap.Error(err))
		}
		log.Debug("Controller.Close: database closed", zap.Duration("took", time.Since(start)))
	}

	// Cancel context
	c.cancel()

	return nil
}
