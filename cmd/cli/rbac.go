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
	"github.com/haify-project/haify/pkg/client"
)

// The RBAC commands go through the gRPC API like every other command, so they
// honour --token and the --tls flags. They used to speak plain HTTP to the
// REST port, which put admin tokens and freshly created user tokens on the
// wire in clear even on a TLS cluster.

// withRBACClient dials the controller and runs fn with a bounded context.
func withRBACClient(fn func(ctx context.Context, c *client.HaifyClient) error) error {
	log := zap.L()
	log.Debug("withRBACClient: connecting to controller")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c, err := newHaifyClient()
	if err != nil {
		log.Debug("withRBACClient: newHaifyClient failed", zap.Error(err))
		return err
	}
	defer closeClient(c)
	err = fn(ctx, c)
	log.Debug("withRBACClient: done", zap.Error(err))
	return err
}

const rbacDisabledNotice = "RBAC is not enabled on the controller (single-token auth)."

func rbacCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "rbac",
		Short: "Inspect role-based access control (identity and policy)",
	}
	cmd.AddCommand(rbacWhoamiCommand())
	cmd.AddCommand(rbacPoliciesCommand())
	cmd.AddCommand(rbacUserCommand())
	return cmd
}

func rbacUserCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "user",
		Short: "Manage RBAC users (admin only)",
	}
	cmd.AddCommand(rbacUserAddCommand())
	cmd.AddCommand(rbacUserRemoveCommand())
	cmd.AddCommand(rbacUserSetRoleCommand())
	return cmd
}

func rbacUserAddCommand() *cobra.Command {
	var name, role, userToken string
	cmd := &cobra.Command{
		Use:   "add",
		Short: "Add a user with a role (a token is generated unless --user-token is given)",
		RunE: func(cmd *cobra.Command, args []string) error {
			log := zap.L()
			log.Debug("rbacUserAddCommand: running command", zap.Strings("args", args), zap.String("name", name),
				zap.String("role", role), zap.Bool("has_user_token", userToken != ""))
			if name == "" || role == "" {
				log.Debug("rbacUserAddCommand: --name or --role missing")
				return fmt.Errorf("--name and --role are required")
			}
			return withRBACClient(func(ctx context.Context, c *client.HaifyClient) error {
				token, err := c.CreateRbacUser(ctx, name, role, userToken)
				if err != nil {
					log.Debug("rbacUserAddCommand: CreateRbacUser failed", zap.Error(err))
					return err
				}
				log.Debug("rbacUserAddCommand: user created", zap.String("name", name), zap.String("role", role), zap.Bool("token_issued", token != ""))
				fmt.Printf("User %q created with role %q.\n", name, role)
				fmt.Printf("Token (store it now, it is not shown again):\n  %s\n", token)
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "User name")
	cmd.Flags().StringVar(&role, "role", "", "Role (admin, operator, viewer)")
	// Not --token: that is the global flag carrying the caller's own token.
	cmd.Flags().StringVar(&userToken, "user-token", "", "Token for the new user (optional; min 16 chars; generated when omitted)")
	return cmd
}

func rbacUserRemoveCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "remove <name>",
		Short: "Remove a user",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			log := zap.L()
			log.Debug("rbacUserRemoveCommand: running command", zap.Strings("args", args))
			return withRBACClient(func(ctx context.Context, c *client.HaifyClient) error {
				if err := c.DeleteRbacUser(ctx, args[0]); err != nil {
					log.Debug("rbacUserRemoveCommand: DeleteRbacUser failed", zap.Error(err))
					return err
				}
				log.Debug("rbacUserRemoveCommand: user removed", zap.String("name", args[0]))
				fmt.Printf("User %q removed.\n", args[0])
				return nil
			})
		},
	}
}

func rbacUserSetRoleCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "set-role <name> <role>",
		Short: "Change a user's role",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			log := zap.L()
			log.Debug("rbacUserSetRoleCommand: running command", zap.Strings("args", args))
			return withRBACClient(func(ctx context.Context, c *client.HaifyClient) error {
				if err := c.SetRbacUserRole(ctx, args[0], args[1]); err != nil {
					log.Debug("rbacUserSetRoleCommand: SetRbacUserRole failed", zap.Error(err))
					return err
				}
				log.Debug("rbacUserSetRoleCommand: role changed", zap.String("name", args[0]), zap.String("role", args[1]))
				fmt.Printf("User %q is now %q.\n", args[0], args[1])
				return nil
			})
		},
	}
}

func rbacWhoamiCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "whoami",
		Short: "Show the identity and role of your API token",
		RunE: func(cmd *cobra.Command, args []string) error {
			log := zap.L()
			log.Debug("rbacWhoamiCommand: running command", zap.Strings("args", args))
			return withRBACClient(func(ctx context.Context, c *client.HaifyClient) error {
				res, err := c.RbacWhoami(ctx)
				if err != nil {
					log.Debug("rbacWhoamiCommand: RbacWhoami failed", zap.Error(err))
					return err
				}
				if !res.Enabled {
					log.Debug("rbacWhoamiCommand: RBAC not enabled")
					fmt.Println(rbacDisabledNotice)
					return nil
				}
				log.Debug("rbacWhoamiCommand: identity", zap.String("user", res.User), zap.String("role", res.Role), zap.Bool("can_admin", res.CanAdmin))
				fmt.Printf("User:      %s\n", res.User)
				fmt.Printf("Role:      %s\n", res.Role)
				fmt.Printf("Admin:     %v\n", res.CanAdmin)
				return nil
			})
		},
	}
}

func rbacPoliciesCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "policies",
		Short: "Show effective roles and user assignments (admin only)",
		RunE: func(cmd *cobra.Command, args []string) error {
			log := zap.L()
			log.Debug("rbacPoliciesCommand: running command", zap.Strings("args", args))
			return withRBACClient(func(ctx context.Context, c *client.HaifyClient) error {
				res, err := c.ListRbacPolicies(ctx)
				if err != nil {
					log.Debug("rbacPoliciesCommand: ListRbacPolicies failed", zap.Error(err))
					return err
				}
				if !res.Enabled {
					log.Debug("rbacPoliciesCommand: RBAC not enabled")
					fmt.Println(rbacDisabledNotice)
					return nil
				}
				printRBACPolicies(res.Users, res.Policies)
				return nil
			})
		},
	}
}

// printRBACPolicies writes the user and policy tables. Writes to stdout are
// best-effort: they fail only on a closed pipe (`haify ... | head`) or a full
// disk, and reporting either would turn a successful read into an error.
func printRBACPolicies(users []*haifypb.RbacUser, policies []*haifypb.RbacPolicy) {
	log := zap.L()
	log.Debug("printRBACPolicies: printing users and policies", zap.Int("users", len(users)), zap.Int("policies", len(policies)))
	fmt.Println("Users")
	uw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	_, _ = fmt.Fprintln(uw, "  NAME\tROLE")
	for _, u := range users {
		_, _ = fmt.Fprintf(uw, "  %s\t%s\n", u.GetName(), u.GetRole())
	}
	_ = uw.Flush()

	fmt.Println("\nPolicies")
	pw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	_, _ = fmt.Fprintln(pw, "  ROLE\tOBJECT\tACTION")
	for _, p := range policies {
		_, _ = fmt.Fprintf(pw, "  %s\t%s\t%s\n", p.GetRole(), p.GetObject(), p.GetAction())
	}
	_ = pw.Flush()
}
