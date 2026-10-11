// Package controller provides the Haify controller
package controller

import (
	"context"
	"crypto/tls"
	"encoding/json"
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
}

// SetLogRing attaches the buffer the log view reads from. The ring has to exist
// before the logger that tees into it, which happens in main, so it is handed
// over here rather than built by New.
func (c *Controller) SetLogRing(r *logbuf.Ring) { c.logRing = r }

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
		return
	}
	if logger == nil {
		logger = zap.NewNop()
	}
	if err := db.Close(); err != nil {
		logger.Warn("Failed to close database after an aborted controller start", zap.Error(err))
	}
}

// New creates a new controller
func New(cfg *config.Config, logger *zap.Logger) (*Controller, error) {
	ctx, cancel := context.WithCancel(context.Background())

	// Open database
	db, err := database.Open(&database.Config{
		Path:           cfg.Database.Path,
		AuditRetention: cfg.Audit.MaxEntries,
		AuditMaxAge:    time.Duration(cfg.Audit.RetentionDays) * 24 * time.Hour,
	}, logger)
	if err != nil {
		// A schema this binary cannot safely write is the one open failure that
		// must not degrade into "continue without persistence". Running on with
		// db == nil means the controller reports an empty cluster and then
		// starts writing that view back over a database whose real content it
		// never understood — which is precisely the data loss the version check
		// exists to prevent. Refuse to start and let the operator roll forward
		// or restore.
		if database.IsSchemaIncompatible(err) {
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
	ctrl.deployment = &resPathsDeployment{deploymentClient: deploymentClient, extra: func() map[string][]string {
		if ctrl.nodes == nil {
			return nil
		}
		return ctrl.nodes.replicationPathsByDRBDName()
	}}

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
	}

	return ctrl, nil
}

// initHostsMapping initializes hostname to address mapping
func (c *Controller) initHostsMapping() {
	// Get hostnames from all hosts
	if len(c.hosts) == 0 {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	for _, host := range c.hosts {
		result, err := c.deployment.Exec(ctx, []string{host}, "hostname")
		if err == nil && result.AllSuccess() {
			for h, r := range result.Hosts {
				if r.Success && r.Output != "" {
					hostname := r.Output
					c.hostsMap[hostname] = h
					c.logger.Debug("Host mapping",
						zap.String("hostname", hostname),
						zap.String("address", h))
				}
			}
		}
	}
}

// loadHostsFromDatabase loads hosts from registered nodes in database
func (c *Controller) loadHostsFromDatabase(ctx context.Context) error {
	nodes, err := c.nodes.ListNodes(ctx)
	if err != nil {
		return err
	}

	if len(nodes) == 0 {
		return fmt.Errorf("no nodes found in database")
	}

	var hosts []string
	for _, node := range nodes {
		hosts = append(hosts, node.Address)
	}

	c.hosts = hosts
	c.logger.Info("Loaded hosts from database", zap.Strings("hosts", hosts))
	return nil
}

// Start starts the controller
func (c *Controller) Start() error {
	c.logger.Info("Starting Haify controller")

	// Load hosts from registered nodes in database
	if c.db != nil {
		if err := c.loadHostsFromDatabase(context.Background()); err != nil {
			c.logger.Warn("Failed to load hosts from database", zap.Error(err))
		}
	}

	// Initialize deployment client with hosts
	c.resources.SetDeployment(c.deployment)
	c.resources.SetHosts(c.hosts)
	c.gateway.SetHosts(c.hosts)

	// Start metrics server if enabled
	if c.config.Metrics.Enabled && c.metrics != nil {
		if err := c.startMetricsServer(); err != nil {
			return fmt.Errorf("failed to start metrics server: %w", err)
		}
	}

	// Bring up the event bus, health detector and Webhook receivers before the
	// API, so a watcher that connects the instant the port opens does not miss
	// the first poll's findings.
	c.startNotifications()

	// Start gRPC server
	if err := c.startGRPCServer(); err != nil {
		return fmt.Errorf("failed to start gRPC server: %w", err)
	}

	// Start UI server on whatever [ui] asks for.
	if c.config.UI.UIEnabled() {
		uiAddr, uiPort := c.config.UI.UIAddress(c.config.Server.ListenAddress)
		// The UI proxies its own-origin /v1 and /ai to these, so that publishing
		// the UI port alone is enough to use it from outside the LAN.
		uiServer, err := NewUIServer(c.logger, uiAddr, uiPort, c.restPort(), defaultAIPort, c.restLoopbackTLS)
		if err != nil {
			return fmt.Errorf("failed to create UI server: %w", err)
		}
		c.uiServer = uiServer
		if err := c.uiServer.Start(); err != nil {
			return fmt.Errorf("failed to start UI server: %w", err)
		}
	}

	// Start the snapshot scheduler. Only the active controller reaches here
	// (self-HA promotes a single node), so no distributed coordination is
	// needed. Schedules persist in the DB and reload on the next active node
	// after failover.
	if c.config.Schedule.Enabled && c.db != nil {
		if err := c.schedules.Start(context.Background()); err != nil {
			c.logger.Warn("Failed to start snapshot scheduler", zap.Error(err))
		}
	}

	// The audit trail is sent off the cluster and pruned by the active
	// controller only; its cursor moves with the database on failover.
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

	// Stop metrics server
	if c.metricsServer != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := c.metricsServer.Shutdown(ctx); err != nil {
			c.logger.Error("Failed to shutdown metrics server", zap.Error(err))
		}
	}

	// Stop gRPC server
	if c.server != nil {
		c.server.GracefulStop()
	}

	// Stop REST gateway server
	if c.restServer != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := c.restServer.Shutdown(ctx); err != nil {
			c.logger.Error("Failed to shutdown REST gateway server", zap.Error(err))
		}
	}

	// Stop UI server
	if c.uiServer != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := c.uiServer.Shutdown(ctx); err != nil {
			c.logger.Error("Failed to shutdown UI server", zap.Error(err))
		}
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

// NodeName is the reverse of ResolveHost: the node name registered for an
// address, or the address itself when none is. Messages meant for an operator
// name nodes the way every command takes them.
//
// The registered node name wins. The hosts map also carries each node's
// hostname as an alias of the same address, and taking whichever map entry
// came first returned one name on one call and the other on the next: alerts
// keyed by node (pool.data_near_full) then cleared as "no longer exists" and
// fired again under the other name every poll.
func (c *Controller) NodeName(address string) string {
	if c.nodes != nil {
		if name := c.nodes.GetNodeNameByAddress(address); name != "" {
			return name
		}
	}
	c.hostsLock.RLock()
	defer c.hostsLock.RUnlock()
	best := ""
	for name, addr := range c.hostsMap {
		if addr == address && name != address && (best == "" || name < best) {
			best = name
		}
	}
	if best != "" {
		return best
	}
	return address
}

// ResolveHost resolves a hostname to an address
func (c *Controller) ResolveHost(hostOrAddr string) string {
	c.hostsLock.RLock()
	defer c.hostsLock.RUnlock()

	// Try to resolve hostname to address first
	if addr, ok := c.hostsMap[hostOrAddr]; ok {
		return addr
	}

	// If it's already an address in our hosts list, return as is
	for _, addr := range c.hosts {
		if addr == hostOrAddr {
			return hostOrAddr
		}
	}

	// Return as-is (might be a hostname that SSH can resolve)
	return hostOrAddr
}

// NormalizeHost converts an address to hostname if available
// Used for display purposes to avoid showing duplicates
func (c *Controller) NormalizeHost(addrOrHost string) string {
	c.hostsLock.RLock()
	defer c.hostsLock.RUnlock()

	// If it's already in hosts list, return as is (prefer hostnames over IPs)
	for _, host := range c.hosts {
		if host == addrOrHost {
			return host
		}
	}

	// Reverse lookup: check if this address maps to a hostname
	for hostname, addr := range c.hostsMap {
		if addr == addrOrHost {
			return hostname
		}
	}

	// Return as-is
	return addrOrHost
}

// ==================== DATABASE ====================

// loadFromDatabase loads nodes and gateways from database
func (c *Controller) loadFromDatabase(ctx context.Context) error {
	// Load nodes
	dbNodes, err := c.db.ListNodes(ctx)
	if err != nil {
		return fmt.Errorf("failed to load nodes: %w", err)
	}

	for _, dbNode := range dbNodes {
		var labels map[string]string
		if dbNode.Labels != "" {
			if err := json.Unmarshal([]byte(dbNode.Labels), &labels); err != nil {
				c.logger.Warn("Failed to decode node labels; ignoring",
					zap.String("node", dbNode.Name), zap.Error(err))
			}
		}
		c.nodes.mu.Lock()
		c.nodes.nodes[dbNode.Address] = &NodeInfo{
			Name:               dbNode.Name,
			Address:            dbNode.Address,
			ReplicationAddress: dbNode.ReplicationAddress,
			ReplicationPaths:   dbNode.ReplicationPaths,
			Hostname:           dbNode.Hostname,
			State:              NodeState(dbNode.State),
			LastSeen:           dbNode.LastSeen,
			Version:            dbNode.Version,
			Capacity:           make(map[string]interface{}),
			Labels:             labels,
			OfflineSince:       dbNode.OfflineSince,
		}
		c.nodes.mu.Unlock()

		// Build hostname -> IP address mapping for DRBD config
		if dbNode.Hostname != "" && dbNode.Address != "" {
			c.hostsLock.Lock()
			c.hostsMap[dbNode.Hostname] = dbNode.Address
			c.hostsLock.Unlock()
		}
		// Also map name -> IP if different from hostname
		if dbNode.Name != "" && dbNode.Name != dbNode.Hostname && dbNode.Address != "" {
			c.hostsLock.Lock()
			c.hostsMap[dbNode.Name] = dbNode.Address
			c.hostsLock.Unlock()
		}

		c.logger.Debug("Loaded node from database",
			zap.String("name", dbNode.Name),
			zap.String("address", dbNode.Address))
	}

	// Load gateways
	dbGateways, err := c.db.ListGateways(ctx)
	if err != nil {
		return fmt.Errorf("failed to load gateways: %w", err)
	}

	for _, dbGateway := range dbGateways {
		c.logger.Debug("Loaded gateway from database",
			zap.String("name", dbGateway.Name),
			zap.String("type", string(dbGateway.Type)),
			zap.String("resource", dbGateway.Resource))
	}

	c.logger.Info("Loaded data from database",
		zap.Int("nodes", len(dbNodes)),
		zap.Int("gateways", len(dbGateways)))

	return nil
}

// Close closes the controller and its resources
func (c *Controller) Close() error {
	c.logger.Info("Closing controller")

	// Stop gRPC server
	if c.server != nil {
		c.server.GracefulStop()
	}

	// Close database
	if c.db != nil {
		if err := c.db.Close(); err != nil {
			c.logger.Error("Failed to close database", zap.Error(err))
		}
	}

	// Cancel context
	c.cancel()

	return nil
}
