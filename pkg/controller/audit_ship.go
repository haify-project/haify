package controller

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/haify-project/haify/pkg/config"
	"github.com/haify-project/haify/pkg/database"
	"github.com/haify-project/haify/pkg/event"
	"go.uber.org/zap"
)

// Sending the audit trail off the cluster ([audit] syslog, webhook_url).
//
// The trail in the controller database answers "who did what" until someone
// who holds the controller rewrites the database. A copy on a log server or an
// object store with retention, which that someone does not hold, still
// answers it afterwards. The trail is the outbox (pkg/database/audit_ship.go):
// the shipper sends every entry after a destination's cursor, in order, and
// moves the cursor only once the destination took the batch — so a
// destination that is down receives everything when it is back, and a
// controller that takes over resumes where the last one stopped.

const (
	auditShipInterval = 2 * time.Second
	auditShipBatch    = 200
	// auditShipAlarmAfter is how long a destination may fail before it is an
	// event: a restart of the log server is not news, an hour of it is.
	auditShipAlarmAfter = 5 * time.Minute
	auditPruneInterval  = time.Hour
)

// auditDestination is somewhere the trail is sent.
type auditDestination interface {
	// Name keys the destination's cursor; changing it resends everything.
	Name() string
	Send(ctx context.Context, records []database.AuditRecord) error
	Close()
}

// auditDestinations builds what [audit] names.
func auditDestinations(cfg config.AuditConfig) ([]auditDestination, error) {
	log := zap.L()
	log.Debug("auditDestinations: building audit destinations", zap.Bool("has_webhook_url", cfg.WebhookURL != ""),
		zap.Bool("has_webhook_token", cfg.WebhookToken != ""), zap.String("syslog_ca", cfg.SyslogCA))
	var out []auditDestination
	network, addr, err := cfg.SyslogTarget()
	if err != nil {
		log.Debug("auditDestinations: syslog target invalid", zap.Error(err))
		return nil, err
	}
	if network != "" {
		log.Debug("auditDestinations: syslog destination", zap.String("network", network), zap.String("addr", addr))
		d := &syslogDestination{network: network, addr: addr, host: localNode()}
		if network == "tls" {
			d.tlsConfig = &tls.Config{MinVersion: tls.VersionTLS12}
			if cfg.SyslogCA != "" {
				start := time.Now()
				pem, err := os.ReadFile(cfg.SyslogCA)
				log.Debug("auditDestinations: read syslog CA", zap.String("path", cfg.SyslogCA),
					zap.Int("size_bytes", len(pem)), zap.Duration("took", time.Since(start)), zap.Error(err))
				if err != nil {
					return nil, fmt.Errorf("audit.syslog_ca: %w", err)
				}
				pool := x509.NewCertPool()
				if !pool.AppendCertsFromPEM(pem) {
					log.Debug("auditDestinations: syslog CA holds no PEM certificate", zap.String("path", cfg.SyslogCA))
					return nil, fmt.Errorf("audit.syslog_ca %s holds no PEM certificate", cfg.SyslogCA)
				}
				d.tlsConfig.RootCAs = pool
			}
		}
		out = append(out, d)
	} else {
		log.Debug("auditDestinations: no syslog destination")
	}
	if u := strings.TrimSpace(cfg.WebhookURL); u != "" {
		log.Debug("auditDestinations: webhook destination", zap.Bool("has_token", cfg.WebhookToken != ""))
		out = append(out, &webhookDestination{url: u, token: cfg.WebhookToken,
			client: &http.Client{Timeout: 15 * time.Second}})
	} else {
		log.Debug("auditDestinations: no webhook destination")
	}
	log.Debug("auditDestinations: destinations built", zap.Int("count", len(out)))
	return out, nil
}

// syslogDestination sends one RFC 5424 message per entry, facility "log
// audit" (13), newline-framed over TCP (RFC 6587) or one datagram each over
// UDP. UDP can lose messages without telling anyone; use tcp or tls for a
// trail that has to be complete.
type syslogDestination struct {
	network, addr, host string
	tlsConfig           *tls.Config

	mu   sync.Mutex
	conn net.Conn
}

func (d *syslogDestination) Name() string { return "syslog:" + d.network + "://" + d.addr }

func (d *syslogDestination) dial(ctx context.Context) (net.Conn, error) {
	log := zap.L()
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	start := time.Now()
	switch d.network {
	case "tls":
		cfg := d.tlsConfig.Clone()
		if cfg.ServerName == "" {
			cfg.ServerName, _, _ = net.SplitHostPort(d.addr)
		}
		conn, err := (&tls.Dialer{NetDialer: dialer, Config: cfg}).DialContext(ctx, "tcp", d.addr)
		log.Debug("syslogDestination.dial: dialed over TLS", zap.String("addr", d.addr),
			zap.String("server_name", cfg.ServerName), zap.Duration("took", time.Since(start)), zap.Error(err))
		return conn, err
	default:
		conn, err := dialer.DialContext(ctx, d.network, d.addr)
		log.Debug("syslogDestination.dial: dialed", zap.String("network", d.network), zap.String("addr", d.addr),
			zap.Duration("took", time.Since(start)), zap.Error(err))
		return conn, err
	}
}

func (d *syslogDestination) Send(ctx context.Context, records []database.AuditRecord) error {
	log := zap.L()
	log.Debug("syslogDestination.Send: sending batch", zap.String("destination", d.Name()), zap.Int("count", len(records)))
	start := time.Now()
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.conn == nil {
		log.Debug("syslogDestination.Send: no connection, dialing", zap.String("destination", d.Name()))
		conn, err := d.dial(ctx)
		if err != nil {
			log.Debug("syslogDestination.Send: dial failed", zap.String("destination", d.Name()), zap.Error(err))
			return err
		}
		d.conn = conn
	}
	for _, r := range records {
		msg, err := syslogMessage(d.host, r)
		if err != nil {
			log.Debug("syslogDestination.Send: rendering entry failed", zap.Uint64("seq", r.Seq), zap.Error(err))
			return err
		}
		_ = d.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
		if _, err := d.conn.Write(msg); err != nil {
			// The batch is resent whole on a new connection: a receiver may
			// see an entry twice, never miss one. The sequence number in each
			// message lets it drop the duplicates.
			log.Debug("syslogDestination.Send: write failed, dropping connection",
				zap.String("destination", d.Name()), zap.Uint64("seq", r.Seq), zap.Error(err))
			_ = d.conn.Close()
			d.conn = nil
			return err
		}
	}
	log.Debug("syslogDestination.Send: batch sent", zap.String("destination", d.Name()),
		zap.Int("count", len(records)), zap.Duration("took", time.Since(start)))
	return nil
}

func (d *syslogDestination) Close() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.conn != nil {
		zap.L().Debug("syslogDestination.Close: closing connection", zap.String("destination", d.Name()))
		_ = d.conn.Close()
		d.conn = nil
	}
}

// syslogMessage renders one entry. The message body is the entry as JSON,
// sequence number included, so a receiver can tell a gap from a quiet hour.
func syslogMessage(host string, r database.AuditRecord) ([]byte, error) {
	body, err := json.Marshal(r)
	if err != nil {
		zap.L().Debug("syslogMessage: marshal failed", zap.Uint64("seq", r.Seq), zap.Error(err))
		return nil, err
	}
	severity := 6 // informational
	switch {
	case !r.Event.Granted:
		severity = 4 // warning: someone was refused
	case r.Event.Result != "OK":
		severity = 5 // notice: the call failed
	}
	if host == "" {
		host = "-"
	}
	return []byte(fmt.Sprintf("<%d>1 %s %s haify-controller - audit - %s\n",
		13*8+severity, r.Event.Timestamp.UTC().Format("2006-01-02T15:04:05.000000Z07:00"), host, body)), nil
}

// webhookDestination POSTs each batch as {"records": [...]}; any 2xx takes it.
type webhookDestination struct {
	url, token string
	client     *http.Client
}

func (d *webhookDestination) Name() string { return "webhook:" + event.RedactURL(d.url) }

func (d *webhookDestination) Send(ctx context.Context, records []database.AuditRecord) error {
	log := zap.L()
	log.Debug("webhookDestination.Send: sending batch", zap.Int("count", len(records)), zap.Bool("has_token", d.token != ""))
	body, err := json.Marshal(map[string]any{"source": "haify-controller", "records": records})
	if err != nil {
		log.Debug("webhookDestination.Send: marshal failed", zap.Error(err))
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.url, bytes.NewReader(body))
	if err != nil {
		log.Debug("webhookDestination.Send: building request failed", zap.Error(err))
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if d.token != "" {
		req.Header.Set("Authorization", "Bearer "+d.token)
	}
	start := time.Now()
	resp, err := d.client.Do(req)
	if err != nil {
		err = event.RedactRequestError(err)
		log.Debug("webhookDestination.Send: POST failed", zap.String("host", req.URL.Host),
			zap.Duration("took", time.Since(start)), zap.Error(err))
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	log.Debug("webhookDestination.Send: POST answered", zap.String("host", req.URL.Host),
		zap.Int("status", resp.StatusCode), zap.Int("size_bytes", len(body)), zap.Duration("took", time.Since(start)))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("%s answered HTTP %d", d.url, resp.StatusCode)
	}
	return nil
}

func (d *webhookDestination) Close() {}

// auditShipper sends the trail to its destinations and applies retention.
type auditShipper struct {
	db     *database.DB
	dests  []auditDestination
	events *event.Bus
	log    *zap.Logger

	failingSince map[string]time.Time
	alarmed      map[string]bool
}

// startAuditShipper runs the shipper until ctx ends. Retention runs even with
// no destination configured.
func (c *Controller) startAuditShipper(ctx context.Context) {
	log := c.traceLogger()
	if c.db == nil {
		log.Debug("startAuditShipper: no database, audit shipper not started")
		return
	}
	dests, err := auditDestinations(c.config.Audit)
	if err != nil {
		// Validate already checked the addresses; this is a CA file that
		// could not be read, which must not stop the controller.
		c.logger.Error("Audit trail will not be sent off the cluster", zap.Error(err))
	}
	s := &auditShipper{db: c.db, dests: dests, events: c.events, log: log,
		failingSince: map[string]time.Time{}, alarmed: map[string]bool{}}
	log.Debug("startAuditShipper: starting audit shipper", zap.Int("destinations", len(dests)))
	go s.run(ctx)
}

func (s *auditShipper) run(ctx context.Context) {
	s.log.Debug("auditShipper.run: shipper loop started", zap.Int("destinations", len(s.dests)))
	defer func() {
		for _, d := range s.dests {
			s.log.Debug("auditShipper.run: closing destination", zap.String("destination", d.Name()))
			d.Close()
		}
	}()
	ship := time.NewTicker(auditShipInterval)
	defer ship.Stop()
	nextPrune := time.Now()
	for {
		tick := time.Now()
		s.log.Debug("auditShipper.run: tick", zap.Int("destinations", len(s.dests)))
		for _, d := range s.dests {
			s.drain(ctx, d)
		}
		if now := time.Now(); !now.Before(nextPrune) {
			s.log.Debug("auditShipper.run: prune due")
			s.prune(ctx)
			nextPrune = now.Add(auditPruneInterval)
		}
		if n := s.db.TakeAuditTruncated(); n > 0 {
			s.log.Debug("auditShipper.run: entries truncated by max_entries", zap.Uint64("count", n))
			s.publish(event.TypeAuditTruncated, event.StatusInfo, event.SeverityCritical, "",
				fmt.Sprintf("%d audit entries younger than [audit] retention_days were dropped to stay under max_entries; "+
					"raise max_entries, or look for whatever is flooding the API", n))
		}
		s.log.Debug("auditShipper.run: tick done", zap.Duration("took", time.Since(tick)))
		select {
		case <-ctx.Done():
			s.log.Debug("auditShipper.run: context done, stopping", zap.Error(ctx.Err()))
			return
		case <-ship.C:
		}
	}
}

// drain sends everything after d's cursor, a batch at a time.
func (s *auditShipper) drain(ctx context.Context, d auditDestination) {
	name := d.Name()
	s.log.Debug("auditShipper.drain: draining destination", zap.String("destination", name))
	for ctx.Err() == nil {
		start := time.Now()
		cursor, err := s.db.AuditShipCursor(ctx, name)
		if err != nil {
			s.log.Debug("auditShipper.drain: reading cursor failed", zap.String("destination", name),
				zap.Duration("took", time.Since(start)), zap.Error(err))
			s.failed(d, err)
			return
		}
		records, err := s.db.AuditEventsAfter(ctx, cursor, auditShipBatch)
		s.log.Debug("auditShipper.drain: read entries after cursor", zap.String("destination", name),
			zap.Uint64("cursor", cursor), zap.Int("count", len(records)), zap.Duration("took", time.Since(start)), zap.Error(err))
		if err != nil {
			s.failed(d, err)
			return
		}
		if len(records) == 0 {
			s.log.Debug("auditShipper.drain: destination caught up", zap.String("destination", name))
			s.recovered(d)
			return
		}
		start = time.Now()
		if err := d.Send(ctx, records); err != nil {
			s.log.Debug("auditShipper.drain: send failed", zap.String("destination", name),
				zap.Int("count", len(records)), zap.Duration("took", time.Since(start)), zap.Error(err))
			s.failed(d, err)
			return
		}
		last := records[len(records)-1].Seq
		if err := s.db.SetAuditShipCursor(ctx, name, last); err != nil {
			s.log.Debug("auditShipper.drain: moving cursor failed", zap.String("destination", name),
				zap.Uint64("cursor", last), zap.Error(err))
			s.failed(d, err)
			return
		}
		s.log.Debug("auditShipper.drain: batch shipped", zap.String("destination", name),
			zap.Int("count", len(records)), zap.Uint64("cursor", last), zap.Duration("took", time.Since(start)))
	}
	s.log.Debug("auditShipper.drain: context done", zap.String("destination", name), zap.Error(ctx.Err()))
}

func (s *auditShipper) failed(d auditDestination, err error) {
	since, ok := s.failingSince[d.Name()]
	s.log.Debug("auditShipper.failed: destination failing", zap.String("destination", d.Name()),
		zap.Bool("already_failing", ok), zap.Bool("alarmed", s.alarmed[d.Name()]), zap.Error(err))
	if !ok {
		since = time.Now()
		s.failingSince[d.Name()] = since
		s.log.Warn("Sending the audit trail failed; retrying", zap.String("destination", d.Name()), zap.Error(err))
	}
	if !s.alarmed[d.Name()] && time.Since(since) >= auditShipAlarmAfter {
		s.log.Debug("auditShipper.failed: failing long enough to alarm", zap.String("destination", d.Name()),
			zap.Duration("failing_for", time.Since(since)))
		s.alarmed[d.Name()] = true
		s.publish(event.TypeAuditShippingFailed, event.StatusFiring, event.SeverityWarning, d.Name(),
			fmt.Sprintf("the audit trail has not reached %s since %s: %v; entries wait in the controller database "+
				"for [audit] retention_days", d.Name(), since.UTC().Format(time.RFC3339), err))
	}
}

func (s *auditShipper) recovered(d auditDestination) {
	if _, ok := s.failingSince[d.Name()]; !ok {
		return
	}
	s.log.Debug("auditShipper.recovered: destination recovered", zap.String("destination", d.Name()),
		zap.Bool("alarmed", s.alarmed[d.Name()]))
	delete(s.failingSince, d.Name())
	s.log.Info("Audit trail caught up", zap.String("destination", d.Name()))
	if s.alarmed[d.Name()] {
		delete(s.alarmed, d.Name())
		s.publish(event.TypeAuditShippingFailed, event.StatusResolved, event.SeverityWarning, d.Name(),
			fmt.Sprintf("the audit trail reaches %s again and has caught up", d.Name()))
	}
}

// prune drops entries past their retention, a bounded batch per transaction
// so the request path is never held up behind one long delete.
func (s *auditShipper) prune(ctx context.Context) {
	for ctx.Err() == nil {
		start := time.Now()
		n, err := s.db.PruneExpiredAudit(ctx, 5000)
		if err != nil {
			s.log.Warn("Pruning the audit trail failed", zap.Error(err))
			return
		}
		s.log.Debug("auditShipper.prune: pruned batch", zap.Int("count", n), zap.Duration("took", time.Since(start)))
		if n < 5000 {
			return
		}
	}
}

func (s *auditShipper) publish(t event.Type, st event.Status, sev event.Severity, dest, msg string) {
	s.log.Warn(msg)
	if s.events == nil {
		s.log.Debug("auditShipper.publish: no event bus, event not published", zap.String("type", string(t)))
		return
	}
	s.log.Debug("auditShipper.publish: publishing event", zap.String("type", string(t)),
		zap.String("status", string(st)), zap.String("destination", dest))
	e := event.Event{Type: t, Severity: sev, Status: st, Message: msg}
	if dest != "" {
		e.Details = map[string]string{"destination": dest}
	}
	s.events.Publish(e)
}
