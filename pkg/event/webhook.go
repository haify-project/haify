package event

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"go.uber.org/zap"
)

// WebhookConfig describes one HTTP receiver.
type WebhookConfig struct {
	// URL receives a POST per event.
	URL string
	// Kind is the message format the far end expects. Empty means generic: the
	// Event JSON unchanged. A chat service needs its own envelope and rejects
	// anything else — see render.go.
	Kind Kind
	// Secret is the signing secret for kinds that authenticate that way
	// (DingTalk's 加签). It is never logged and never leaves this process
	// except as a signature.
	Secret string
	// Filter narrows what this receiver is sent. A pager wants critical only; a
	// chat channel may want everything.
	Filter Filter
	// Headers are added to every request — an auth token, a routing key, the
	// content type expected by a particular chat service.
	Headers map[string]string
	// Timeout bounds a single delivery attempt. Zero uses 10s.
	Timeout time.Duration
	// Retries is how many additional attempts a failed delivery gets. Zero uses
	// 2 (three attempts in total).
	Retries int
	// OnResult, when set, is told the final outcome of each bus-driven
	// delivery: nil once it was delivered, the last error once every attempt
	// failed. It runs on the delivery goroutine. A shutdown mid-retry reports
	// nothing, because nothing was decided.
	OnResult func(e Event, err error)
}

// Webhook delivers bus events to an HTTP endpoint.
//
// Deliveries run on the Webhook's own goroutine, reading from its bus
// subscription, so a receiver that is slow or down delays only its own queue.
// A failed delivery is retried with backoff and then dropped: holding it would
// mean an unbounded queue, and a stale alert delivered ten minutes late is
// worse than no alert, because the operator acts on it as if it were current.
type Webhook struct {
	cfg    WebhookConfig
	client *http.Client
	log    *zap.Logger
	done   chan struct{}
}

// NewWebhook creates a receiver. It does not start delivering until Start.
func NewWebhook(cfg WebhookConfig, log *zap.Logger) *Webhook {
	if log == nil {
		log = zap.NewNop()
	}
	if cfg.Timeout <= 0 {
		log.Debug("NewWebhook: timeout not set, using 10s", zap.String("host", webhookHost(cfg.URL)))
		cfg.Timeout = 10 * time.Second
	}
	if cfg.Retries <= 0 {
		log.Debug("NewWebhook: retries not set, using 2", zap.String("host", webhookHost(cfg.URL)))
		cfg.Retries = 2
	}
	log.Debug("NewWebhook: creating webhook receiver",
		zap.String("host", webhookHost(cfg.URL)), zap.String("kind", string(cfg.Kind)),
		zap.Bool("has_secret", cfg.Secret != ""), zap.Int("headers", len(cfg.Headers)),
		zap.String("min_severity", string(cfg.Filter.MinSeverity)), zap.Int("types", len(cfg.Filter.Types)),
		zap.String("resource", cfg.Filter.Resource), zap.Duration("timeout", cfg.Timeout),
		zap.Int("retries", cfg.Retries), zap.Bool("has_on_result", cfg.OnResult != nil))
	return &Webhook{
		cfg:    cfg,
		client: &http.Client{Timeout: cfg.Timeout},
		log:    log,
		done:   make(chan struct{}),
	}
}

// Start subscribes to bus and delivers matching events until ctx is cancelled.
func (w *Webhook) Start(ctx context.Context, bus *Bus) {
	host := webhookHost(w.cfg.URL)
	w.log.Debug("Webhook.Start: subscribing to the event bus", zap.String("host", host))
	ch, cancel := bus.Subscribe(w.cfg.Filter)
	go func() {
		defer close(w.done)
		defer cancel()
		for {
			select {
			case <-ctx.Done():
				w.log.Debug("Webhook.Start: context cancelled, delivery loop stopping", zap.String("host", host))
				return
			case e, ok := <-ch:
				if !ok {
					w.log.Debug("Webhook.Start: subscription closed, delivery loop stopping", zap.String("host", host))
					return
				}
				w.log.Debug("Webhook.Start: event received for delivery",
					zap.String("host", host), zap.Uint64("event_id", e.ID), zap.String("type", string(e.Type)))
				w.deliver(ctx, e)
			}
		}
	}()
}

// Wait blocks until the delivery goroutine has stopped. Only meaningful after
// Start and a cancelled context; used by tests to avoid racing on shutdown.
func (w *Webhook) Wait() { <-w.done }

// Deliver sends one event now and reports what happened, instead of logging it
// and moving on the way the bus-driven path does.
//
// This is what a "send a test message" button needs. The background path is
// deliberately fire-and-forget — an operator must not be blocked on a chat
// service being slow — but a person who just entered a bot URL is owed the
// actual answer, including the far end's own rejection text.
func (w *Webhook) Deliver(ctx context.Context, e Event) error {
	host := webhookHost(w.cfg.URL)
	w.log.Debug("Webhook.Deliver: sending event now",
		zap.String("host", host), zap.String("kind", string(w.cfg.Kind)),
		zap.Uint64("event_id", e.ID), zap.String("type", string(e.Type)))
	body, err := w.cfg.Kind.Render(e)
	if err != nil {
		w.log.Debug("Webhook.Deliver: encoding the event failed", zap.String("host", host), zap.Error(err))
		return fmt.Errorf("encode event: %w", err)
	}
	start := time.Now()
	err = w.post(ctx, body)
	w.log.Debug("Webhook.Deliver: delivery finished",
		zap.String("host", host), zap.Uint64("event_id", e.ID),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	return err
}

func (w *Webhook) deliver(ctx context.Context, e Event) {
	host := webhookHost(w.cfg.URL)
	w.log.Debug("Webhook.deliver: delivering event",
		zap.String("host", host), zap.String("kind", string(w.cfg.Kind)),
		zap.Uint64("event_id", e.ID), zap.String("type", string(e.Type)),
		zap.String("severity", string(e.Severity)), zap.Int("retries", w.cfg.Retries))
	body, err := w.cfg.Kind.Render(e)
	if err != nil {
		w.log.Error("webhook: encode event", zap.Error(err))
		w.report(e, fmt.Errorf("encode event: %w", err))
		return
	}

	backoff := 500 * time.Millisecond
	var lastErr error
	for attempt := 0; attempt <= w.cfg.Retries; attempt++ {
		if attempt > 0 {
			w.log.Debug("Webhook.deliver: backing off before retry",
				zap.String("host", host), zap.Uint64("event_id", e.ID),
				zap.Int("attempt", attempt+1), zap.Duration("backoff", backoff))
			select {
			case <-ctx.Done():
				w.log.Debug("Webhook.deliver: context cancelled mid-retry, outcome not reported",
					zap.String("host", host), zap.Uint64("event_id", e.ID))
				return
			case <-time.After(backoff):
			}
			backoff *= 2
		}
		start := time.Now()
		err := w.post(ctx, body)
		w.log.Debug("Webhook.deliver: attempt finished",
			zap.String("host", host), zap.Uint64("event_id", e.ID),
			zap.Int("attempt", attempt+1), zap.Duration("took", time.Since(start)), zap.Error(err))
		if err != nil {
			lastErr = err
			w.log.Warn("webhook: delivery failed",
				zap.String("url", RedactURL(w.cfg.URL)),
				zap.Uint64("event_id", e.ID),
				zap.Int("attempt", attempt+1),
				zap.Error(err))
			continue
		}
		w.log.Info("webhook: delivered",
			zap.String("url", RedactURL(w.cfg.URL)),
			zap.String("type", string(e.Type)),
			zap.String("severity", string(e.Severity)),
			zap.String("resource", e.Resource))
		w.report(e, nil)
		return
	}
	w.log.Error("webhook: giving up on event",
		zap.String("url", RedactURL(w.cfg.URL)),
		zap.Uint64("event_id", e.ID),
		zap.String("message", e.Message))
	w.report(e, lastErr)
}

func (w *Webhook) report(e Event, err error) {
	if w.cfg.OnResult != nil {
		w.log.Debug("Webhook.report: reporting delivery outcome",
			zap.String("host", webhookHost(w.cfg.URL)), zap.Uint64("event_id", e.ID),
			zap.Bool("delivered", err == nil), zap.Error(err))
		w.cfg.OnResult(e, err)
		return
	}
	w.log.Debug("Webhook.report: no result callback, outcome not reported",
		zap.Uint64("event_id", e.ID), zap.Bool("delivered", err == nil))
}

// webhookHost is the host part of a receiver URL, the only part safe to log:
// chat-service URLs carry their access token in the path or query.
func webhookHost(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Host
}

func (w *Webhook) post(ctx context.Context, body []byte) error {
	// Signed per attempt, not once: DingTalk's signature covers a timestamp it
	// only accepts within an hour, so a retry after a long backoff needs a
	// fresh one.
	host := webhookHost(w.cfg.URL)
	w.log.Debug("Webhook.post: posting to receiver",
		zap.String("host", host), zap.String("kind", string(w.cfg.Kind)),
		zap.Int("body_bytes", len(body)), zap.Bool("signed", w.cfg.Secret != ""),
		zap.Int("headers", len(w.cfg.Headers)))
	dest, err := w.cfg.Kind.SignedURL(w.cfg.URL, w.cfg.Secret, time.Now())
	if err != nil {
		w.log.Debug("Webhook.post: signing the URL failed", zap.String("host", host), zap.Error(err))
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, dest, bytes.NewReader(body))
	if err != nil {
		w.log.Debug("Webhook.post: building the request failed", zap.String("host", host), zap.Error(err))
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range w.cfg.Headers {
		req.Header.Set(k, v)
	}

	start := time.Now()
	resp, err := w.client.Do(req)
	if err != nil {
		err = redactRequestError(err)
		w.log.Debug("Webhook.post: request failed",
			zap.String("host", host), zap.Duration("took", time.Since(start)), zap.Error(err))
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	w.log.Debug("Webhook.post: receiver answered",
		zap.String("host", host), zap.Int("status", resp.StatusCode), zap.Duration("took", time.Since(start)))

	// 4xx other than 408/429 will not succeed on retry either, but reporting
	// them as errors is still right: the caller logs and moves on, and the
	// operator needs to see that their receiver is rejecting us.
	if resp.StatusCode >= 400 {
		w.log.Debug("Webhook.post: receiver rejected the request", zap.String("host", host), zap.Int("status", resp.StatusCode))
		return fmt.Errorf("receiver returned HTTP %d", resp.StatusCode)
	}

	// The body has to be read even on a 200. Feishu, WeCom and DingTalk all
	// answer 200 for a message they refused, putting the reason in the body;
	// stopping at the status code would report every one of those as delivered.
	// Bounded because a misconfigured URL can point at anything at all.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	if err != nil {
		w.log.Debug("Webhook.post: reading the reply failed", zap.String("host", host), zap.Error(err))
		return fmt.Errorf("read the receiver's reply: %w", err)
	}
	err = w.cfg.Kind.CheckResponse(raw)
	w.log.Debug("Webhook.post: reply checked",
		zap.String("host", host), zap.Int("reply_bytes", len(raw)), zap.Error(err))
	return err
}
