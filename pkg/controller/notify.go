package controller

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/haify-project/haify/pkg/database"
	"github.com/haify-project/haify/pkg/event"
)

// NotifyManager owns the alert delivery channels that live in the database.
//
// Receivers from controller.toml are started once at boot and never change;
// these can be added, edited, muted and removed while the controller runs. That
// is the whole point of them: reconfiguring alerting by editing a file and
// restarting means a deliberate window with no alerting, on a cluster that is
// usually having a bad day already.
//
// Applying a change means stopping every channel's subscription and starting
// the current set. That is heavier than diffing, and it is chosen anyway: a
// subscription carries a queue of undelivered events, so a diff would have to
// decide what happens to the queue of a channel whose severity threshold just
// changed, and every answer to that is a surprise to somebody. Channel edits
// are a human-scale operation; a moment of resubscription costs nothing.
//
// Traces here name channels and kinds only. A channel's URL carries the bot
// token, it also has a secret and headers, and the errors that validating or
// delivering to it returns quote the URL, so they are logged as present or not.
type NotifyManager struct {
	controller *Controller

	mu      sync.Mutex
	cancels []context.CancelFunc
	active  int
}

// NewNotifyManager creates the manager. It delivers nothing until Reload.
func NewNotifyManager(c *Controller) *NotifyManager {
	return &NotifyManager{controller: c}
}

// Active reports how many database-backed channels are currently subscribed.
func (nm *NotifyManager) Active() int {
	nm.mu.Lock()
	defer nm.mu.Unlock()
	return nm.active
}

// Reload rebuilds every database-backed channel subscription from the current
// contents of the database.
//
// A channel whose configuration is unusable is skipped with a log line rather
// than failing the reload: one bad entry must not take down delivery to the
// others, which is the case where somebody is waiting for a page.
func (nm *NotifyManager) Reload(ctx context.Context) error {
	log := loggerOfController(nm.controller)
	if nm.controller.db == nil || nm.controller.events == nil {
		log.Debug("NotifyManager.Reload: no database or event bus, nothing to reload",
			zap.Bool("has_db", nm.controller.db != nil), zap.Bool("has_events", nm.controller.events != nil))
		return nil
	}
	start := time.Now()
	channels, err := nm.controller.db.ListNotifyChannels(ctx)
	log.Debug("NotifyManager.Reload: listed notification channels", zap.Int("count", len(channels)),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return fmt.Errorf("list notification channels: %w", err)
	}

	nm.mu.Lock()
	defer nm.mu.Unlock()

	log.Debug("NotifyManager.Reload: stopping current subscriptions", zap.Int("count", len(nm.cancels)))
	for _, cancel := range nm.cancels {
		cancel()
	}
	nm.cancels = nil
	nm.active = 0

	for _, ch := range channels {
		if !ch.Enabled {
			log.Debug("NotifyManager.Reload: channel disabled, skipping", zap.String("channel", ch.Name))
			continue
		}
		cfg, err := webhookConfigFor(ch)
		if err != nil {
			nm.controller.logger.Warn("Skipping unusable notification channel",
				zap.String("channel", ch.Name), zap.Error(err))
			continue
		}
		log.Debug("NotifyManager.Reload: subscribing channel", zap.String("channel", ch.Name),
			zap.String("kind", string(cfg.Kind)), zap.Bool("has_url", cfg.URL != ""),
			zap.Bool("has_secret", cfg.Secret != ""), zap.Int("headers", len(cfg.Headers)),
			zap.String("min_severity", ch.MinSeverity), zap.Int("types", len(cfg.Filter.Types)))
		// The inspection reads these to tell a channel that delivers from one
		// that has failed every delivery since its receiver moved.
		cfg.OnResult = nm.controller.recordDelivery(ch.Name)
		// Derived from the controller's own context, so shutdown still tears
		// every channel down without the manager being involved.
		cctx, cancel := context.WithCancel(nm.controller.ctx)
		event.NewWebhook(cfg, nm.controller.logger.With(zap.String("channel", ch.Name))).
			Start(cctx, nm.controller.events)
		nm.cancels = append(nm.cancels, cancel)
		nm.active++
	}

	nm.controller.logger.Info("Notification channels reloaded",
		zap.Int("enabled", nm.active), zap.Int("configured", len(channels)))
	return nil
}

// Test delivers one synthetic event to a single channel and reports what the
// far end said.
//
// It bypasses the bus on purpose. A test that went through the bus would be
// broadcast to every other channel too — paging everyone to check one bot URL —
// and it would return before delivery, so a wrong secret would show up as
// success here and as a log line nobody reads.
func (nm *NotifyManager) Test(ctx context.Context, name string) error {
	log := loggerOfController(nm.controller)
	log.Debug("NotifyManager.Test: testing channel", zap.String("channel", name))
	if nm.controller.db == nil {
		log.Debug("NotifyManager.Test: no database")
		return fmt.Errorf("notification channels require the controller database")
	}
	start := time.Now()
	ch, err := nm.controller.db.GetNotifyChannel(ctx, name)
	log.Debug("NotifyManager.Test: read channel", zap.String("channel", name), zap.Bool("found", err == nil),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return err
	}
	cfg, err := webhookConfigFor(ch)
	if err != nil {
		log.Debug("NotifyManager.Test: channel configuration unusable", zap.String("channel", name))
		return err
	}
	// Retries off: the caller is a human waiting on a button, and a failure is
	// the answer they asked for rather than something to paper over.
	cfg.Retries = 0

	start = time.Now()
	err = event.NewWebhook(cfg, nm.controller.logger).Deliver(ctx, event.Event{
		Type:     event.TypeChannelTest,
		Severity: event.SeverityInfo,
		Status:   event.StatusInfo,
		Message: fmt.Sprintf("Test message from Haify for channel %q. "+
			"Alerts about this cluster will arrive here.", ch.Name),
		Details:   map[string]string{"channel": ch.Name, "kind": ch.Kind},
		Timestamp: time.Now().UTC(),
	})
	log.Debug("NotifyManager.Test: delivered test event", zap.String("channel", name), zap.String("kind", ch.Kind),
		zap.Bool("delivered", err == nil), zap.Duration("took", time.Since(start)))
	return err
}

// webhookConfigFor turns a stored channel into a delivery configuration.
func webhookConfigFor(ch *database.NotifyChannel) (event.WebhookConfig, error) {
	log := zap.L()
	log.Debug("webhookConfigFor: building", zap.String("channel", ch.Name), zap.String("kind", ch.Kind),
		zap.Bool("has_url", ch.URL != ""), zap.Bool("has_secret", ch.Secret != ""), zap.Int("headers", len(ch.Headers)))
	kind, err := event.ParseKind(ch.Kind)
	if err != nil {
		log.Debug("webhookConfigFor: unknown kind", zap.String("channel", ch.Name), zap.String("kind", ch.Kind))
		return event.WebhookConfig{}, err
	}
	if err := validateChannelURL(ch.URL); err != nil {
		log.Debug("webhookConfigFor: URL rejected", zap.String("channel", ch.Name))
		return event.WebhookConfig{}, err
	}
	// The URL's host settles the argument when it can. A Feishu bot URL with
	// the kind left on "generic" posts the raw event JSON, gets refused, and is
	// answered with HTTP 200 — indistinguishable from success anywhere in the
	// delivery path, because a generic receiver's body is the operator's own
	// and not ours to interpret. Caught here it costs one error message; missed
	// it costs an outage nobody hears about.
	if want, known := event.KindForURL(ch.URL); known && want != kind {
		log.Debug("webhookConfigFor: URL belongs to another kind", zap.String("channel", ch.Name),
			zap.String("kind", string(kind)), zap.String("url_kind", string(want)))
		return event.WebhookConfig{}, fmt.Errorf(
			"this URL is a %s endpoint but the channel kind is %q; %s will refuse every message "+
				"(and answer HTTP 200 while doing it). Set the kind to %s",
			want, kind, want, want)
	}
	types := make([]event.Type, 0, len(ch.Types))
	for _, t := range ch.Types {
		if t = strings.TrimSpace(t); t != "" {
			types = append(types, event.Type(t))
		}
	}
	log.Debug("webhookConfigFor: built", zap.String("channel", ch.Name), zap.String("kind", string(kind)),
		zap.String("min_severity", ch.MinSeverity), zap.Int("types", len(types)))
	return event.WebhookConfig{
		URL:     ch.URL,
		Kind:    kind,
		Secret:  ch.Secret,
		Headers: ch.Headers,
		Filter: event.Filter{
			MinSeverity: event.ParseSeverity(ch.MinSeverity),
			Types:       types,
		},
	}, nil
}

// validateChannelURL rejects a URL the delivery path could not use.
//
// The scheme check is not cosmetic: a value pasted with the scheme missing
// parses happily as a relative reference, and the resulting request fails per
// delivery with an error that describes the URL rather than the mistake.
//
// Its traces name the failed check only; the URL holds the token.
func validateChannelURL(raw string) error {
	log := zap.L()
	raw = strings.TrimSpace(raw)
	if raw == "" {
		log.Debug("validateChannelURL: empty URL")
		return fmt.Errorf("a notification channel needs a URL")
	}
	u, err := url.Parse(raw)
	if err != nil {
		log.Debug("validateChannelURL: URL does not parse")
		return fmt.Errorf("notification URL is not a URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		log.Debug("validateChannelURL: scheme is not http or https", zap.String("scheme", u.Scheme))
		return fmt.Errorf("notification URL must start with http:// or https:// (got %q)", raw)
	}
	if u.Host == "" {
		log.Debug("validateChannelURL: URL has no host")
		return fmt.Errorf("notification URL has no host: %q", raw)
	}
	log.Debug("validateChannelURL: URL accepted", zap.String("scheme", u.Scheme))
	return nil
}
