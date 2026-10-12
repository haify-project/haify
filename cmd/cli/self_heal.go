package main

import (
	"context"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"go.uber.org/zap"

	haifypb "github.com/haify-project/haify/api/proto/v1"
)

// Self-healing commands: moving a replica, rebalancing by plan, and dealing
// with a node after it was evicted (docs/user-guide.md, "Self-healing").

func withController(timeout time.Duration, fn func(ctx context.Context, c haifypb.HaifyControllerClient) error) error {
	log := zap.L()
	log.Debug("withController: connecting to controller", zap.Duration("timeout", timeout))
	c, conn, err := newResourceGRPCClient()
	if err != nil {
		log.Debug("withController: newResourceGRPCClient failed", zap.Error(err))
		return err
	}
	defer func() { _ = conn.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	err = fn(ctx, c)
	log.Debug("withController: done", zap.Error(err))
	return err
}

func resultLine(ok bool, msg string) error {
	if !ok {
		zap.L().Debug("resultLine: controller reported failure", zap.String("message", msg))
		return fmt.Errorf("%s", msg)
	}
	zap.L().Debug("resultLine: controller reported success", zap.String("message", msg))
	fmt.Println(msg)
	return nil
}

func resourceMoveReplicaCommand() *cobra.Command {
	var from, to string
	cmd := &cobra.Command{
		Use:   "move-replica <resource>",
		Short: "Move a replica to another node: the new one syncs before the old one goes",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			log := zap.L()
			log.Debug("resourceMoveReplicaCommand: running command", zap.Strings("args", args), zap.String("from", from), zap.String("to", to))
			if from == "" || to == "" {
				log.Debug("resourceMoveReplicaCommand: --from or --to missing")
				return fmt.Errorf("--from and --to are required")
			}
			return withController(30*time.Minute, func(ctx context.Context, c haifypb.HaifyControllerClient) error {
				r, err := c.MoveReplica(ctx, &haifypb.MoveReplicaRequest{Resource: args[0], From: from, To: to})
				if err != nil {
					log.Debug("resourceMoveReplicaCommand: MoveReplica failed", zap.Error(err))
					return err
				}
				return resultLine(r.Success, r.Message)
			})
		},
	}
	cmd.Flags().StringVar(&from, "from", "", "Node whose replica moves")
	cmd.Flags().StringVar(&to, "to", "", "Node that gets it")
	return cmd
}

func rebalanceCommand() *cobra.Command {
	var apply bool
	var maxMoves uint32
	cmd := &cobra.Command{
		Use:   "rebalance",
		Short: "Propose (or with --apply, run) replica moves from the fullest node",
		RunE: func(cmd *cobra.Command, args []string) error {
			log := zap.L()
			log.Debug("rebalanceCommand: running command", zap.Strings("args", args), zap.Bool("apply", apply), zap.Uint32("max_moves", maxMoves))
			return withController(time.Minute, func(ctx context.Context, c haifypb.HaifyControllerClient) error {
				r, err := c.PlanRebalance(ctx, &haifypb.PlanRebalanceRequest{Apply: apply, MaxMoves: maxMoves})
				if err != nil {
					log.Debug("rebalanceCommand: PlanRebalance failed", zap.Error(err))
					return err
				}
				log.Debug("rebalanceCommand: plan", zap.Int("moves", len(r.Moves)), zap.Int("notes", len(r.Notes)), zap.Bool("success", r.Success))
				if len(r.Moves) > 0 {
					w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
					_, _ = fmt.Fprintln(w, "RESOURCE\tFROM\tTO\tSIZE")
					for _, m := range r.Moves {
						log.Debug("rebalanceCommand: move", zap.String("resource", m.Resource), zap.String("from", m.From), zap.String("to", m.To), zap.Uint64("size_gb", m.SizeGb))
						_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%d GiB\n", m.Resource, m.From, m.To, m.SizeGb)
					}
					_ = w.Flush()
				}
				for _, n := range r.Notes {
					fmt.Println("note:", n)
				}
				return resultLine(r.Success, r.Message)
			})
		},
	}
	cmd.Flags().BoolVar(&apply, "apply", false, "Run the moves, one at a time")
	cmd.Flags().Uint32Var(&maxMoves, "max-moves", 0, "At most this many moves (default 10)")
	return cmd
}

func nodeLostCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "lost <node>",
		Short: "Remove every replica of a node that will not come back, and keep it evicted",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			log := zap.L()
			log.Debug("nodeLostCommand: running command", zap.Strings("args", args))
			return withController(30*time.Minute, func(ctx context.Context, c haifypb.HaifyControllerClient) error {
				r, err := c.MarkNodeLost(ctx, &haifypb.MarkNodeLostRequest{Node: args[0]})
				if err != nil {
					log.Debug("nodeLostCommand: MarkNodeLost failed", zap.Error(err))
					return err
				}
				log.Debug("nodeLostCommand: result", zap.String("node", args[0]), zap.Bool("success", r.Success), zap.Strings("resources", r.Resources))
				for _, res := range r.Resources {
					fmt.Println("removed the replica of", res)
				}
				return resultLine(r.Success, r.Message)
			})
		},
	}
}

func nodeRestoreCommand() *cobra.Command {
	var dryRun bool
	cmd := &cobra.Command{
		Use:   "restore <node>",
		Short: "Clean what a returned node holds of resources it left, and let it take replicas again",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			log := zap.L()
			log.Debug("nodeRestoreCommand: running command", zap.Strings("args", args), zap.Bool("dry_run", dryRun))
			return withController(30*time.Minute, func(ctx context.Context, c haifypb.HaifyControllerClient) error {
				r, err := c.RestoreNode(ctx, &haifypb.RestoreNodeRequest{Node: args[0], DryRun: dryRun})
				if err != nil {
					log.Debug("nodeRestoreCommand: RestoreNode failed", zap.Error(err))
					return err
				}
				log.Debug("nodeRestoreCommand: result", zap.String("node", args[0]), zap.Bool("success", r.Success), zap.Int("plan_steps", len(r.Plan)))
				for _, p := range r.Plan {
					fmt.Println("-", p)
				}
				return resultLine(r.Success, r.Message)
			})
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Only show what would be deleted")
	return cmd
}

func haSetPreferredCommand() *cobra.Command {
	var nodes []string
	var policy string
	cmd := &cobra.Command{
		Use:   "set-preferred <resource>",
		Short: "Order where drbd-reactor starts the resource (a preference, not a fence)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			log := zap.L()
			log.Debug("haSetPreferredCommand: running command", zap.Strings("args", args), zap.Strings("nodes", nodes), zap.String("policy", policy))
			return withController(time.Minute, func(ctx context.Context, c haifypb.HaifyControllerClient) error {
				r, err := c.SetHaPreferredNodes(ctx, &haifypb.SetHaPreferredNodesRequest{Resource: args[0], Nodes: nodes, Policy: policy})
				if err != nil {
					log.Debug("haSetPreferredCommand: SetHaPreferredNodes failed", zap.Error(err))
					return err
				}
				return resultLine(r.Success, r.Message)
			})
		},
	}
	cmd.Flags().StringSliceVar(&nodes, "nodes", nil, "Nodes in order of preference; none removes the preference")
	cmd.Flags().StringVar(&policy, "policy", "", "start-only (pick where it starts; drbd-reactor 1.9+) or always (also move it back)")
	return cmd
}
