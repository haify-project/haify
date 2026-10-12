package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	haifypb "github.com/haify-project/haify/api/proto/v1"
	"github.com/haify-project/haify/pkg/database"
	"github.com/haify-project/haify/pkg/event"
	"go.uber.org/zap"
)

// ==================== NOTIFICATION CHANNELS ====================
//
// Where alerts go. Two rules shape this surface, both inherited from the backup
// target RPCs for the same reasons: a channel's signing secret is write-only,
// and a change takes effect immediately rather than at the next restart.
//
// Traces here never carry a channel's URL, headers or secret: a webhook URL
// or an Authorization header can embed a token, so only name, kind and
// presence flags are logged.

func (s *Server) ListNotifyChannels(ctx context.Context, _ *haifypb.ListNotifyChannelsRequest) (*haifypb.ListNotifyChannelsResponse, error) {
	log := s.serverLog()
	log.Debug("ListNotifyChannels: listing")
	kinds := make([]string, 0, len(event.Kinds()))
	for _, k := range event.Kinds() {
		kinds = append(kinds, string(k))
	}
	if s.ctrl.db == nil {
		log.Debug("ListNotifyChannels: no controller database")
		return &haifypb.ListNotifyChannelsResponse{
			Success: false,
			Message: "notification channels require the controller database",
			Kinds:   kinds,
		}, nil
	}
	start := time.Now()
	channels, err := s.ctrl.db.ListNotifyChannels(ctx)
	if err != nil {
		log.Debug("ListNotifyChannels: database read failed", zap.Duration("took", time.Since(start)), zap.Error(err))
		return &haifypb.ListNotifyChannelsResponse{Success: false, Message: err.Error(), Kinds: kinds}, nil
	}
	log.Debug("ListNotifyChannels: read from database", zap.Int("count", len(channels)), zap.Duration("took", time.Since(start)))
	out := make([]*haifypb.NotifyChannelInfo, 0, len(channels))
	for _, c := range channels {
		log.Debug("ListNotifyChannels: channel", zap.String("channel", c.Name), zap.String("kind", c.Kind),
			zap.Bool("enabled", c.Enabled))
		out = append(out, notifyChannelInfo(c))
	}
	return &haifypb.ListNotifyChannelsResponse{
		Success: true, Message: "Notification channels listed successfully",
		Channels: out, Kinds: kinds,
	}, nil
}

func (s *Server) SaveNotifyChannel(ctx context.Context, req *haifypb.SaveNotifyChannelRequest) (*haifypb.SaveNotifyChannelResponse, error) {
	log := s.serverLog()
	log.Debug("SaveNotifyChannel: saving", zap.String("channel", req.Name), zap.String("kind", req.Kind),
		zap.Bool("has_url", req.Url != ""), zap.Int("headers", len(req.Headers)), zap.String("min_severity", req.MinSeverity),
		zap.Strings("types", req.Types), zap.Bool("enabled", req.Enabled),
		zap.Bool("has_secret", req.Secret != ""), zap.Bool("clear_secret", req.ClearSecret))
	if s.ctrl.db == nil {
		log.Debug("SaveNotifyChannel: no controller database")
		return &haifypb.SaveNotifyChannelResponse{
			Success: false, Message: "notification channels require the controller database",
		}, nil
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		log.Debug("SaveNotifyChannel: no channel name")
		return &haifypb.SaveNotifyChannelResponse{Success: false, Message: "a channel name is required"}, nil
	}

	ch := &database.NotifyChannel{
		Name:        name,
		Kind:        strings.TrimSpace(req.Kind),
		URL:         strings.TrimSpace(req.Url),
		MinSeverity: strings.TrimSpace(req.MinSeverity),
		Types:       req.Types,
		Headers:     req.Headers,
		Enabled:     req.Enabled,
		Secret:      req.Secret,
	}

	// An empty secret means "leave what is stored alone". A UI cannot render a
	// write-only secret in order to send it back, so without this, editing the
	// severity threshold of a signed DingTalk channel would silently unsign it
	// and every later delivery would be rejected. clear_secret is the explicit
	// way to remove one.
	if existing, err := s.ctrl.db.GetNotifyChannel(ctx, name); err == nil {
		ch.CreatedAt = existing.CreatedAt
		switch {
		case req.ClearSecret:
			log.Debug("SaveNotifyChannel: existing channel, clearing its secret", zap.String("channel", name))
			ch.Secret = ""
		case req.Secret == "":
			log.Debug("SaveNotifyChannel: existing channel, keeping its stored secret", zap.String("channel", name),
				zap.Bool("has_secret", existing.Secret != ""))
			ch.Secret = existing.Secret
		default:
			log.Debug("SaveNotifyChannel: existing channel, replacing its secret", zap.String("channel", name))
		}
	} else {
		log.Debug("SaveNotifyChannel: new channel", zap.String("channel", name), zap.Error(err))
	}

	// Validated before it is stored, not at delivery time: a channel that
	// cannot work is a channel that silently is not alerting, and the moment to
	// find that out is while someone is looking at the form.
	if _, err := webhookConfigFor(ch); err != nil {
		log.Debug("SaveNotifyChannel: channel config invalid", zap.String("channel", name), zap.Error(err))
		return &haifypb.SaveNotifyChannelResponse{Success: false, Message: err.Error()}, nil
	}

	start := time.Now()
	if err := s.ctrl.db.SaveNotifyChannel(ctx, ch); err != nil {
		log.Debug("SaveNotifyChannel: database write failed", zap.String("channel", name),
			zap.Duration("took", time.Since(start)), zap.Error(err))
		return &haifypb.SaveNotifyChannelResponse{Success: false, Message: err.Error()}, nil
	}
	log.Debug("SaveNotifyChannel: stored", zap.String("channel", name), zap.Duration("took", time.Since(start)))
	if err := s.ctrl.reloadNotifyChannels(ctx); err != nil {
		log.Debug("SaveNotifyChannel: reload failed", zap.String("channel", name), zap.Error(err))
		return &haifypb.SaveNotifyChannelResponse{
			Success: false,
			Message: fmt.Sprintf("channel %q was saved but could not be activated: %v", name, err),
			Channel: notifyChannelInfo(ch),
		}, nil
	}
	log.Debug("SaveNotifyChannel: saved and activated", zap.String("channel", name))
	return &haifypb.SaveNotifyChannelResponse{
		Success: true,
		Message: fmt.Sprintf("Notification channel %q saved", name),
		Channel: notifyChannelInfo(ch),
	}, nil
}

func (s *Server) DeleteNotifyChannel(ctx context.Context, req *haifypb.DeleteNotifyChannelRequest) (*haifypb.DeleteNotifyChannelResponse, error) {
	log := s.serverLog()
	log.Debug("DeleteNotifyChannel: deleting", zap.String("channel", req.Name))
	if s.ctrl.db == nil {
		log.Debug("DeleteNotifyChannel: no controller database")
		return &haifypb.DeleteNotifyChannelResponse{
			Success: false, Message: "notification channels require the controller database",
		}, nil
	}
	if _, err := s.ctrl.db.GetNotifyChannel(ctx, req.Name); err != nil {
		log.Debug("DeleteNotifyChannel: channel not found", zap.String("channel", req.Name), zap.Error(err))
		return &haifypb.DeleteNotifyChannelResponse{Success: false, Message: err.Error()}, nil
	}
	if err := s.ctrl.db.DeleteNotifyChannel(ctx, req.Name); err != nil {
		log.Debug("DeleteNotifyChannel: database delete failed", zap.String("channel", req.Name), zap.Error(err))
		return &haifypb.DeleteNotifyChannelResponse{Success: false, Message: err.Error()}, nil
	}
	if err := s.ctrl.reloadNotifyChannels(ctx); err != nil {
		log.Debug("DeleteNotifyChannel: reload failed", zap.String("channel", req.Name), zap.Error(err))
		return &haifypb.DeleteNotifyChannelResponse{
			Success: false,
			Message: fmt.Sprintf("channel %q was deleted but delivery could not be reloaded: %v", req.Name, err),
		}, nil
	}
	log.Debug("DeleteNotifyChannel: deleted", zap.String("channel", req.Name))
	return &haifypb.DeleteNotifyChannelResponse{
		Success: true, Message: fmt.Sprintf("Notification channel %q deleted", req.Name),
	}, nil
}

func (s *Server) TestNotifyChannel(ctx context.Context, req *haifypb.TestNotifyChannelRequest) (*haifypb.TestNotifyChannelResponse, error) {
	log := s.serverLog()
	log.Debug("TestNotifyChannel: sending test message", zap.String("channel", req.Name))
	if s.ctrl.notify == nil {
		log.Debug("TestNotifyChannel: notifications disabled")
		return &haifypb.TestNotifyChannelResponse{
			Success: false,
			Message: "notifications are disabled on this controller ([alert] enabled = false)",
		}, nil
	}
	// Bounded independently of the caller: a chat service that accepts the
	// connection and then never answers would otherwise hold the RPC open for
	// as long as the client is willing to wait.
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	start := time.Now()
	if err := s.ctrl.notify.Test(ctx, req.Name); err != nil {
		log.Debug("TestNotifyChannel: failed", zap.String("channel", req.Name), zap.Duration("took", time.Since(start)), zap.Error(err))
		return &haifypb.TestNotifyChannelResponse{Success: false, Message: err.Error()}, nil
	}
	log.Debug("TestNotifyChannel: accepted", zap.String("channel", req.Name), zap.Duration("took", time.Since(start)))
	return &haifypb.TestNotifyChannelResponse{
		Success: true,
		Message: fmt.Sprintf("Test message accepted by %q", req.Name),
	}, nil
}

// notifyChannelInfo renders a stored channel for the API, without its secret.
func notifyChannelInfo(c *database.NotifyChannel) *haifypb.NotifyChannelInfo {
	info := &haifypb.NotifyChannelInfo{
		Name: c.Name, Kind: c.Kind, Url: c.URL,
		MinSeverity: c.MinSeverity, Types: c.Types, Headers: c.Headers,
		Enabled: c.Enabled, HasSecret: c.Secret != "",
	}
	if !c.CreatedAt.IsZero() {
		info.CreatedAt = c.CreatedAt.UTC().Format(time.RFC3339)
	}
	if !c.UpdatedAt.IsZero() {
		info.UpdatedAt = c.UpdatedAt.UTC().Format(time.RFC3339)
	}
	zap.L().Debug("notifyChannelInfo: rendered channel", zap.String("channel", c.Name), zap.String("kind", c.Kind),
		zap.Bool("has_secret", info.HasSecret))
	return info
}

// reloadNotifyChannels re-subscribes delivery after a channel changed.
//
// A controller with notifications disabled has no manager and no bus, so there
// is nothing to reload — and saving a channel there is still allowed, so the
// configuration is ready for whenever [alert] is turned on.
func (c *Controller) reloadNotifyChannels(ctx context.Context) error {
	log := c.traceLogger()
	if c.notify == nil {
		log.Debug("reloadNotifyChannels: notifications disabled, nothing to reload")
		return nil
	}
	start := time.Now()
	err := c.notify.Reload(ctx)
	log.Debug("reloadNotifyChannels: reloaded", zap.Duration("took", time.Since(start)), zap.Error(err))
	return err
}
