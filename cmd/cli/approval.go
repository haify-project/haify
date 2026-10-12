package main

import (
	"context"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"go.uber.org/zap"

	"github.com/haify-project/haify/pkg/client"
)

// With [rbac.approval] on, a call such as deleting a backup target fails
// with the id of a pending request; a second user approves it here, and the
// first repeats the call.

func approvalCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "approval",
		Short: "Two-person approval: list, approve or reject held-back calls",
	}
	cmd.AddCommand(approvalListCommand(), approvalDecideCommand(true), approvalDecideCommand(false))
	return cmd
}

func approvalListCommand() *cobra.Command {
	var all bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List pending (and approved, not yet used) requests",
		RunE: func(cmd *cobra.Command, args []string) error {
			log := zap.L()
			log.Debug("approvalListCommand: running command", zap.Strings("args", args), zap.Bool("all", all))
			return withRBACClient(func(ctx context.Context, c *client.HaifyClient) error {
				list, err := c.ListApprovals(ctx, all)
				if err != nil {
					log.Debug("approvalListCommand: ListApprovals failed", zap.Error(err))
					return err
				}
				if len(list) == 0 {
					log.Debug("approvalListCommand: no requests")
					fmt.Println("No approval requests.")
					return nil
				}
				w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
				_, _ = fmt.Fprintln(w, "ID\tSTATE\tMETHOD\tREQUESTER\tEXPIRES\tDECIDED BY\tREQUEST")
				log.Debug("approvalListCommand: printing requests", zap.Int("count", len(list)))
				for _, a := range list {
					log.Debug("approvalListCommand: request", zap.String("id", a.Id), zap.String("state", a.State), zap.String("method", a.Method), zap.String("requester", a.Requester))
					_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", a.Id, a.State, a.Method, a.Requester,
						a.ExpiresAt, dashIfEmpty(a.DecidedBy), a.Request)
				}
				return w.Flush()
			})
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "Include rejected, used and expired requests")
	return cmd
}

func approvalDecideCommand(approve bool) *cobra.Command {
	use, short := "reject <id>", "Reject a pending request"
	if approve {
		use, short = "approve <id>", "Approve another user's pending request (read its REQUEST first)"
	}
	return &cobra.Command{
		Use:   use,
		Short: short,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			log := zap.L()
			log.Debug("approvalDecideCommand: running command", zap.Strings("args", args), zap.Bool("approve", approve))
			return withRBACClient(func(ctx context.Context, c *client.HaifyClient) error {
				if !approve {
					log.Debug("approvalDecideCommand: rejecting", zap.String("id", args[0]))
					if _, err := c.RejectRequest(ctx, args[0]); err != nil {
						log.Debug("approvalDecideCommand: RejectRequest failed", zap.Error(err))
						return err
					}
					fmt.Printf("Request %s rejected.\n", args[0])
					return nil
				}
				a, msg, err := c.ApproveRequest(ctx, args[0])
				if err != nil {
					log.Debug("approvalDecideCommand: ApproveRequest failed", zap.Error(err))
					return err
				}
				log.Debug("approvalDecideCommand: approved", zap.String("id", args[0]), zap.String("method", a.Method), zap.String("requester", a.Requester))
				fmt.Printf("Approved %s %s for %s; %s.\n", a.Method, a.Request, a.Requester, msg)
				return nil
			})
		},
	}
}

func dashIfEmpty(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
