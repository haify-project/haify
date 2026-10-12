package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	haifypb "github.com/haify-project/haify/api/proto/v1"
	"github.com/haify-project/haify/pkg/client"
	"github.com/spf13/cobra"
	"go.uber.org/zap"
)

// ReactorStatus represents the status output from drbd-reactorctl status --json
type ReactorStatus struct {
	Promoter   []ReactorPromoterStatus `json:"promoter"`
	Prometheus []ReactorPluginStatus   `json:"prometheus"`
	Debugger   []ReactorPluginStatus   `json:"debugger"`
	UMH        []ReactorPluginStatus   `json:"umh"`
	AgentX     []ReactorPluginStatus   `json:"agentx"`
}

// ReactorPromoterStatus represents status of a promoter plugin
type ReactorPromoterStatus struct {
	DRBDResource string                 `json:"drbd_resource"`
	Path         string                 `json:"path"`
	PrimaryOn    string                 `json:"primary_on"`
	Target       ReactorServiceStatus   `json:"target"`
	Dependencies []ReactorServiceStatus `json:"dependencies"`
	Status       string                 `json:"status"`
}

// ReactorServiceStatus represents status of a systemd service
type ReactorServiceStatus struct {
	Name    string `json:"name"`
	Status  string `json:"status"`
	Freezer string `json:"freezer"`
}

// ReactorPluginStatus represents status of a generic reactor plugin
type ReactorPluginStatus struct {
	Path    string `json:"path"`
	Address string `json:"address,omitempty"`
	Status  string `json:"status"`
}

func haCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "ha",
		Short: "Make a DRBD resource highly available with drbd-reactor",
	}

	cmd.AddCommand(haCreate())
	cmd.AddCommand(haEvict())
	cmd.AddCommand(haSetTiebreaker())
	cmd.AddCommand(haSetPreferredCommand())
	cmd.AddCommand(haDelete())
	cmd.AddCommand(haList())
	cmd.AddCommand(haStatus())
	cmd.AddCommand(haSelfCommand())

	return cmd
}

func haCreate() *cobra.Command {
	var services string
	var mountPoint string
	var fsType string
	var vip string
	var vm string

	cmd := &cobra.Command{
		Use:   "create <resource>",
		Short: "Create HA configuration for a resource",
		Long: "Create a drbd-reactor promoter for a resource: its mount, services and virtual IP\n" +
			"start on the node where the resource is Primary, and on another replica when that\n" +
			"node fails.\n\n" +
			"--vm runs a libvirt guest instead: define it under that name on every diskful\n" +
			"replica first (virsh define, autostart off), with its disks on\n" +
			"/dev/drbd/by-res/<resource>/<volume>.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			log := zap.L()
			log.Debug("haCreate: running command", zap.Strings("args", args), zap.String("services", services),
				zap.String("mount", mountPoint), zap.String("fs_type", fsType), zap.String("vip", vip), zap.String("vm", vm))
			resource := args[0]

			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()

			haifyClient, err := newHaifyClient()
			if err != nil {
				log.Debug("haCreate: newHaifyClient failed", zap.Error(err))
				return fmt.Errorf("failed to connect to controller: %w", err)
			}
			defer closeClient(haifyClient)

			// Parse services
			var serviceList []string
			if services != "" {
				serviceList = strings.Split(services, ",")
				log.Debug("haCreate: services to manage", zap.Strings("services", serviceList))
			}

			var agents []*haifypb.OcfAgent
			if vm != "" {
				log.Debug("haCreate: adding a VirtualDomain agent for the guest", zap.String("vm", vm))
				agent, err := client.VirtualDomainAgent(vm)
				if err != nil {
					log.Debug("haCreate: VirtualDomainAgent failed", zap.Error(err))
					return err
				}
				agents = append(agents, agent)
			}

			configPath, err := haifyClient.MakeHa(ctx, resource, serviceList, mountPoint, fsType, vip, agents, nil)
			if err != nil {
				log.Debug("haCreate: MakeHa failed", zap.Error(err))
				return fmt.Errorf("failed to create HA config: %w", err)
			}

			log.Debug("haCreate: HA config created", zap.String("resource", resource), zap.String("config_path", configPath))
			if vm != "" {
				log.Debug("haCreate: labelling the resource with its guest", zap.String("resource", resource), zap.String("vm", vm))
				if _, err := haifyClient.SetResourceLabels(ctx, resource,
					map[string]string{client.LibvirtDomainLabel: vm}, nil); err != nil {
					log.Debug("haCreate: SetResourceLabels failed", zap.Error(err))
					fmt.Printf("Warning: could not label %s with the guest: %v\n", resource, err)
				}
			}

			fmt.Printf("HA configuration created successfully\n")
			fmt.Printf("  Resource:  %s\n", resource)
			fmt.Printf("  Config:    %s\n", configPath)
			if len(serviceList) > 0 {
				fmt.Printf("  Services:  %v\n", serviceList)
			}
			if mountPoint != "" {
				fmt.Printf("  Mount:     %s (%s)\n", mountPoint, fsType)
			}
			if vip != "" {
				fmt.Printf("  VIP:       %s\n", vip)
			}
			if vm != "" {
				fmt.Printf("  VM:        %s (libvirt)\n", vm)
			}
			fmt.Printf("\nConfiguration distributed to all nodes and drbd-reactor reloaded\n")

			log.Debug("haCreate: done")
			return nil
		},
	}

	cmd.Flags().StringVar(&services, "services", "", "Systemd services to start/stop (comma-separated)")
	cmd.Flags().StringVar(&mountPoint, "mount", "", "Mount point for filesystem")
	cmd.Flags().StringVar(&fsType, "fstype", "ext4", "Filesystem type (ext4, xfs, etc.)")
	cmd.Flags().StringVar(&vip, "vip", "", "Virtual IP (CIDR, e.g., 192.168.1.100/24)")
	cmd.Flags().StringVar(&vm, "vm", "", "libvirt guest to run where the resource is Primary (defined on every replica)")

	return cmd
}

func haDelete() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "delete <resource>",
		Short: "Delete HA configuration for a resource",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			log := zap.L()
			log.Debug("haDelete: running command", zap.Strings("args", args))
			resource := args[0]

			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()

			haifyClient, err := newHaifyClient()
			if err != nil {
				log.Debug("haDelete: newHaifyClient failed", zap.Error(err))
				return fmt.Errorf("failed to connect to controller: %w", err)
			}
			defer closeClient(haifyClient)

			err = haifyClient.DeleteHa(ctx, resource)
			if err != nil {
				log.Debug("haDelete: DeleteHa failed", zap.Error(err))
				return fmt.Errorf("failed to delete HA config: %w", err)
			}

			fmt.Printf("HA configuration deleted successfully\n")
			fmt.Printf("  Resource: %s\n", resource)

			log.Debug("haDelete: done")
			return nil
		},
	}

	return cmd
}

func haEvict() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "evict <resource>",
		Short: "Evict an HA resource from the current active node",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			log := zap.L()
			log.Debug("haEvict: running command", zap.Strings("args", args))
			resource := args[0]

			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()

			haifyClient, err := newHaifyClient()
			if err != nil {
				log.Debug("haEvict: newHaifyClient failed", zap.Error(err))
				return fmt.Errorf("failed to connect to controller: %w", err)
			}
			defer closeClient(haifyClient)

			if err := haifyClient.EvictHa(ctx, resource); err != nil {
				log.Debug("haEvict: EvictHa failed", zap.Error(err))
				return fmt.Errorf("failed to evict HA resource: %w", err)
			}

			fmt.Printf("HA resource evicted successfully\n")
			fmt.Printf("  Resource: %s\n", resource)

			log.Debug("haEvict: done")
			return nil
		},
	}

	return cmd
}

func haList() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List all HA configurations",
		RunE: func(cmd *cobra.Command, args []string) error {
			log := zap.L()
			log.Debug("haList: running command", zap.Strings("args", args))
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			haifyClient, err := newHaifyClient()
			if err != nil {
				log.Debug("haList: newHaifyClient failed", zap.Error(err))
				return fmt.Errorf("failed to connect to controller: %w", err)
			}
			defer closeClient(haifyClient)

			configs, err := haifyClient.ListHa(ctx)
			if err != nil {
				log.Debug("haList: ListHa failed", zap.Error(err))
				return fmt.Errorf("failed to list HA configs: %w", err)
			}

			if len(configs) == 0 {
				log.Debug("haList: returning early", zap.String("condition", "len(configs) == 0"))
				fmt.Println("No HA configurations found")
				return nil
			}

			// Fetch reactor status via the controller so we always query the
			// primary node, not the machine running haify.
			type promoterInfo struct {
				status, primaryOn        string
				targetName, targetStatus string
			}
			pm := make(map[string]promoterInfo)
			haStatuses, statusErr := haifyClient.GetHaStatus(ctx, "")
			log.Debug("haList: promoter status fetched", zap.Int("promoters", len(haStatuses)), zap.Error(statusErr))
			if statusErr == nil {
				for _, p := range haStatuses {
					log.Debug("haList: promoter", zap.String("resource", p.GetDrbdResource()), zap.String("status", p.GetStatus()), zap.String("primary_on", p.GetPrimaryOn()))
					pi := promoterInfo{
						status:    p.GetStatus(),
						primaryOn: p.GetPrimaryOn(),
					}
					if t := p.GetTarget(); t != nil {
						pi.targetName = t.GetName()
						pi.targetStatus = t.GetStatus()
					}
					pm[p.GetDrbdResource()] = pi
				}
			}

			fmt.Printf("HA Configurations (%d):\n", len(configs))
			for _, cfg := range configs {
				_, hasPromoter := pm[cfg.GetResource()]
				log.Debug("haList: config", zap.String("resource", cfg.GetResource()), zap.Bool("has_promoter_status", hasPromoter))
				fmt.Printf("  - %s\n", cfg.GetResource())
				if p, ok := pm[cfg.GetResource()]; ok {
					fmt.Printf("      Status:    %s %s\n", statusIcon(p.status), p.status)
					if p.primaryOn != "" {
						fmt.Printf("      Primary:   %s\n", p.primaryOn)
					}
					if p.targetName != "" {
						fmt.Printf("      Target:    %s (%s)\n", p.targetName, p.targetStatus)
					}
				}
				if status, err := haifyClient.ResourceStatus(ctx, cfg.GetResource()); err == nil {
					activeNode := ""
					for node, nodeState := range status.GetNodeStates() {
						if nodeState.GetRole() == "Primary" {
							activeNode = node
							break
						}
					}
					log.Debug("haList: resource status", zap.String("resource", cfg.GetResource()), zap.String("active_node", activeNode), zap.String("role", status.GetRole()))
					if activeNode != "" {
						fmt.Printf("      Active:    %s\n", activeNode)
					} else if status.GetRole() != "" {
						fmt.Printf("      Role:      %s\n", status.GetRole())
					}
				} else {
					log.Debug("haList: resource status unavailable", zap.String("resource", cfg.GetResource()), zap.Error(err))
				}
				if cfg.GetMountPoint() != "" {
					fmt.Printf("      Mount:     %s (%s)\n", cfg.GetMountPoint(), cfg.GetFsType())
				}
				if len(cfg.GetServices()) > 0 {
					fmt.Printf("      Services:  %v\n", cfg.GetServices())
				}
				if cfg.GetVip() != "" {
					fmt.Printf("      VIP:       %s\n", cfg.GetVip())
				}
				fmt.Println()
			}

			if statusErr != nil {
				log.Debug("haList: warning that promoter status was unavailable", zap.Error(statusErr))
				fmt.Printf("Warning: failed to fetch HA status from controller: %v\n", statusErr)
			}

			log.Debug("haList: done", zap.Int("count", len(configs)))
			return nil
		},
	}

	return cmd
}

func haStatus() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "status <resource>",
		Short: "Show an HA resource's service chain, standbys and what else uses its mounts",
		Long: "Show where a resource's drbd-reactor chain runs and what each unit of it is, whether\n" +
			"each standby could take it over (and why not), and which services outside the chain\n" +
			"use its mounts, which would hold up the unmount on a failover. Works for `ha create`\n" +
			"resources, gateways and apps alike.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			log := zap.L()
			log.Debug("haStatus: running command", zap.Strings("args", args))
			resource := args[0]
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			haifyClient, err := newHaifyClient()
			if err != nil {
				log.Debug("haStatus: newHaifyClient failed", zap.Error(err))
				return fmt.Errorf("failed to connect to controller: %w", err)
			}
			defer closeClient(haifyClient)

			// A gateway's or an app's promoter has no `ha create` config but
			// the same kind of chain, so a missing config is not the end.
			cfg, cfgErr := haifyClient.GetHa(ctx, resource)
			haStatuses, haStatusErr := haifyClient.GetHaStatus(ctx, resource)
			log.Debug("haStatus: config and promoter status fetched", zap.String("resource", resource), zap.NamedError("config_err", cfgErr),
				zap.Int("promoters", len(haStatuses)), zap.NamedError("status_err", haStatusErr))
			if cfgErr != nil && (haStatusErr != nil || len(haStatuses) == 0) {
				log.Debug("haStatus: neither an HA config nor a promoter found", zap.String("resource", resource))
				return fmt.Errorf("failed to get HA config: %w", cfgErr)
			}
			var promoter *haifypb.HaPromoterStatus
			if haStatusErr == nil && len(haStatuses) > 0 {
				promoter = haStatuses[0]
			}
			resourceStatus, statusErr := haifyClient.ResourceStatus(ctx, resource)
			log.Debug("haStatus: resource status fetched", zap.String("resource", resource), zap.Bool("has_promoter", promoter != nil),
				zap.String("role", resourceStatus.GetRole()), zap.Error(statusErr))

			fmt.Printf("HA Configuration: %s\n", resource)
			if cfgErr == nil {
				fmt.Printf("  Config:  controller database\n")
			} else {
				fmt.Printf("  Config:  promoter of a gateway or app\n")
			}
			if statusErr == nil && resourceStatus != nil {
				fmt.Printf("  Role:    %s\n", resourceStatus.GetRole())
				fmt.Printf("  Nodes:   %v\n", resourceStatus.GetNodes())
			}
			if promoter != nil {
				fmt.Printf("  Status:  %s %s\n", statusIcon(promoter.GetStatus()), promoter.GetStatus())
				if promoter.GetActiveNode() != "" {
					fmt.Printf("  Active:  %s\n", promoter.GetActiveNode())
				}
				if promoter.GetPrimaryOn() != "" {
					fmt.Printf("  Primary: %s\n", promoter.GetPrimaryOn())
				}
			} else if haStatusErr != nil {
				log.Debug("haStatus: reactor status unavailable", zap.Error(haStatusErr))
				fmt.Printf("  Reactor: unavailable (%v)\n", haStatusErr)
			}
			if (promoter == nil || promoter.GetActiveNode() == "") && statusErr == nil && resourceStatus != nil {
				for node, nodeState := range resourceStatus.GetNodeStates() {
					if nodeState.GetRole() == "Primary" {
						fmt.Printf("  Primary: %s (DRBD)\n", node)
						break
					}
				}
			}
			if cfgErr == nil {
				if cfg.GetMountPoint() != "" {
					fmt.Printf("  Mount:   %s (%s)\n", cfg.GetMountPoint(), cfg.GetFsType())
				}
				if cfg.GetVip() != "" {
					fmt.Printf("  VIP:     %s\n", cfg.GetVip())
				}
				if len(cfg.GetServices()) > 0 {
					fmt.Printf("  Services: %v\n", cfg.GetServices())
				}
			}
			if statusErr == nil && resourceStatus != nil && len(resourceStatus.GetNodeStates()) > 0 {
				fmt.Printf("  Node States:\n")
				for node, nodeState := range resourceStatus.GetNodeStates() {
					fmt.Printf("    - %s: role=%s disk=%s repl=%s\n",
						node,
						nodeState.GetRole(),
						nodeState.GetDiskState(),
						nodeState.GetReplicationState())
				}
			}
			if promoter != nil {
				printHaChain(os.Stdout, promoter)
				printHaStandbys(os.Stdout, promoter)
				printHaOutside(os.Stdout, promoter)
			}

			log.Debug("haStatus: done")
			return nil
		},
	}

	return cmd
}

// statusIcon returns a visual indicator for service status.
func statusIcon(status string) string {
	switch status {
	case "active":
		return "●"
	case "inactive":
		return "○"
	case "failed":
		return "✗"
	default:
		return "?"
	}
}

func haSetTiebreaker() *cobra.Command {
	var node string
	var remove bool

	cmd := &cobra.Command{
		Use:   "set-tiebreaker <resource> --node <node>",
		Short: "Move a resource's diskless quorum tiebreaker to another node",
		Long: `Move a resource's diskless quorum tiebreaker to another node, live.

The tiebreaker holds no data; it exists so the survivors of a node failure
still have a quorum majority. It should therefore sit in a different failure
domain from the diskful replicas — a tiebreaker on the same physical host as a
replica means losing that host costs two of three votes and the survivor
suspends I/O.

The change is config-only: nothing resyncs and a promoted resource keeps
serving through it.

  haify ha set-tiebreaker data --node node-e     # move it
  haify ha set-tiebreaker data --remove          # drop it (accepts the quorum risk)`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			log := zap.L()
			log.Debug("haSetTiebreaker: running command", zap.Strings("args", args), zap.String("node", node), zap.Bool("remove", remove))
			resource := args[0]
			if remove {
				log.Debug("haSetTiebreaker: removing the tiebreaker")
				node = ""
			} else if node == "" {
				log.Debug("haSetTiebreaker: returning early", zap.String("condition", "node == \"\""))
				return fmt.Errorf("--node is required (or --remove to drop the tiebreaker)")
			}

			ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
			defer cancel()

			haifyClient, err := newHaifyClient()
			if err != nil {
				log.Debug("haSetTiebreaker: newHaifyClient failed", zap.Error(err))
				return fmt.Errorf("failed to connect to controller: %w", err)
			}
			defer closeClient(haifyClient)

			previous, message, err := haifyClient.SetTiebreaker(ctx, resource, node)
			if err != nil {
				log.Debug("haSetTiebreaker: SetTiebreaker failed", zap.Error(err))
				return fmt.Errorf("failed to set tiebreaker: %w", err)
			}

			log.Debug("haSetTiebreaker: tiebreaker set", zap.String("resource", resource), zap.String("previous", previous), zap.String("node", node))
			if previous == "" {
				previous = "(none)"
			}
			if node == "" {
				log.Debug("haSetTiebreaker: tiebreaker removed")
				fmt.Printf("Tiebreaker removed from %q (was %s)\n", resource, previous)
				// Whether losing the tiebreaker matters depends on how many
				// replicas are left, so report what the controller found rather
				// than warning unconditionally.
				fmt.Printf("%s\n", message)
				return nil
			}
			fmt.Printf("Tiebreaker for %q moved: %s -> %s\n", resource, previous, node)
			log.Debug("haSetTiebreaker: done")
			return nil
		},
	}

	cmd.Flags().StringVar(&node, "node", "", "Node to host the diskless quorum tiebreaker")
	cmd.Flags().BoolVar(&remove, "remove", false, "Remove the tiebreaker instead of moving it")
	return cmd
}
