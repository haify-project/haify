package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"go.uber.org/zap"
)

func healthCommand() *cobra.Command {
	var nodes string

	cmd := &cobra.Command{
		Use:   "health-check [node...]",
		Short: "Check node health (drbd, drbd-reactor, resource-agents)",
		RunE: func(cmd *cobra.Command, args []string) error {
			log := zap.L()
			log.Debug("healthCommand: running command", zap.Strings("args", args), zap.String("nodes", nodes))
			// Nodes may be named as arguments too; they used to be ignored
			// without a word and every node checked instead.
			if len(args) > 0 {
				nodes = strings.Join(append(args, strings.Split(nodes, ",")...), ",")
				nodes = strings.Trim(nodes, ",")
				log.Debug("healthCommand: nodes merged from arguments and --nodes", zap.String("nodes", nodes))
			}
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()

			haifyClient, err := newHaifyClient()
			if err != nil {
				log.Debug("healthCommand: newHaifyClient failed", zap.Error(err))
				return fmt.Errorf("failed to connect to controller: %w", err)
			}
			defer closeClient(haifyClient)

			// Get nodes to check
			var nodeList []string
			if nodes != "" {
				nodeList = strings.Split(nodes, ",")
				for i := range nodeList {
					nodeList[i] = strings.TrimSpace(nodeList[i])
				}
				log.Debug("healthCommand: checking the given nodes", zap.Strings("nodes", nodeList))
			} else {
				// Get all registered nodes
				log.Debug("healthCommand: no nodes given, checking every registered node")
				nodeInfos, err := haifyClient.ListNodes(ctx)
				if err != nil {
					log.Debug("healthCommand: ListNodes failed", zap.Error(err))
					return fmt.Errorf("failed to list nodes: %w", err)
				}
				if len(nodeInfos) == 0 {
					log.Debug("healthCommand: returning early", zap.String("condition", "len(nodeInfos) == 0"))
					return fmt.Errorf("no registered nodes found")
				}
				for _, n := range nodeInfos {
					nodeList = append(nodeList, n.Address)
				}
				log.Debug("healthCommand: registered nodes", zap.Strings("nodes", nodeList))
			}

			// Check health for each node
			allHealthy := true
			for _, node := range nodeList {
				fmt.Printf("\n=== Node: %s ===\n", node)
				healthy, err := haifyClient.HealthCheck(ctx, node)
				if err != nil {
					log.Debug("healthCommand: HealthCheck failed", zap.String("node", node), zap.Error(err))
					fmt.Printf("  Error: %v\n", err)
					allHealthy = false
					continue
				}

				// Print DRBD status
				log.Debug("healthCommand: node health", zap.String("node", node), zap.Bool("drbd", healthy.DrbdInstalled),
					zap.String("drbd_version", healthy.DrbdVersion), zap.Bool("reactor", healthy.DrbdReactorInstalled),
					zap.Bool("reactor_running", healthy.DrbdReactorRunning), zap.Bool("resource_agents", healthy.ResourceAgentsInstalled),
					zap.Int("agents", len(healthy.AvailableAgents)))
				if healthy.DrbdInstalled {
					fmt.Printf("  [OK] DRBD: %s\n", healthy.DrbdVersion)
				} else {
					fmt.Printf("  [MISSING] DRBD not installed\n")
					allHealthy = false
				}

				// Print drbd-reactor status
				if healthy.DrbdReactorInstalled {
					fmt.Printf("  [OK] drbd-reactor: %s\n", healthy.DrbdReactorVersion)
					if healthy.DrbdReactorRunning {
						fmt.Printf("  [OK] drbd-reactor service: running\n")
					} else {
						fmt.Printf("  [WARN] drbd-reactor service: not running\n")
					}
				} else {
					fmt.Printf("  [MISSING] drbd-reactor not installed\n")
					allHealthy = false
				}

				// Print resource-agents-extra status
				if healthy.ResourceAgentsInstalled {
					fmt.Printf("  [OK] resource-agents-extra: installed\n")
					if len(healthy.AvailableAgents) > 0 {
						fmt.Printf("  [INFO] Available agents: %s\n", strings.Join(healthy.AvailableAgents, ", "))
					}
				} else {
					fmt.Printf("  [MISSING] resource-agents-extra not installed\n")
					allHealthy = false
				}
			}

			fmt.Println()
			if allHealthy {
				log.Debug("healthCommand: all nodes healthy", zap.Int("nodes", len(nodeList)))
				fmt.Println("Overall: All nodes are healthy")
				return nil
			}
			log.Debug("healthCommand: some nodes have issues", zap.Int("nodes", len(nodeList)))
			fmt.Println("Overall: Some nodes have issues")
			return fmt.Errorf("health check failed")
		},
	}

	cmd.Flags().StringVar(&nodes, "nodes", "", "Comma-separated nodes to check (default: all registered nodes)")

	return cmd
}
