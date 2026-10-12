package main

import (
	"fmt"
	"slices"
	"strings"

	"github.com/spf13/cobra"
	"go.uber.org/zap"
)

func gatewayGet() *cobra.Command {
	var resource string

	cmd := &cobra.Command{
		Use:   "get --resource <name>",
		Short: "Get gateway details",
		RunE: func(cmd *cobra.Command, args []string) error {
			log := zap.L()
			log.Debug("gatewayGet: running command", zap.Strings("args", args), zap.String("resource", resource))
			if resource == "" {
				log.Debug("gatewayGet: returning early", zap.String("condition", "resource == \"\""))
				return fmt.Errorf("--resource is required")
			}

			haifyClient, err := newHaifyClient()
			if err != nil {
				log.Debug("gatewayGet: newHaifyClient failed", zap.Error(err))
				return fmt.Errorf("failed to connect to controller: %w", err)
			}
			defer closeClient(haifyClient)

			gw, err := haifyClient.GetGateway(cmd.Context(), resource)
			if err != nil {
				log.Debug("gatewayGet: GetGateway failed", zap.Error(err))
				return fmt.Errorf("failed to get gateway: %w", err)
			}

			log.Debug("gatewayGet: gateway", zap.String("resource", gw.Resource), zap.String("type", gw.Type),
				zap.String("state", gw.State), zap.String("node", gw.Node), zap.Int("options", len(gw.Options)))
			fmt.Printf("Resource: %s\n", gw.Resource)
			fmt.Printf("Type:     %s\n", gw.Type)
			fmt.Printf("State:    %s\n", emptyGatewayValue(gw.State, "unknown"))
			fmt.Printf("Node:     %s\n", emptyGatewayValue(gw.Node, "-"))
			if gw.Path != "" {
				fmt.Printf("Path:     %s\n", gw.Path)
			}
			if len(gw.Options) > 0 {
				fmt.Printf("Details:\n")
				for _, key := range sortedGatewayOptionKeys(gw.Options) {
					fmt.Printf("  %s: %s\n", key, gw.Options[key])
				}
			}

			log.Debug("gatewayGet: done")
			return nil
		},
	}

	cmd.Flags().StringVar(&resource, "resource", "", "DRBD resource name")
	_ = cmd.MarkFlagRequired("resource")

	return cmd
}

func gatewayStatus() *cobra.Command {
	var resource string

	cmd := &cobra.Command{
		Use:   "status --resource <name>",
		Short: "Show gateway status",
		RunE: func(cmd *cobra.Command, args []string) error {
			log := zap.L()
			log.Debug("gatewayStatus: running command", zap.Strings("args", args), zap.String("resource", resource))
			if resource == "" {
				log.Debug("gatewayStatus: returning early", zap.String("condition", "resource == \"\""))
				return fmt.Errorf("--resource is required")
			}

			haifyClient, err := newHaifyClient()
			if err != nil {
				log.Debug("gatewayStatus: newHaifyClient failed", zap.Error(err))
				return fmt.Errorf("failed to connect to controller: %w", err)
			}
			defer closeClient(haifyClient)

			gw, err := haifyClient.GetGateway(cmd.Context(), resource)
			if err != nil {
				log.Debug("gatewayStatus: GetGateway failed", zap.Error(err))
				return fmt.Errorf("failed to get gateway status: %w", err)
			}

			log.Debug("gatewayStatus: gateway", zap.String("resource", gw.Resource), zap.String("type", gw.Type),
				zap.String("state", gw.State), zap.String("node", gw.Node), zap.String("role", gw.Options["role"]))
			fmt.Printf("%s %s\n", strings.ToUpper(gw.Type), gw.Resource)
			fmt.Printf("  state: %s\n", emptyGatewayValue(gw.State, "unknown"))
			fmt.Printf("  node:  %s\n", emptyGatewayValue(gw.Node, "-"))
			if role := gw.Options["role"]; role != "" {
				fmt.Printf("  role:  %s\n", role)
			}
			if volumes := gw.Options["volumes"]; volumes != "" {
				fmt.Printf("  vols:  %s\n", volumes)
			}
			if path := gw.Path; path != "" {
				fmt.Printf("  path:  %s\n", path)
			}

			log.Debug("gatewayStatus: done")
			return nil
		},
	}

	cmd.Flags().StringVar(&resource, "resource", "", "DRBD resource name")
	_ = cmd.MarkFlagRequired("resource")

	return cmd
}

func sortedGatewayOptionKeys(options map[string]string) []string {
	keys := make([]string, 0, len(options))
	for key := range options {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

func emptyGatewayValue(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}
