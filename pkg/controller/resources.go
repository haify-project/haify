package controller

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/haify-project/haify/pkg/deployment"
)

// ResourceInfo represents DRBD resource information
type ResourceInfo struct {
	Name       string
	Port       uint32
	Protocol   string
	Nodes      []string
	Role       string
	Volumes    []*ResourceVolumeInfo
	NodeStates map[string]*ResourceNodeState
	// DisklessNodes are nodes that join the resource purely as quorum
	// tiebreakers: they vote but store no data.
	DisklessNodes []string
	// DisklessClients are nodes attached as diskless data clients: no local
	// replica, but connected over DRBD and promotable to serve the volume over
	// the network. Distinct from DisklessNodes (quorum-only tiebreakers).
	DisklessClients []string
	// QuorumRisk is true when the resource has exactly two diskful nodes and
	// no tiebreaker, so losing either node suspends I/O (no quorum majority).
	QuorumRisk bool
	// FaultDomainRisk names the labelled fault domain(s) whose loss would take
	// every copy of the data or the quorum majority; see placement_domain.go.
	FaultDomainRisk string
	Labels          map[string]string
	Profile         string
	// WANMode marks a resource with an off-site asynchronous copy, and DRNode
	// names which of Nodes holds it. Listing the DR as just another replica is
	// misleading: it is a different site, replicates under protocol A, and never
	// takes over automatically.
	WANMode bool
	DRNode  string
	// Encrypted is true when every replica's backing volume is a LUKS2
	// container (DRBD → LUKS → LVM). Encryption at rest only: DRBD is
	// above the crypt layer, so replication traffic is plaintext.
	Encrypted bool
}

// ResourceNodeState represents detailed state of a node for a resource
type ResourceNodeState struct {
	Role        string
	DiskState   string
	Replication string
	// SyncPercent is the resync completion for a peer (0..100). It is 100 for a
	// node that is fully in sync / not resyncing, and only carries a meaningful
	// intermediate value while Replication is a resync state (SyncSource/
	// SyncTarget/PausedSync*).
	SyncPercent float64
	// SyncPercentKnown says whether SyncPercent was actually derived from DRBD
	// output. Only the structured `drbdsetup status --json` carries completion;
	// the plain-text fallback has no such field, so its states leave this false
	// with SyncPercent at its zero value.
	//
	// Anything charting completion over time must consult this first: a
	// text-parsed zero graphed as "0% synced" turns a perfectly healthy cluster
	// into one whose resync appears never to have started.
	SyncPercentKnown bool
	// OutOfSyncKiB is how much differs between this peer and the node whose
	// status was read, summed over volumes. Zero for that node itself.
	OutOfSyncKiB uint64
	// Quorum is whether this node holds DRBD quorum for the resource, or nil
	// when DRBD did not report it. Nil is the normal case for peers — a node's
	// status only carries its own quorum — and for the plain-text parse, which
	// does not surface quorum at all.
	Quorum *bool
	// Connection is the peer's DRBD connection state as the answering node sees
	// it: "Connected", "Connecting", "StandAlone", ... Empty for the answering
	// node itself, which has no connection to describe.
	//
	// It exists because a peer that is not Connected reports no role, no disk
	// and no replication at all — drbdadm prints one line, "<peer>
	// connection:Connecting", and nothing else. Until this field, such a peer
	// was simply absent from the map, and absent reads as "nothing wrong with
	// it" everywhere downstream. Two real failures came out of that: a replica
	// sat StandAlone after a split brain for a day with no alert, and a node
	// rejoining the cluster — whose peers are all Connecting for a few seconds
	// — made the whole resource look like it had no Primary.
	Connection string
	// TLS is whether the connection to this peer is encrypted.
	TLS bool
	// WrittenKiB is what DRBD wrote to the answering node's backing disks
	// since the resource came up there, summed over volumes; nil for peers and
	// when DRBD did not report it.
	WrittenKiB *uint64
	// ReadKiB is the answering node's read counter, like WrittenKiB.
	ReadKiB *uint64
	// SentKiB and ReceivedKiB are what the answering node sent to and received
	// from this peer since their connection came up, summed over volumes; nil
	// for the answering node itself and when DRBD did not report them.
	SentKiB     *uint64
	ReceivedKiB *uint64
}

// ResourceVolumeInfo represents DRBD volume information
type ResourceVolumeInfo struct {
	VolumeID uint32
	Device   string
	SizeGB   uint64
	// SizeBytes is the device's exact size when it was given one, else 0.
	SizeBytes uint64
	// Pool is the storage pool (volume group) backing this volume.
	Pool string
	// BackingVolume is the logical volume name inside the pool
	// (e.g. "<resource>_data"); "<pool>/<backing_volume>" is the path
	// consumed by snapshot operations.
	BackingVolume string
	// Encrypted is true when DRBD reaches this volume through a LUKS2
	// container rather than the LV/zvol directly. A snapshot of an encrypted
	// volume is a snapshot of the ciphertext.
	Encrypted bool
}

// ResourceManager manages DRBD resources using dispatch
type ResourceManager struct {
	controller *Controller
	deployment deploymentClient
	hosts      []string
	hostMap    map[string]string // hostname -> IP for config generation
	mu         sync.RWMutex
	// verifyMarks remembers, per resource and peer, how much was marked out of
	// sync when a verify started; see verifyMarks.
	verifyMarks sync.Map
	// haProbes keeps recent HA status probes of nodes; see GetHaStatus.
	haProbes haProbeCache
	// primaryHosts remembers, per resource, the address of the node last seen
	// Primary, so its status is read first; see drbdStatusFromAnyHost.
	primaryHosts sync.Map
	// alloc holds the ports and minors of creates still in progress; see
	// allocReservations.
	alloc allocReservations
	// unreachableHosts remembers hosts that failed a status query, so they
	// are asked last for a while; see unreachableFor.
	unreachableHosts sync.Map
}

// NewResourceManager creates a new resource manager
func NewResourceManager(ctrl *Controller) *ResourceManager {
	return &ResourceManager{
		controller: ctrl,
		hosts:      make([]string, 0),
		hostMap:    make(map[string]string),
	}
}

// resourcesLog is the logger the resource manager traces through: the
// controller's, or the global one for a manager built without a controller or
// logger (tests build them bare).
func (rm *ResourceManager) resourcesLog() *zap.Logger {
	if rm != nil && rm.controller != nil && rm.controller.logger != nil {
		return rm.controller.logger
	}
	return zap.L()
}

// resourcesFailedHosts renders, in a trace, the hosts a command failed on. It
// is lazy, so a trace that the level drops costs nothing; a nil result renders
// as an empty list.
type resourcesFailedHosts struct{ res *deployment.ExecResult }

// MarshalLogArray implements zapcore.ArrayMarshaler.
func (f resourcesFailedHosts) MarshalLogArray(enc zapcore.ArrayEncoder) error {
	if f.res == nil {
		return nil
	}
	for host, hr := range f.res.Hosts {
		if hr == nil || !hr.Success {
			enc.AppendString(host)
		}
	}
	return nil
}

// SetDeployment sets the deployment client
func (rm *ResourceManager) SetDeployment(client deploymentClient) {
	rm.resourcesLog().Debug("ResourceManager.SetDeployment: setting deployment client", zap.Bool("has_client", client != nil))
	rm.mu.Lock()
	defer rm.mu.Unlock()
	rm.deployment = client
}

// SetHosts sets the list of hosts for resource operations
func (rm *ResourceManager) SetHosts(hosts []string) {
	log := rm.resourcesLog()
	log.Debug("ResourceManager.SetHosts: setting hosts", zap.Strings("hosts", hosts))
	rm.mu.Lock()
	defer rm.mu.Unlock()

	// Clean hosts list and build map
	var cleanHosts []string
	rm.hostMap = make(map[string]string)

	for _, host := range hosts {
		// Try to resolve hostname to IP
		parts := strings.Split(host, ":")
		if len(parts) > 1 {
			// Format: "hostname:ip"
			hostname := parts[0]
			ip := parts[1]

			rm.hostMap[hostname] = ip
			cleanHosts = append(cleanHosts, ip)
			log.Debug("ResourceManager.SetHosts: mapped hostname to address", zap.String("node", hostname), zap.String("host", ip))
		} else {
			// Format: "hostname"
			rm.hostMap[host] = host
			cleanHosts = append(cleanHosts, host)
			log.Debug("ResourceManager.SetHosts: host is its own address", zap.String("host", host))
		}
	}
	rm.hosts = cleanHosts
	log.Debug("ResourceManager.SetHosts: hosts set", zap.Int("count", len(cleanHosts)))
}

// GetHosts returns the list of hosts
func (rm *ResourceManager) GetHosts() []string {
	rm.mu.RLock()
	defer rm.mu.RUnlock()
	return rm.hosts
}

func (rm *ResourceManager) resourceHosts(ctx context.Context, resource string) ([]string, error) {
	log := rm.resourcesLog()
	log.Debug("resourceHosts: resolving hosts", zap.String("resource", resource))
	if rm.controller.db != nil {
		dbRes, err := rm.controller.db.GetResource(ctx, resource)
		log.Debug("resourceHosts: database lookup", zap.String("resource", resource),
			zap.Bool("found", dbRes != nil), zap.Error(err))
		if err == nil && dbRes != nil {
			var hosts []string
			for _, node := range strings.Split(dbRes.Nodes, ",") {
				node = strings.TrimSpace(node)
				if node == "" {
					continue
				}
				host := rm.controller.ResolveHost(node)
				log.Debug("resourceHosts: member node", zap.String("resource", resource), zap.String("node", node), zap.String("host", host))
				hosts = append(hosts, host)
			}
			if len(hosts) > 0 {
				log.Debug("resourceHosts: hosts from database", zap.String("resource", resource), zap.Strings("hosts", hosts))
				return hosts, nil
			}
			log.Debug("resourceHosts: record lists no nodes; falling back to configured hosts", zap.String("resource", resource))
		}
	} else {
		log.Debug("resourceHosts: no database; using configured hosts", zap.String("resource", resource))
	}

	rm.mu.RLock()
	defer rm.mu.RUnlock()
	if len(rm.hosts) == 0 {
		log.Debug("resourceHosts: no hosts configured", zap.String("resource", resource))
		return nil, fmt.Errorf("no hosts configured")
	}
	log.Debug("resourceHosts: configured hosts", zap.String("resource", resource), zap.Strings("hosts", rm.hosts))
	return append([]string(nil), rm.hosts...), nil
}

// disklessHosts returns the resolved addresses of a resource's diskless quorum
// tiebreaker nodes, or nil when it has none. Kept separate from resourceHosts
// because tiebreakers must never be treated as data-bearing nodes (e.g. as
// failover Primary candidates). They do hold a copy of the resource config,
// though, so teardown and every change to the volume set must reach them —
// see disklessParticipantHosts.
func (rm *ResourceManager) disklessHosts(ctx context.Context, resource string) []string {
	log := rm.resourcesLog()
	if rm.controller.db == nil {
		log.Debug("disklessHosts: no database; no tiebreakers known", zap.String("resource", resource))
		return nil
	}
	dbRes, err := rm.controller.db.GetResource(ctx, resource)
	if err != nil || dbRes == nil || dbRes.DisklessNodes == "" {
		log.Debug("disklessHosts: no tiebreakers", zap.String("resource", resource),
			zap.Bool("found", dbRes != nil), zap.Error(err))
		return nil
	}
	var hosts []string
	for _, node := range strings.Split(dbRes.DisklessNodes, ",") {
		node = strings.TrimSpace(node)
		if node == "" {
			continue
		}
		host := rm.controller.ResolveHost(node)
		log.Debug("disklessHosts: tiebreaker node", zap.String("resource", resource), zap.String("node", node), zap.String("host", host))
		hosts = append(hosts, host)
	}
	return hosts
}

// VolumeSpec describes one DRBD volume to create: its size and (optionally) the
// pool it is backed by. The storage type is a resource-level property shared by
// all volumes.
type VolumeSpec struct {
	SizeGB uint32
	Pool   string
	// SizeBytes, when set, is the exact size the DRBD device presents
	// (rounded up to a 512-byte sector); the backing volume is still
	// allocated in whole GiB. SizeGB may then be left zero (exact_size.go).
	SizeBytes uint64
}

// resolvedVolume is a VolumeSpec with its pool auto-selected/normalized, a
// concrete backing-volume name and (later) an allocated device minor.
type resolvedVolume struct {
	id         int
	volumeName string
	pool       string
	sizeGB     uint32
	// exactBytes caps the DRBD device at this many bytes; zero leaves it at
	// whatever the backing volume holds.
	exactBytes uint64
	minor      int
	// encrypted routes DRBD at this volume's LUKS container instead of at the
	// LV/zvol. Carried per volume rather than passed alongside so the config
	// generator cannot be handed a volume list and the wrong flag.
	encrypted bool
}

// backingDevice is the path DRBD is pointed at for this volume: the crypt
// container when it is encrypted, the LV or zvol when it is not.
func (v resolvedVolume) backingDevice(storageType string) string {
	if v.encrypted {
		return luksMapperPath(v.pool, v.volumeName)
	}
	return backingPathForVolume(v.pool, v.volumeName, storageType)
}

// CreateResource creates a single-volume DRBD resource. It is a thin wrapper
// over CreateResourceWithVolumes retained for existing callers (CLI, self-HA,
// CSI) that only ever create one volume.
func (rm *ResourceManager) CreateResource(ctx context.Context, name string, port uint32, nodes []string, protocol string, sizeGB uint32, pool string, storageType string, drbdOptions map[string]string) error {
	rm.resourcesLog().Debug("CreateResource: creating single-volume resource", zap.String("resource", name),
		zap.Uint32("port", port), zap.Strings("nodes", nodes), zap.String("protocol", protocol),
		zap.Uint32("size_gb", sizeGB), zap.String("pool", pool), zap.String("storage_type", storageType),
		zap.Int("drbd_options", len(drbdOptions)))
	// LAN path: WAN is nil, so the resource is created exactly as before.
	return rm.CreateResourceWithVolumes(ctx, name, port, nodes, protocol, storageType, drbdOptions,
		[]VolumeSpec{{SizeGB: sizeGB, Pool: pool}}, nil)
}

// WANSpec carries the opt-in WAN-replication parameters supplied to
// CreateResourceWithVolumes. Nil ⇒ an ordinary LAN resource (behavior
// unchanged). When set, the resource replicates between the single primary node
// in `nodes` and DRNode across the internet via a per-resource haify-proxy pair
// (protocol A + loopback-routed DRBD). See docs/design/wan-replication.md.
type WANSpec struct {
	// DRNode is the DR-site node name. It must be a registered node and distinct
	// from the primary; it becomes the resource's second (and only) peer.
	DRNode string
	// DREndpoint is the DR site's public WAN address the primary dials.
	DREndpoint string
	// EgressAddress optionally pins the source address the primary's proxy binds
	// before dialing out, putting WAN replication on a chosen interface. Empty
	// lets the primary's routing table decide.
	EgressAddress string
	// WANPort is the WAN mTLS port the DR acceptor binds. Zero ⇒ the controller
	// picks a random high port (> 3000).
	WANPort uint32
}

// ResourceMetadata carries the resource-level choices that are not part of the
// volume list: organizational metadata, and whether the backing volumes are
// encrypted at rest.
type ResourceMetadata struct {
	Labels  map[string]string
	Profile string
	// Encrypt wraps every replica's backing volume in a LUKS2 container, so
	// the stack becomes DRBD → LUKS → LVM. Unlike Labels and Profile this
	// one does change what gets built, and it can only be chosen here: see
	// assertEncryptionNotRetrofitted.
	Encrypt bool
}

// CreateResourceWithVolumes creates a DRBD resource with one or more volumes
// (volume 0..N) atomically across the given nodes. All volumes share the
// resource's storage type; each may target its own pool.
func (rm *ResourceManager) CreateResourceWithVolumes(ctx context.Context, name string, port uint32, nodes []string, protocol string, storageType string, drbdOptions map[string]string, volumes []VolumeSpec, wan *WANSpec) error {
	rm.resourcesLog().Debug("CreateResourceWithVolumes: creating resource without metadata", zap.String("resource", name),
		zap.Uint32("port", port), zap.Strings("nodes", nodes), zap.Int("volumes", len(volumes)), zap.Bool("wan", wan != nil))
	return rm.CreateResourceWithVolumesMetadata(ctx, name, port, nodes, protocol, storageType, drbdOptions, volumes, wan, ResourceMetadata{})
}

func cloneStringMap(values map[string]string) map[string]string {
	if len(values) == 0 {
		return nil
	}
	cloned := make(map[string]string, len(values))
	for key, value := range values {
		cloned[key] = value
	}
	return cloned
}
