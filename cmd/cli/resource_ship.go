package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	haifypb "github.com/haify-project/haify/api/proto/v1"
	"github.com/haify-project/haify/pkg/client"
	"github.com/spf13/cobra"
	"go.uber.org/zap"
)

func resourceShip() *cobra.Command {
	var to, tokenFile, as, pool string
	var replicas uint32
	cmd := &cobra.Command{
		Use:   "ship <resource> --to <controller>",
		Short: "Copy a resource to another Haify cluster, directly",
		Long: `A snapshot of every volume streams from a replica here straight into a new
resource on the other cluster, encrypted, with nothing in between. Both ends
checksum what passed; if they disagree the copy there is deleted. The resource
here keeps serving throughout. Follow it with: haify pool jobs

  haify resource ship db --to 10.1.0.10:3374 --token-file ~/.haify/dr-token
  haify resource ship db --to 10.1.0.10:3374 --as db-copy --pool vg0 --replicas 2

The other cluster's nodes must accept connections from this cluster's nodes.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			log := zap.L()
			log.Debug("resourceShip: running command", zap.Strings("args", args), zap.String("to", to), zap.String("token_file", tokenFile),
				zap.String("as", as), zap.String("pool", pool), zap.Uint32("replicas", replicas))
			if to == "" {
				log.Debug("resourceShip: --to missing")
				return fmt.Errorf("--to is required: the other cluster's controller, host:port")
			}
			token := ""
			if tokenFile != "" {
				b, err := os.ReadFile(tokenFile)
				if err != nil {
					log.Debug("resourceShip: token file not readable", zap.String("path", tokenFile), zap.Error(err))
					return err
				}
				token = strings.TrimSpace(string(b))
				log.Debug("resourceShip: token read for the other cluster", zap.String("path", tokenFile), zap.Bool("has_token", token != ""))
			} else {
				log.Debug("resourceShip: no --token-file, the other cluster gets no token")
			}
			return runJob(func(ctx context.Context, c *client.HaifyClient) (string, error) {
				resp, err := c.ShipResource(ctx, &haifypb.ShipResourceRequest{Resource: args[0], Target: to,
					TargetToken: token, Name: as, Pool: pool, Replicas: replicas})
				if err != nil {
					log.Debug("resourceShip: ShipResource failed", zap.Error(err))
					return "", err
				}
				log.Debug("resourceShip: ship job started", zap.String("job_id", resp.JobId))
				return fmt.Sprintf("job %s: %s", resp.JobId, resp.Message), nil
			})
		},
	}
	cmd.Flags().StringVar(&to, "to", "", "The other cluster's controller (host:port; its floating IP with Self-HA)")
	cmd.Flags().StringVar(&tokenFile, "token-file", "", "File with an API token of the other cluster")
	cmd.Flags().StringVar(&as, "as", "", "Name there (default: the same)")
	cmd.Flags().StringVar(&pool, "pool", "", "Pool there (default: the same name as here)")
	cmd.Flags().Uint32Var(&replicas, "replicas", 0, "Replicas there (default: as many as here)")
	return cmd
}
