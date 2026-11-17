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

// `haify ha vm-status` and `haify ha vm-sync`: the libvirt definition of an
// HA guest on every replica that may run it.

func haVMStatus() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "vm-status <resource>",
		Short: "Show whether every replica has the same definition of an HA guest",
		Long: "For a resource made highly available with `ha create --vm`: whether each replica\n" +
			"that may run the guest has its definition, under the same UUID, unchanged since the\n" +
			"last sync. A replica without it cannot take the guest over.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			log := zap.L()
			log.Debug("haVMStatus: running command", zap.Strings("args", args))
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()

			haifyClient, err := newHaifyClient()
			if err != nil {
				log.Debug("haVMStatus: newHaifyClient failed", zap.Error(err))
				return fmt.Errorf("failed to connect to controller: %w", err)
			}
			defer closeClient(haifyClient)

			d, err := haifyClient.GetVmDefinition(ctx, args[0])
			if err != nil {
				log.Debug("haVMStatus: GetVmDefinition failed", zap.Error(err))
				return fmt.Errorf("failed to read the guest definition: %w", err)
			}
			printVMDefinition(os.Stdout, d)
			log.Debug("haVMStatus: done", zap.Bool("consistent", d.GetConsistent()))
			if !d.GetConsistent() {
				fmt.Printf("\nRun `haify ha vm-sync %s` to make the replicas agree.\n", args[0])
			}
			return nil
		},
	}
	return cmd
}

func haVMSync() *cobra.Command {
	var from string

	cmd := &cobra.Command{
		Use:   "vm-sync <resource>",
		Short: "Give every replica the same definition of an HA guest",
		Long: "Take the guest's libvirt definition from one node and define it on every other\n" +
			"replica, under the same UUID and with autostart off. Run it after changing the\n" +
			"guest (memory, a disk) on one node. The running guest is not touched: a change\n" +
			"applies when it next starts.\n\n" +
			"Without --from, the one node whose definition was edited since the last sync is the\n" +
			"source; when none was, replicas that lack the definition are given the one Haify\n" +
			"keeps. --from names the node when more than one was edited.\n\n" +
			"  virsh edit web1                      # on kvm1\n" +
			"  haify ha vm-sync web1\n" +
			"  haify ha vm-sync web1 --from kvm2",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			log := zap.L()
			log.Debug("haVMSync: running command", zap.Strings("args", args), zap.String("from", from))
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()

			haifyClient, err := newHaifyClient()
			if err != nil {
				log.Debug("haVMSync: newHaifyClient failed", zap.Error(err))
				return fmt.Errorf("failed to connect to controller: %w", err)
			}
			defer closeClient(haifyClient)

			d, err := haifyClient.SyncVmDefinition(ctx, args[0], from)
			if d != nil && len(d.GetNodes()) > 0 {
				printVMDefinition(os.Stdout, d)
			}
			if err != nil {
				log.Debug("haVMSync: SyncVmDefinition failed", zap.Error(err))
				return fmt.Errorf("failed to sync the guest definition: %w", err)
			}
			fmt.Printf("\nDefinition of %s synced from %s\n", d.GetDomain(), d.GetSourceNode())
			log.Debug("haVMSync: done", zap.Bool("consistent", d.GetConsistent()))
			return nil
		},
	}

	cmd.Flags().StringVar(&from, "from", "", "Node whose definition to keep")
	return cmd
}

// printVMDefinition renders a guest's definition across its replicas.
func printVMDefinition(w io.Writer, d *haifypb.VmDefinitionResponse) {
	_, _ = fmt.Fprintf(w, "Guest %s (%s)\n", d.GetDomain(), d.GetResource())
	_, _ = fmt.Fprintf(w, "  UUID:    %s\n", d.GetUuid())
	if d.GetSourceNode() != "" {
		_, _ = fmt.Fprintf(w, "  Source:  %s, synced %s\n", d.GetSourceNode(), time.Unix(d.GetUpdatedUnix(), 0).Format(time.RFC3339))
	}
	_, _ = fmt.Fprintf(w, "  Replicas:\n")
	for _, n := range d.GetNodes() {
		line := fmt.Sprintf("    - %s: %s", n.GetNode(), vmDefinitionStateText(n.GetState()))
		if n.GetRunning() {
			line += ", guest running here"
		}
		if n.GetDetail() != "" {
			line += " (" + n.GetDetail() + ")"
		}
		_, _ = fmt.Fprintln(w, line)
	}
}

// vmDefinitionStateText words a replica's definition state.
func vmDefinitionStateText(state string) string {
	switch state {
	case "ok":
		return "defined"
	case "missing":
		return "not defined"
	case "changed":
		return "differs"
	case "uuid_mismatch":
		return "another UUID"
	case "unreachable":
		return "did not answer"
	}
	return state
}
