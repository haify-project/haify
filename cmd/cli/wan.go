package main

import (
	"context"
	"fmt"
	"time"

	haifypb "github.com/haify-project/haify/api/proto/v1"
	"github.com/spf13/cobra"
	"go.uber.org/zap"
)

func wanCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "wan",
		Short: "Cross-site (WAN) replication maintenance",
	}
	cmd.AddCommand(wanRepairCommand())
	cmd.AddCommand(wanSetEndpointCommand())
	return cmd
}

func wanSetEndpointCommand() *cobra.Command {
	var req haifypb.SetWanEndpointRequest
	cmd := &cobra.Command{
		Use:   "set-endpoint <resource> [--dr-endpoint <address>] [--egress-address <ip> | --clear-egress]",
		Short: "Change where a WAN resource reaches its DR site, and rebuild its tunnels there",
		Long: "The DR endpoint is the DR site's address the primary dials; the egress address\n" +
			"pins the source it dials from. Both are rebuilt into the tunnels at once.\n\n" +
			"A new DR endpoint that does not answer is refused and the old one kept, since\n" +
			"the old tunnel was working. --skip-check saves it anyway, for a DR site whose\n" +
			"firewall is not open yet; run wan repair once it is.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			log := zap.L()
			log.Debug("wanSetEndpointCommand: running command", zap.Strings("args", args), zap.String("dr_endpoint", req.DrEndpoint),
				zap.String("egress_address", req.EgressAddress), zap.Bool("clear_egress", req.ClearEgress), zap.Bool("skip_check", req.SkipReachabilityCheck))
			req.Name = args[0]
			haifyClient, err := newHaifyClient()
			if err != nil {
				log.Debug("wanSetEndpointCommand: newHaifyClient failed", zap.Error(err))
				return fmt.Errorf("failed to connect to controller: %w", err)
			}
			defer closeClient(haifyClient)
			resp, err := haifyClient.SetWanEndpoint(cmd.Context(), &req)
			if err != nil {
				log.Debug("wanSetEndpointCommand: SetWanEndpoint failed", zap.Error(err))
				return err
			}
			fmt.Println(resp.Message)
			log.Debug("wanSetEndpointCommand: done")
			return nil
		},
	}
	cmd.Flags().StringVar(&req.DrEndpoint, "dr-endpoint", "", "DR site's address the primary dials (IP or host name, no port)")
	cmd.Flags().StringVar(&req.EgressAddress, "egress-address", "", "Source address the primary dials from")
	cmd.Flags().BoolVar(&req.ClearEgress, "clear-egress", false, "Let the routing table choose the source address again")
	cmd.Flags().BoolVar(&req.SkipReachabilityCheck, "skip-check", false, "Save and provision even if the DR endpoint does not answer yet")
	return cmd
}

func wanRepairCommand() *cobra.Command {
	var dryRun bool

	cmd := &cobra.Command{
		Use:   "repair <resource>",
		Short: "Reconcile a WAN resource's replication tunnels with its current nodes",
		Long: "Re-provisions the WAN legs a resource should have and removes instances\n" +
			"left behind by a node that was renumbered or removed.\n\n" +
			"Leg names used to embed a node's address, so changing that address\n" +
			"orphaned the tunnel: it kept replicating under the old name while status\n" +
			"reported a phantom outage. Repair converges the two.\n\n" +
			"This restarts the tunnels, so run it with --dry-run first.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			log := zap.L()
			log.Debug("wanRepairCommand: running command", zap.Strings("args", args), zap.Bool("dry_run", dryRun))
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
			defer cancel()

			c, err := newHaifyClient()
			if err != nil {
				log.Debug("wanRepairCommand: newHaifyClient failed", zap.Error(err))
				return fmt.Errorf("failed to connect to controller: %w", err)
			}
			defer closeClient(c)

			resp, err := c.RepairWanProxy(ctx, args[0], dryRun)
			if err != nil {
				log.Debug("wanRepairCommand: RepairWanProxy failed", zap.Error(err))
				return err
			}

			log.Debug("wanRepairCommand: repair result", zap.Strings("expected_legs", resp.ExpectedLegs), zap.Strings("removed_legs", resp.RemovedLegs))
			out := cmd.OutOrStdout()
			// Writes to the command's own output stream are best-effort. The only ways
			// they fail are a closed pipe (`haify ... | head`) or a full disk, neither of
			// which this command can report anywhere the operator is still looking, and
			// treating them as errors would report a successful operation as failed.
			_, _ = fmt.Fprintln(out, "Expected legs:")
			for _, l := range resp.ExpectedLegs {
				_, _ = fmt.Fprintf(out, "  %s\n", l)
			}
			if len(resp.RemovedLegs) == 0 {
				_, _ = fmt.Fprintln(out, "\nNo stale instances.")
			} else {
				verb := "Removed"
				if dryRun {
					verb = "Would remove"
				}
				_, _ = fmt.Fprintf(out, "\n%s:\n", verb)
				for _, l := range resp.RemovedLegs {
					_, _ = fmt.Fprintf(out, "  %s\n", l)
				}
			}
			_, _ = fmt.Fprintf(out, "\n%s\n", resp.Message)
			log.Debug("wanRepairCommand: done")
			return nil
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "report what would change without touching anything")
	return cmd
}
