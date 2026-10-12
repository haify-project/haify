package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/haify-project/haify/api/proto/v1"
	"github.com/haify-project/haify/pkg/alert"
	"github.com/haify-project/haify/pkg/event"
	"github.com/haify-project/haify/pkg/rbac"
)

// defaultEventLimit caps an unbounded ListEvents request. Large enough to cover
// an incident, small enough not to hand a browser the entire ring on every
// refresh.
const defaultEventLimit = 200

// ListEvents returns retained notifications, oldest first.
func (s *Server) ListEvents(ctx context.Context, req *pb.ListEventsRequest) (*pb.ListEventsResponse, error) {
	log := s.ctrl.traceLogger()
	bus := s.ctrl.Events()
	if bus == nil {
		log.Debug("Server.ListEvents: notifications disabled")
		return &pb.ListEventsResponse{
			Success: false,
			Message: "event notifications are disabled; set [alert] enabled = true in controller.toml",
		}, nil
	}

	limit := int(req.GetLimit())
	if limit <= 0 {
		limit = defaultEventLimit
	}
	events := bus.Recent(filterFromRequest(req.GetMinSeverity(), req.GetTypes(), req.GetResource()), req.GetSinceId(), limit)

	published, dropped, _ := bus.Stats()
	log.Debug("Server.ListEvents: read retained events", zap.Int("limit", limit), zap.Uint64("since_id", req.GetSinceId()),
		zap.Int("count", len(events)), zap.Uint64("published", published), zap.Uint64("dropped", dropped))
	out := make([]*pb.Event, 0, len(events))
	for _, e := range events {
		out = append(out, eventToProto(e))
	}
	return &pb.ListEventsResponse{
		Success:   true,
		Events:    out,
		Published: published,
		Dropped:   dropped,
	}, nil
}

// WatchEvents streams notifications until the client disconnects or the
// controller shuts down.
func (s *Server) WatchEvents(req *pb.WatchEventsRequest, stream pb.HaifyController_WatchEventsServer) error {
	log := s.ctrl.traceLogger()
	bus := s.ctrl.Events()
	if bus == nil {
		log.Debug("Server.WatchEvents: notifications disabled")
		return status.Error(codes.FailedPrecondition,
			"event notifications are disabled; set [alert] enabled = true in controller.toml")
	}
	filter := filterFromRequest(req.GetMinSeverity(), req.GetTypes(), req.GetResource())

	// Subscribe before replaying history. The other order leaves a window in
	// which an event published between the replay and the subscription reaches
	// neither, which is exactly the event a reconnecting client came back for.
	ch, cancel := bus.Subscribe(filter)
	defer cancel()

	var lastID uint64
	replay := bus.Recent(filter, req.GetSinceId(), defaultEventLimit)
	log.Debug("Server.WatchEvents: subscribed, replaying history", zap.Uint64("since_id", req.GetSinceId()),
		zap.Int("count", len(replay)))
	for _, e := range replay {
		if err := stream.Send(eventToProto(e)); err != nil {
			log.Debug("Server.WatchEvents: replay send failed", zap.Uint64("event_id", e.ID), zap.Error(err))
			return err
		}
		lastID = e.ID
	}

	ctx := stream.Context()
	for {
		select {
		case <-ctx.Done():
			log.Debug("Server.WatchEvents: client gone", zap.Uint64("last_id", lastID))
			return nil
		case <-s.ctrl.ctx.Done():
			log.Debug("Server.WatchEvents: controller stopping", zap.Uint64("last_id", lastID))
			return nil
		case e, ok := <-ch:
			if !ok {
				log.Debug("Server.WatchEvents: subscription closed", zap.Uint64("last_id", lastID))
				return nil
			}
			// Suppress the overlap between the replay and the live feed.
			if e.ID <= lastID {
				log.Debug("Server.WatchEvents: already replayed, skipped", zap.Uint64("event_id", e.ID))
				continue
			}
			lastID = e.ID
			log.Debug("Server.WatchEvents: sending event", zap.Uint64("event_id", e.ID), zap.String("type", string(e.Type)),
				zap.String("resource", e.Resource))
			if err := stream.Send(eventToProto(e)); err != nil {
				log.Debug("Server.WatchEvents: send failed", zap.Uint64("event_id", e.ID), zap.Error(err))
				return err
			}
		}
	}
}

func filterFromRequest(minSeverity string, types []string, resource string) event.Filter {
	f := event.Filter{
		MinSeverity: event.ParseSeverity(minSeverity),
		Resource:    resource,
	}
	for _, t := range types {
		if t != "" {
			f.Types = append(f.Types, event.Type(t))
		}
	}
	return f
}

func eventToProto(e event.Event) *pb.Event {
	return &pb.Event{
		Id:              e.ID,
		Type:            string(e.Type),
		Severity:        string(e.Severity),
		Status:          string(e.Status),
		Resource:        e.Resource,
		Node:            e.Node,
		Message:         e.Message,
		Details:         e.Details,
		TimestampUnixMs: e.Timestamp.UnixMilli(),
	}
}

// ==================== Server-sent events ====================

// sseKeepalive bounds how long a stream can look dead to an intermediary. Load
// balancers and reverse proxies drop idle connections, and a quiet cluster is
// the normal case for this endpoint, so a comment frame goes out on this
// interval to keep the connection (and the operator's confidence) alive.
const sseKeepalive = 25 * time.Second

// registerEventRoutes adds the browser-facing event stream to the REST gateway.
//
// The gRPC-gateway already exposes WatchEvents at /v1/events/watch, but as
// newline-delimited JSON, which EventSource cannot read. This endpoint carries
// the same events in text/event-stream framing so the web UI can subscribe with
// four lines of JavaScript and get automatic reconnection for free.
func (c *Controller) registerEventRoutes(mux *runtime.ServeMux, engine *rbac.Engine) {
	c.traceLogger().Debug("Controller.registerEventRoutes: registering /v1/events/stream", zap.Bool("rbac", engine != nil))
	if err := mux.HandlePath("GET", "/v1/events/stream", c.eventStreamHandler(engine)); err != nil {
		c.logger.Error("failed to register event stream route", zap.Error(err))
	}
}

func (c *Controller) eventStreamHandler(engine *rbac.Engine) runtime.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request, _ map[string]string) {
		log := c.traceLogger()
		log.Debug("Controller.eventStreamHandler: stream requested", zap.String("remote", r.RemoteAddr))
		// This route bypasses the gRPC interceptors, so it has to check
		// credentials itself — the same way the RBAC introspection routes do.
		if !c.authorizeEventStream(engine, w, r) {
			return
		}

		bus := c.Events()
		if bus == nil {
			log.Debug("Controller.eventStreamHandler: notifications disabled")
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{
				"error": "event notifications are disabled; set [alert] enabled = true in controller.toml",
			})
			return
		}

		flusher, ok := w.(http.Flusher)
		if !ok {
			log.Debug("Controller.eventStreamHandler: response writer cannot flush")
			writeJSON(w, http.StatusInternalServerError, map[string]any{
				"error": "streaming unsupported by this server",
			})
			return
		}

		q := r.URL.Query()
		filter := filterFromRequest(q.Get("min_severity"), q["type"], q.Get("resource"))

		// EventSource resends the id of the last event it saw on reconnect, so
		// honouring Last-Event-ID is what makes browser reconnection lossless.
		var sinceID uint64
		if v := r.Header.Get("Last-Event-ID"); v != "" {
			_, _ = fmt.Sscanf(v, "%d", &sinceID)
		} else if v := q.Get("since_id"); v != "" {
			_, _ = fmt.Sscanf(v, "%d", &sinceID)
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		// Nginx buffers proxied responses by default, which holds every event
		// until the buffer fills — indistinguishable from a broken stream.
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)
		flusher.Flush()

		ch, cancel := bus.Subscribe(filter)
		defer cancel()

		var lastID uint64
		replay := bus.Recent(filter, sinceID, defaultEventLimit)
		log.Debug("Controller.eventStreamHandler: subscribed, replaying history", zap.Uint64("since_id", sinceID),
			zap.Int("count", len(replay)), zap.String("resource", filter.Resource))
		for _, e := range replay {
			if !writeSSE(w, e) {
				log.Debug("Controller.eventStreamHandler: client gone during replay", zap.Uint64("event_id", e.ID))
				return
			}
			lastID = e.ID
		}
		flusher.Flush()

		ticker := time.NewTicker(sseKeepalive)
		defer ticker.Stop()

		for {
			select {
			case <-r.Context().Done():
				log.Debug("Controller.eventStreamHandler: client gone", zap.Uint64("last_id", lastID))
				return
			case <-c.ctx.Done():
				log.Debug("Controller.eventStreamHandler: controller stopping", zap.Uint64("last_id", lastID))
				return
			case <-ticker.C:
				if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
					log.Debug("Controller.eventStreamHandler: keepalive failed, client gone", zap.Error(err))
					return
				}
				log.Debug("Controller.eventStreamHandler: keepalive sent")
				flusher.Flush()
			case e, ok := <-ch:
				if !ok {
					log.Debug("Controller.eventStreamHandler: subscription closed")
					return
				}
				if e.ID <= lastID {
					log.Debug("Controller.eventStreamHandler: already replayed, skipped", zap.Uint64("event_id", e.ID))
					continue
				}
				lastID = e.ID
				log.Debug("Controller.eventStreamHandler: sending event", zap.Uint64("event_id", e.ID),
					zap.String("type", string(e.Type)), zap.String("resource", e.Resource))
				if !writeSSE(w, e) {
					log.Debug("Controller.eventStreamHandler: write failed, client gone", zap.Uint64("event_id", e.ID))
					return
				}
				flusher.Flush()
			}
		}
	}
}

// authorizeEventStream applies whichever access control the controller is
// configured with. With neither RBAC nor a static token the endpoint is open,
// matching every other API surface on this port.
func (c *Controller) authorizeEventStream(engine *rbac.Engine, w http.ResponseWriter, r *http.Request) bool {
	// Tokens are never traced, only whether one was presented.
	log := c.traceLogger()
	presented := bearerFromHeader(r) != ""
	if engine != nil {
		if _, _, ok := engine.Whoami(bearerFromHeader(r)); !ok {
			log.Debug("Controller.authorizeEventStream: RBAC rejected token", zap.Bool("has_token", presented))
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unknown or missing API token"})
			return false
		}
		log.Debug("Controller.authorizeEventStream: RBAC accepted token")
		return true
	}
	if token := c.config.Auth.Token; token != "" {
		if bearerFromHeader(r) != token {
			log.Debug("Controller.authorizeEventStream: static token mismatch", zap.Bool("has_token", presented))
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "missing or invalid API token"})
			return false
		}
		log.Debug("Controller.authorizeEventStream: static token accepted")
		return true
	}
	log.Debug("Controller.authorizeEventStream: no access control configured, open")
	return true
}

// writeSSE emits one event frame. The id: field is what the browser echoes back
// as Last-Event-ID on reconnect. Returns false once the client is gone.
func writeSSE(w io.Writer, e event.Event) bool {
	body, err := json.Marshal(e)
	if err != nil {
		zap.L().Debug("writeSSE: event not encodable, skipped", zap.Uint64("event_id", e.ID), zap.Error(err))
		return true // skip this one; a single unencodable event is not fatal
	}
	_, err = fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", e.ID, e.Type, body)
	return err == nil
}

// ==================== Detector adapters ====================

// GetNodeStatusList adapts NodeManager to alert.NodeLister. Reachability is
// tested the same way every other operation reaches a node — an SSH round trip
// — because that is the dependency whose failure matters. A node that answers
// ICMP but not SSH is, for this controller's purposes, down.
func (nm *NodeManager) GetNodeStatusList(ctx context.Context) ([]alert.NodeStatusInfo, error) {
	log := nm.controller.traceLogger()
	nodes, err := nm.ListNodes(ctx)
	if err != nil {
		log.Debug("NodeManager.GetNodeStatusList: listing nodes failed", zap.Error(err))
		return nil, err
	}

	out := make([]alert.NodeStatusInfo, 0, len(nodes))
	for _, n := range nodes {
		name := n.Name
		if name == "" {
			name = n.Address
		}
		info := alert.NodeStatusInfo{Name: name, Reachable: true}
		start := time.Now()
		if err := nm.CheckNodeHealth(ctx, n.Address); err != nil {
			info.Reachable = false
			info.Message = err.Error()
		}
		log.Debug("NodeManager.GetNodeStatusList: node checked", zap.String("node", name), zap.String("host", n.Address),
			zap.Bool("reachable", info.Reachable), zap.Duration("took", time.Since(start)))
		out = append(out, info)
	}
	return out, nil
}

// GetPoolStatusList adapts StorageManager to alert.PoolLister.
//
// It reuses ListPools rather than issuing its own queries, which keeps the
// figures the monitor alerts on identical to the ones the UI and CLI show — an
// alert that disagrees with the page an operator opens next is worse than no
// alert. ListPools already folds thin pool utilisation in with one lvs call for
// the whole cluster, so this costs nothing beyond the listing itself.
//
// Pools that report no thin pool are passed through rather than filtered here;
// the monitor needs to see them to tell a pool that was converted to thick from
// one that was deleted.
func (sm *StorageManager) GetPoolStatusList(ctx context.Context) ([]alert.PoolStatusInfo, error) {
	log := sm.controller.traceLogger()
	pools, err := sm.ListPools(ctx)
	if err != nil {
		log.Debug("StorageManager.GetPoolStatusList: listing pools failed", zap.Error(err))
		return nil, err
	}

	vdo := sm.vdoPoolNames(ctx)
	log.Debug("StorageManager.GetPoolStatusList: pools listed", zap.Int("count", len(pools)), zap.Int("vdo", len(vdo)))
	out := make([]alert.PoolStatusInfo, 0, len(pools))
	for _, p := range pools {
		if p == nil {
			continue
		}
		log.Debug("StorageManager.GetPoolStatusList: pool", zap.String("pool", p.Name), zap.String("node", p.Node),
			zap.Uint64("total_bytes", p.TotalBytes), zap.Uint64("free_bytes", p.FreeBytes), zap.Bool("thin", p.ThinUsage != nil))
		info := alert.PoolStatusInfo{
			Name: p.Name,
			// Pools report their node by address; alerts are read by people,
			// who know nodes by name.
			Node:       sm.controller.NodeName(p.Node),
			TotalBytes: p.TotalBytes,
			FreeBytes:  p.FreeBytes,
		}
		if u := p.ThinUsage; u != nil {
			info.ThinPool = u.PoolLV
			info.DataPercent = u.DataPercent
			info.MetaPercent = u.MetaPercent
			info.OutOfSpace = u.OutOfSpace
			info.ThinSizeBytes = u.SizeBytes
		}
		info.VDO = vdo[p.Name]
		if v := p.VDO; v != nil {
			info.VDOPhysicalKnown = true
			info.VDOPhysicalPercent = v.PhysicalPercent
		}
		out = append(out, info)
	}
	return out, nil
}
