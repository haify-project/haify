package main

import (
	"context"
	"fmt"
	"os"
	"sort"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"go.uber.org/zap"
)

func resourceSetOptions() *cobra.Command {
	var options map[string]string
	var unset []string
	cmd := &cobra.Command{
		Use:   "set-options <resource>",
		Short: "Set or remove DRBD options on a resource and apply them with drbdadm adjust",
		Long: "Set or remove DRBD options on an existing resource and apply them with " +
			"`drbdadm adjust`, without recreating the resource. Options use the " +
			"\"section/key\" form (e.g. net/max-buffers=8000, disk/on-io-error=detach); " +
			"a bare key goes to the resource-level options section. --unset removes a " +
			"key so DRBD's default applies again (an empty value does the same).\n\n" +
			"The new config is checked by a node's drbdadm before any node gets it; an " +
			"option DRBD does not know, or a value it does not accept, changes nothing. " +
			"If applying it fails on a node, the previous config is put back.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			log := zap.L()
			// Option values are not traced: net/shared-secret is one of them.
			log.Debug("resourceSetOptions: running command", zap.Strings("args", args), zap.Int("options", len(options)),
				zap.Strings("unset", unset))
			all := map[string]string{}
			for k, v := range options {
				all[k] = v
			}
			for _, k := range unset {
				all[k] = ""
			}
			if len(all) == 0 {
				log.Debug("resourceSetOptions: nothing given")
				return fmt.Errorf("give at least one --drbd-options key=value or --unset key")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			haifyClient, err := newHaifyClient()
			if err != nil {
				log.Debug("resourceSetOptions: newHaifyClient failed", zap.Error(err))
				return fmt.Errorf("failed to connect to controller: %w", err)
			}
			defer closeClient(haifyClient)
			if err := haifyClient.UpdateResourceOptions(ctx, args[0], all); err != nil {
				log.Debug("resourceSetOptions: UpdateResourceOptions failed", zap.Error(err))
				return fmt.Errorf("failed to update options: %w", err)
			}
			keys := make([]string, 0, len(all))
			for k := range all {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			fmt.Printf("Options applied to '%s': %v\n", args[0], keys)
			log.Debug("resourceSetOptions: done")
			return nil
		},
	}
	cmd.Flags().StringToStringVar(&options, "drbd-options", nil,
		"DRBD options as section/key=value (e.g. net/max-buffers=8000,on-no-quorum=suspend-io)")
	cmd.Flags().StringSliceVar(&unset, "unset", nil, "DRBD options to remove, as section/key (e.g. net/max-buffers)")
	return cmd
}

func resourceGetOptions() *cobra.Command {
	return &cobra.Command{
		Use:   "get-options <resource>",
		Short: "Show the DRBD options a resource's config sets",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			log := zap.L()
			log.Debug("resourceGetOptions: running command", zap.Strings("args", args))
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			haifyClient, err := newHaifyClient()
			if err != nil {
				log.Debug("resourceGetOptions: newHaifyClient failed", zap.Error(err))
				return fmt.Errorf("failed to connect to controller: %w", err)
			}
			defer closeClient(haifyClient)
			opts, err := haifyClient.GetResourceOptions(ctx, args[0])
			if err != nil {
				log.Debug("resourceGetOptions: GetResourceOptions failed", zap.Error(err))
				return fmt.Errorf("failed to read options: %w", err)
			}
			// Best-effort writes, as in every table this CLI prints: a closed
			// pipe is not a failure of the read.
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
			_, _ = fmt.Fprintln(w, "OPTION\tVALUE")
			for _, o := range opts {
				v := o.Value
				if o.Secret {
					v = "(set)"
				}
				_, _ = fmt.Fprintf(w, "%s\t%s\n", o.Key, v)
			}
			_ = w.Flush()
			log.Debug("resourceGetOptions: done", zap.Int("options", len(opts)))
			return nil
		},
	}
}
