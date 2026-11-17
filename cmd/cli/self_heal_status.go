package main

import (
	"context"
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"go.uber.org/zap"
	"google.golang.org/protobuf/encoding/protojson"

	haifypb "github.com/haify-project/haify/api/proto/v1"
)

// `haify self-heal status`: what auto-evict and auto-diskful are doing and
// waiting for (docs/user-guide.md, "Self-healing").

func selfHealCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "self-heal",
		Short: "Self-healing: what it is doing and waiting for",
	}
	cmd.AddCommand(selfHealStatusCommand())
	return cmd
}

func selfHealStatusCommand() *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show offline nodes awaiting eviction, replica moves, and the diskless clients auto-diskful watches",
		Long: `Show what self-healing is doing right now.

Auto-evict: each node that does not answer, how long until its replicas are
replaced, and what holds that back.

Replica moves: each move under way and how far the new replica has synced.

Auto-diskful: each node using a volume as a diskless client, how long it has
used it without a break and how long is left before it gets a replica of its
own; why one is passed over; the swaps in progress; and the resources whose
replica moved to its user and will not move again before the cooldown ends.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			log := zap.L()
			log.Debug("selfHealStatusCommand: running command", zap.Bool("json", jsonOut))
			return withController(time.Minute, func(ctx context.Context, c haifypb.HaifyControllerClient) error {
				r, err := c.GetSelfHealStatus(ctx, &haifypb.GetSelfHealStatusRequest{})
				if err != nil {
					log.Debug("selfHealStatusCommand: GetSelfHealStatus failed", zap.Error(err))
					return err
				}
				if !r.GetSuccess() {
					log.Debug("selfHealStatusCommand: controller reported failure", zap.String("message", r.GetMessage()))
					return fmt.Errorf("%s", r.GetMessage())
				}
				log.Debug("selfHealStatusCommand: status", zap.Int("offline_nodes", len(r.GetAutoEvict().GetNodes())),
					zap.Int("moves", len(r.GetMoves())), zap.Int("clients", len(r.GetAutoDiskful().GetClients())),
					zap.Int("swaps", len(r.GetAutoDiskful().GetSwaps())))
				out := cmd.OutOrStdout()
				if jsonOut {
					raw, merr := protojson.MarshalOptions{Multiline: true, EmitUnpopulated: true}.Marshal(r)
					if merr != nil {
						return merr
					}
					_, _ = fmt.Fprintln(out, string(raw))
					return nil
				}
				printSelfHealStatus(out, r, time.Now())
				return nil
			})
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "print the status as JSON")
	return cmd
}

// printSelfHealStatus writes the status for a person. Output is best-effort,
// as everywhere in this CLI: a closed pipe is not a failed command.
func printSelfHealStatus(out io.Writer, r *haifypb.GetSelfHealStatusResponse, now time.Time) {
	if r.GetMessage() != "" {
		_, _ = fmt.Fprintln(out, "note:", r.GetMessage())
	}
	ev := r.GetAutoEvict()
	_, _ = fmt.Fprintf(out, "Auto-evict: %s", ev.GetMode())
	if ev.GetMode() != "off" {
		_, _ = fmt.Fprintf(out, " (after %d min offline, at most %d%% of the nodes offline)", ev.GetAfterMinutes(), ev.GetMaxOfflinePercent())
	}
	_, _ = fmt.Fprintln(out)
	if ev.GetGate() != "" {
		_, _ = fmt.Fprintln(out, "  held back:", ev.GetGate())
	}
	if ev.GetSyncing() != "" {
		_, _ = fmt.Fprintf(out, "  the replacement replica of %s is syncing; the next replacement waits for it\n", ev.GetSyncing())
	}
	if len(ev.GetNodes()) > 0 {
		w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
		_, _ = fmt.Fprintln(w, "  NODE\tSTATE\tOFFLINE FOR\tEVICTED IN\tREPLICAS\tREASON")
		for _, n := range ev.GetNodes() {
			_, _ = fmt.Fprintf(w, "  %s\t%s\t%s\t%s\t%d\t%s\n", n.GetNode(), n.GetState(), sinceUnix(n.GetOfflineSince(), now),
				remaining(n.GetState() == "waiting", n.GetRemainingSeconds()), n.GetReplicas(), n.GetReason())
		}
		_ = w.Flush()
	} else if ev.GetMode() != "off" {
		_, _ = fmt.Fprintln(out, "  every node answers")
	}

	_, _ = fmt.Fprintln(out)
	if len(r.GetMoves()) == 0 {
		_, _ = fmt.Fprintln(out, "Replica moves: none")
	} else {
		_, _ = fmt.Fprintln(out, "Replica moves:")
		w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
		_, _ = fmt.Fprintln(w, "  RESOURCE\tFROM\tTO\tNEW REPLICA")
		for _, m := range r.GetMoves() {
			_, _ = fmt.Fprintf(w, "  %s\t%s\t%s\t%s\n", m.GetResource(), m.GetFrom(), m.GetTo(), syncText(m.GetState(), m.SyncPercent))
		}
		_ = w.Flush()
	}

	df := r.GetAutoDiskful()
	_, _ = fmt.Fprintln(out)
	_, _ = fmt.Fprintf(out, "Auto-diskful: %s", df.GetMode())
	if df.GetMode() != "off" {
		_, _ = fmt.Fprintf(out, " (after %d min of uninterrupted use, then %d h before the same resource moves again)",
			df.GetAfterMinutes(), df.GetCooldownHours())
	}
	_, _ = fmt.Fprintln(out)
	if df.GetMode() == "off" {
		return
	}
	if len(df.GetClients()) == 0 {
		_, _ = fmt.Fprintln(out, "  no diskless client is in use")
	} else {
		w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
		_, _ = fmt.Fprintln(w, "  RESOURCE\tCLIENT\tIN USE FOR\tREPLICA IN\tSTATE\tREASON")
		for _, c := range df.GetClients() {
			reason := c.GetReason()
			if c.GetRetryAt() != 0 {
				reason += " (next try " + time.Unix(c.GetRetryAt(), 0).UTC().Format(time.RFC3339) + ")"
			}
			_, _ = fmt.Fprintf(w, "  %s\t%s\t%s\t%s\t%s\t%s\n", c.GetResource(), c.GetNode(),
				shortDuration(time.Duration(c.GetInUseSeconds())*time.Second),
				remaining(c.GetState() == "counting", c.GetRemainingSeconds()), c.GetState(), reason)
		}
		_ = w.Flush()
	}
	if len(df.GetSwaps()) > 0 {
		_, _ = fmt.Fprintln(out, "  Swaps in progress:")
		for _, sw := range df.GetSwaps() {
			_, _ = fmt.Fprintf(out, "    %s: replica on %s, %s; %d of %d replicas", sw.GetResource(), sw.GetNode(),
				swapPhase(sw), sw.GetReplicasNow(), sw.GetReplicasTarget())
			if sw.GetStartedAt() != 0 {
				_, _ = fmt.Fprintf(out, "; started %s ago", sinceUnix(sw.GetStartedAt(), now))
			}
			_, _ = fmt.Fprintln(out)
			if sw.GetDetail() != "" {
				_, _ = fmt.Fprintln(out, "      waiting:", sw.GetDetail())
			}
			if sw.GetNextTry() != 0 {
				_, _ = fmt.Fprintf(out, "      attempt %d failed; next try %s\n", sw.GetAttempts(),
					time.Unix(sw.GetNextTry(), 0).UTC().Format(time.RFC3339))
			}
		}
	}
	if len(df.GetCooldowns()) > 0 {
		_, _ = fmt.Fprintln(out, "  Cooling down (replica already moved to its user):")
		for _, cd := range df.GetCooldowns() {
			_, _ = fmt.Fprintf(out, "    %s: moved %s ago, no further move before %s\n", cd.GetResource(),
				sinceUnix(cd.GetMovedAt(), now), time.Unix(cd.GetUntil(), 0).UTC().Format(time.RFC3339))
		}
	}
}

// swapPhase says where a swap is.
func swapPhase(sw *haifypb.AutoDiskfulSwap) string {
	switch sw.GetPhase() {
	case "syncing":
		return syncText("syncing", sw.SyncPercent)
	case "removing":
		return "in sync, an old replica is to be removed"
	case "tiebreaker":
		return fmt.Sprintf("in sync, the replica on %s is removed and that node is being kept as tiebreaker", sw.GetRemoved())
	}
	return sw.GetPhase()
}

// syncText is a replica's state with its sync progress when DRBD gave one.
func syncText(state string, percent *float64) string {
	if state == "" {
		state = "state unknown"
	}
	if percent != nil {
		return fmt.Sprintf("%s (%.1f%%)", state, *percent)
	}
	return state
}

// remaining is a countdown, "now" when it has run out and "-" when nothing
// is counting.
func remaining(counting bool, seconds int64) string {
	switch {
	case counting && seconds > 0:
		return shortDuration(time.Duration(seconds) * time.Second)
	case counting:
		return "now"
	}
	return "-"
}

// sinceUnix is how long ago a Unix time was, "-" for none.
func sinceUnix(sec int64, now time.Time) string {
	if sec == 0 {
		return "-"
	}
	return shortDuration(now.Sub(time.Unix(sec, 0)))
}

// shortDuration renders a duration to the minute: "3d4h", "2h05m", "7m".
func shortDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	d = d.Round(time.Minute)
	days, hours, mins := int(d.Hours())/24, int(d.Hours())%24, int(d.Minutes())%60
	switch {
	case days > 0:
		return fmt.Sprintf("%dd%dh", days, hours)
	case hours > 0:
		return fmt.Sprintf("%dh%02dm", hours, mins)
	}
	return fmt.Sprintf("%dm", mins)
}
