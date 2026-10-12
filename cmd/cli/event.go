package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"go.uber.org/zap"

	haifypb "github.com/haify-project/haify/api/proto/v1"
)

func eventCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "event",
		Aliases: []string{"events", "alert", "alerts"},
		Short:   "Show and follow cluster notifications (degrade, failover, node loss)",
	}
	cmd.AddCommand(eventListCommand(), eventWatchCommand())
	return cmd
}

// eventFilterFlags are shared by list and watch so the two accept the same
// vocabulary; an operator who narrowed a listing can reuse the flags to follow
// the same subset live.
type eventFilterFlags struct {
	minSeverity string
	types       []string
	resource    string
	jsonOut     bool
}

func (f *eventFilterFlags) register(cmd *cobra.Command) {
	cmd.Flags().StringVar(&f.minSeverity, "min-severity", "", "only info|warning|critical and above")
	cmd.Flags().StringSliceVar(&f.types, "type", nil,
		"only these event types, e.g. resource.failover,node.unreachable (repeatable)")
	cmd.Flags().StringVar(&f.resource, "resource", "", "only events for this resource")
	cmd.Flags().BoolVar(&f.jsonOut, "json", false, "emit one JSON object per line")
}

func eventListCommand() *cobra.Command {
	var f eventFilterFlags
	var limit int32
	var sinceID uint64

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List retained notifications, oldest first",
		RunE: func(cmd *cobra.Command, _ []string) error {
			log := zap.L()
			log.Debug("eventListCommand: running command", zap.String("min_severity", f.minSeverity), zap.Strings("types", f.types),
				zap.String("resource", f.resource), zap.Bool("json", f.jsonOut), zap.Int32("limit", limit), zap.Uint64("since_id", sinceID))
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			c, err := newHaifyClient()
			if err != nil {
				log.Debug("eventListCommand: newHaifyClient failed", zap.Error(err))
				return fmt.Errorf("failed to connect to controller: %w", err)
			}
			defer closeClient(c)

			resp, err := c.ListEvents(ctx, &haifypb.ListEventsRequest{
				Limit:       limit,
				MinSeverity: f.minSeverity,
				Types:       f.types,
				Resource:    f.resource,
				SinceId:     sinceID,
			})
			if err != nil {
				log.Debug("eventListCommand: ListEvents failed", zap.Error(err))
				return err
			}

			if len(resp.Events) == 0 {
				log.Debug("eventListCommand: no events")
				fmt.Println("No events.")
				return nil
			}
			log.Debug("eventListCommand: printing events", zap.Int("count", len(resp.Events)), zap.Uint64("dropped", resp.Dropped))
			for _, e := range resp.Events {
				printEvent(cmd.OutOrStdout(), e, f.jsonOut)
			}
			if resp.Dropped > 0 {
				// Writes to the command's own output stream are best-effort. The only ways
				// they fail are a closed pipe (`haify ... | head`) or a full disk, neither of
				// which this command can report anywhere the operator is still looking, and
				// treating them as errors would report a successful operation as failed.
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(),
					"\nwarning: %d event(s) were dropped because a subscriber could not keep up\n", resp.Dropped)
			}
			log.Debug("eventListCommand: done")
			return nil
		},
	}
	f.register(cmd)
	cmd.Flags().Int32Var(&limit, "limit", 0, "maximum events to return (0 = server default)")
	cmd.Flags().Uint64Var(&sinceID, "since-id", 0, "only events newer than this id")
	return cmd
}

func eventWatchCommand() *cobra.Command {
	var f eventFilterFlags
	var sinceID uint64
	var replay bool

	cmd := &cobra.Command{
		Use:   "watch",
		Short: "Follow notifications as they happen",
		Long: "Streams cluster notifications until interrupted.\n\n" +
			"By default the stream starts from now. Use --replay to receive the\n" +
			"controller's retained history first, or --since-id to resume from a\n" +
			"known point after a disconnect.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			log := zap.L()
			log.Debug("eventWatchCommand: running command", zap.String("min_severity", f.minSeverity), zap.Strings("types", f.types),
				zap.String("resource", f.resource), zap.Bool("json", f.jsonOut), zap.Uint64("since_id", sinceID), zap.Bool("replay", replay))
			// No timeout: this is meant to run until the operator stops it.
			ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer cancel()

			c, err := newHaifyClient()
			if err != nil {
				log.Debug("eventWatchCommand: newHaifyClient failed", zap.Error(err))
				return fmt.Errorf("failed to connect to controller: %w", err)
			}
			defer closeClient(c)

			// since_id 0 means "replay everything retained", so watching from now
			// requires asking for the newest id first and starting after it.
			if replay {
				log.Debug("eventWatchCommand: replaying retained history")
				sinceID = 0
			} else if sinceID == 0 {
				log.Debug("eventWatchCommand: starting from now, asking for the newest id")
				resp, err := c.ListEvents(ctx, &haifypb.ListEventsRequest{Limit: 1})
				if err != nil {
					log.Debug("eventWatchCommand: ListEvents failed", zap.Error(err))
					return err
				}
				sinceID = resp.Published
				log.Debug("eventWatchCommand: starting after newest id", zap.Uint64("since_id", sinceID))
			}

			stream, err := c.WatchEvents(ctx, &haifypb.WatchEventsRequest{
				MinSeverity: f.minSeverity,
				Types:       f.types,
				Resource:    f.resource,
				SinceId:     sinceID,
			})
			if err != nil {
				log.Debug("eventWatchCommand: WatchEvents failed", zap.Error(err))
				return err
			}

			if !f.jsonOut {
				_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "Watching for events; press Ctrl-C to stop.")
			}
			var lastID uint64
			for {
				e, err := stream.Recv()
				if err == io.EOF || ctx.Err() != nil {
					log.Debug("eventWatchCommand: stream ended", zap.Bool("eof", err == io.EOF), zap.NamedError("ctx_err", ctx.Err()))
					return nil
				}
				if err != nil {
					log.Debug("eventWatchCommand: stream Recv failed", zap.Error(err))
					return fmt.Errorf("event stream closed: %w", err)
				}
				// Ids are monotonic, so a jump means the controller dropped
				// events for this watcher rather than that nothing happened.
				log.Debug("eventWatchCommand: event received", zap.Uint64("id", e.Id), zap.String("type", e.Type), zap.String("severity", e.Severity))
				if lastID != 0 && e.Id > lastID+1 {
					log.Debug("eventWatchCommand: gap in event ids", zap.Uint64("last_id", lastID), zap.Uint64("id", e.Id))
					_, _ = fmt.Fprintf(cmd.ErrOrStderr(),
						"warning: missed %d event(s) — this client fell behind\n", e.Id-lastID-1)
				}
				lastID = e.Id
				printEvent(cmd.OutOrStdout(), e, f.jsonOut)
			}
		},
	}
	f.register(cmd)
	cmd.Flags().Uint64Var(&sinceID, "since-id", 0, "resume after this event id")
	cmd.Flags().BoolVar(&replay, "replay", false, "deliver retained history before live events")
	return cmd
}

func printEvent(w io.Writer, e *haifypb.Event, asJSON bool) {
	if asJSON {
		body, err := json.Marshal(map[string]any{
			"id":        e.Id,
			"type":      e.Type,
			"severity":  e.Severity,
			"status":    e.Status,
			"resource":  e.Resource,
			"node":      e.Node,
			"message":   e.Message,
			"details":   e.Details,
			"timestamp": time.UnixMilli(e.TimestampUnixMs).Format(time.RFC3339),
		})
		if err == nil {
			_, _ = fmt.Fprintln(w, string(body))
		} else {
			zap.L().Debug("printEvent: event not marshalable, skipped", zap.Uint64("id", e.Id), zap.Error(err))
		}
		return
	}

	ts := time.UnixMilli(e.TimestampUnixMs).Format("2006-01-02 15:04:05")
	_, _ = fmt.Fprintf(w, "%s  %-8s %-8s %-20s %s\n",
		ts, severityLabel(e.Severity), e.Status, e.Type, e.Message)

	if len(e.Details) > 0 {
		keys := make([]string, 0, len(e.Details))
		for k := range e.Details {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			if v := e.Details[k]; v != "" {
				parts = append(parts, k+"="+v)
			}
		}
		if len(parts) > 0 {
			_, _ = fmt.Fprintf(w, "%s  %s\n", strings.Repeat(" ", 19), strings.Join(parts, "  "))
		}
	}
}

// severityLabel marks the two levels that warrant attention. Plain text, no
// colour: this output is routinely piped into a file or another tool.
func severityLabel(s string) string {
	switch s {
	case "critical":
		return "CRIT"
	case "warning":
		return "WARN"
	default:
		return "info"
	}
}
