package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	haifypb "github.com/haify-project/haify/api/proto/v1"
	"github.com/spf13/cobra"
	"go.uber.org/zap"
)

// `haify ha evict`, plain and --live, and `haify ha migration`, which shows
// a live move that is running or the last one that ran.

func haEvict() *cobra.Command {
	var live, noWait bool
	var to string
	var timeout time.Duration

	cmd := &cobra.Command{
		Use:   "evict <resource>",
		Short: "Move an HA resource off its active node",
		Long: "Move an HA resource to another replica: its services stop on the active node and\n" +
			"start on the node that takes over.\n\n" +
			"--live moves a libvirt guest (ha create --vm) without stopping it: the guest is\n" +
			"migrated to another replica while it runs, and drbd-reactor manages it there\n" +
			"afterwards. --to names the node; without it Haify picks a replica that is UpToDate,\n" +
			"connected and has the guest's definition. If the move cannot complete, the guest\n" +
			"stays, running, where it was.\n\n" +
			"  haify ha evict web1                  # restart the guest on another replica\n" +
			"  haify ha evict web1 --live           # move it without stopping it\n" +
			"  haify ha evict web1 --live --to kvm3",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			log := zap.L()
			log.Debug("haEvict: running command", zap.Strings("args", args), zap.Bool("live", live), zap.String("to", to),
				zap.Duration("timeout", timeout), zap.Bool("no_wait", noWait))
			resource := args[0]
			if !live && (to != "" || noWait || cmd.Flags().Changed("timeout")) {
				log.Debug("haEvict: live-only flags without --live")
				return fmt.Errorf("--to, --timeout and --no-wait belong to --live; without it drbd-reactor decides which replica takes over")
			}

			haifyClient, err := newHaifyClient()
			if err != nil {
				log.Debug("haEvict: newHaifyClient failed", zap.Error(err))
				return fmt.Errorf("failed to connect to controller: %w", err)
			}
			defer closeClient(haifyClient)

			if !live {
				// drbd-reactor stops the services, which may take a unit's
				// whole stop timeout, then waits for a peer to take over.
				ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
				defer cancel()
				if err := haifyClient.EvictHa(ctx, resource); err != nil {
					log.Debug("haEvict: EvictHa failed", zap.Error(err))
					return fmt.Errorf("failed to evict HA resource: %w", err)
				}
				fmt.Printf("HA resource evicted successfully\n")
				fmt.Printf("  Resource: %s\n", resource)
				log.Debug("haEvict: done")
				return nil
			}

			// Starting checks every replica and defines the guest where it
			// is missing, before anything is changed.
			startCtx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()
			started, err := haifyClient.EvictHaLive(startCtx, resource, to, timeout)
			if err != nil {
				log.Debug("haEvict: EvictHaLive failed", zap.Error(err))
				return fmt.Errorf("live move not started: %w", err)
			}
			fmt.Printf("Live move started: %s -> %s\n", started.GetSourceNode(), started.GetTargetNode())
			if noWait {
				log.Debug("haEvict: not waiting")
				fmt.Printf("Follow it with: haify ha migration %s\n", resource)
				return nil
			}
			// The move ends by itself at its own deadline, with time to put
			// things back after; this only bounds a controller that vanished.
			wait := timeout
			if wait <= 0 {
				wait = 30 * time.Minute
			}
			waitCtx, cancelWait := context.WithTimeout(context.Background(), wait+15*time.Minute)
			defer cancelWait()
			m, err := haifyClient.WaitHaMigration(waitCtx, resource, 2*time.Second, liveProgressPrinter(os.Stdout))
			if err != nil {
				log.Debug("haEvict: WaitHaMigration failed", zap.Error(err))
				return fmt.Errorf("lost track of the move (it continues on the nodes); `haify ha migration %s` shows it: %w", resource, err)
			}
			log.Debug("haEvict: live move ended", zap.String("outcome", m.GetOutcome()), zap.String("active_node", m.GetActiveNode()))
			return liveOutcomeError(m)
		},
	}

	cmd.Flags().BoolVar(&live, "live", false, "Move a libvirt guest without stopping it")
	cmd.Flags().StringVar(&to, "to", "", "With --live: the node to move the guest to")
	cmd.Flags().DurationVar(&timeout, "timeout", 0, "With --live: give up when the guest's memory is not copied after this long (default 30m)")
	cmd.Flags().BoolVar(&noWait, "no-wait", false, "With --live: return once the move has started")
	return cmd
}

// liveProgressPrinter prints each new step of a live move, and the copy's
// progress while it runs.
func liveProgressPrinter(w io.Writer) func(*haifypb.HaMigration) {
	printed, lastPct := 0, uint32(0)
	return func(m *haifypb.HaMigration) {
		steps := m.GetSteps()
		for ; printed < len(steps); printed++ {
			_, _ = fmt.Fprintf(w, "  %s\n", steps[printed])
		}
		if p := m.GetProgressPercent(); m.GetPhase() == "migrating" && p >= lastPct+5 {
			lastPct = p
			_, _ = fmt.Fprintf(w, "  memory copied: %d%%\n", p)
		}
	}
}

// liveOutcomeError turns how a live move ended into the command's result:
// only a guest that moved is a success.
func liveOutcomeError(m *haifypb.HaMigration) error {
	switch m.GetOutcome() {
	case "moved":
		return nil
	case "stayed":
		return fmt.Errorf("the guest was not moved; it is still running on %s", m.GetActiveNode())
	case "restarted":
		return fmt.Errorf("the guest was not moved live; it was restarted and runs on %s", m.GetActiveNode())
	}
	return fmt.Errorf("the move needs attention; see the last line above")
}

func haMigration() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "migration <resource>",
		Short: "Show the live move of an HA guest: the one running, or the last one",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			log := zap.L()
			log.Debug("haMigration: running command", zap.Strings("args", args))
			resource := args[0]
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			haifyClient, err := newHaifyClient()
			if err != nil {
				log.Debug("haMigration: newHaifyClient failed", zap.Error(err))
				return fmt.Errorf("failed to connect to controller: %w", err)
			}
			defer closeClient(haifyClient)

			m, err := haifyClient.GetHaMigration(ctx, resource)
			if err != nil {
				log.Debug("haMigration: GetHaMigration failed", zap.Error(err))
				return fmt.Errorf("failed to get the live move: %w", err)
			}
			if m == nil {
				log.Debug("haMigration: none")
				fmt.Printf("No live move of %s has run\n", resource)
				return nil
			}
			printHaMigration(os.Stdout, m)
			log.Debug("haMigration: done", zap.String("phase", m.GetPhase()), zap.Bool("done", m.GetDone()))
			return nil
		},
	}
	return cmd
}

// printHaMigration renders one live move.
func printHaMigration(w io.Writer, m *haifypb.HaMigration) {
	_, _ = fmt.Fprintf(w, "Live move of %s (%s)\n", m.GetDomain(), m.GetResource())
	_, _ = fmt.Fprintf(w, "  From:     %s\n", m.GetSourceNode())
	_, _ = fmt.Fprintf(w, "  To:       %s\n", m.GetTargetNode())
	_, _ = fmt.Fprintf(w, "  Started:  %s\n", time.Unix(m.GetStartedUnix(), 0).Format(time.RFC3339))
	if m.GetDone() {
		_, _ = fmt.Fprintf(w, "  Result:   %s\n", m.GetOutcome())
		if m.GetActiveNode() != "" {
			_, _ = fmt.Fprintf(w, "  Runs on:  %s\n", m.GetActiveNode())
		}
	} else {
		_, _ = fmt.Fprintf(w, "  Phase:    %s\n", m.GetPhase())
		if m.GetPhase() == "migrating" {
			_, _ = fmt.Fprintf(w, "  Copied:   %d%%\n", m.GetProgressPercent())
		}
	}
	_, _ = fmt.Fprintf(w, "  Steps:\n")
	for _, s := range m.GetSteps() {
		_, _ = fmt.Fprintf(w, "    - %s\n", s)
	}
}
