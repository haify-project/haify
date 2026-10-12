package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	haifypb "github.com/haify-project/haify/api/proto/v1"
	"github.com/haify-project/haify/pkg/util"
	"github.com/spf13/cobra"
	"go.uber.org/zap"
)

func resourceCreate() *cobra.Command {
	var name string
	var port uint32
	var nodes string
	var replicas uint32
	var replicasOnDifferent []string
	var replicasOnSame []string
	var doNotPlaceWith []string
	var pool string
	var storageType string
	var protocol string
	var size string
	var exactSize bool
	var drbdOptions map[string]string
	var wan bool
	var drNode string
	var drEndpoint string
	var wanPort uint32
	var wanEgress string
	var profile string
	var labels map[string]string
	var encrypt bool

	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create a new DRBD resource",
		RunE: func(cmd *cobra.Command, args []string) error {
			log := zap.L()
			// DRBD option values are not traced: net/shared-secret is one of them.
			log.Debug("resourceCreate: running command", zap.Strings("args", args), zap.String("name", name), zap.Uint32("port", port),
				zap.String("nodes", nodes), zap.Uint32("replicas", replicas), zap.Strings("replicas_on_different", replicasOnDifferent),
				zap.Strings("replicas_on_same", replicasOnSame), zap.Strings("do_not_place_with", doNotPlaceWith), zap.String("pool", pool),
				zap.String("storage_type", storageType), zap.String("protocol", protocol), zap.String("size", size),
				zap.Bool("exact_size", exactSize), zap.Int("drbd_options", len(drbdOptions)), zap.Bool("wan", wan),
				zap.String("dr_node", drNode), zap.String("dr_endpoint", drEndpoint), zap.Uint32("wan_port", wanPort),
				zap.String("wan_egress_address", wanEgress), zap.String("profile", profile), zap.Int("labels", len(labels)),
				zap.Bool("encrypt", encrypt))
			ctx := cmd.Context()

			if name == "" {
				log.Debug("resourceCreate: --name missing")
				return fmt.Errorf("resource name is required")
			}
			if port == 0 {
				log.Debug("resourceCreate: --port missing")
				return fmt.Errorf("DRBD port is required (use --port)")
			}
			if size == "" {
				log.Debug("resourceCreate: --size missing")
				return fmt.Errorf("size is required (use --size)")
			}

			// WAN master switch (mirrors the server guard): the --dr-* / --wan-port
			// flags only apply with --wan.
			if !wan && (drNode != "" || drEndpoint != "" || wanPort != 0 || wanEgress != "") {
				log.Debug("resourceCreate: WAN flags given without --wan")
				return fmt.Errorf("--dr-node/--dr-endpoint/--wan-port/--wan-egress-address require --wan")
			}

			var nodeList []string
			if nodes != "" {
				nodeList = strings.Split(nodes, ",")
			}
			// No --nodes ⇒ auto-placement: the controller picks the nodes with the
			// most free space in --pool. WAN needs an explicit primary, so require
			// --nodes there.
			if len(nodeList) == 0 && !wan {
				log.Debug("resourceCreate: no --nodes, the controller auto-places", zap.Uint32("replicas", replicas))
			}
			if len(nodeList) == 0 && wan {
				log.Debug("resourceCreate: WAN resource without --nodes")
				return fmt.Errorf("WAN resource requires an explicit --nodes primary")
			}

			if wan {
				// --nodes lists the primary SITE: one node for the historic
				// two-endpoint resource, or several for a synchronous primary
				// site with one asynchronous DR copy (两地三中心). The DR node
				// is named separately with --dr-node.
				if len(nodeList) < 1 {
					log.Debug("resourceCreate: WAN resource without a primary-site node")
					return fmt.Errorf("WAN resource requires at least one primary-site node in --nodes")
				}
				if drNode == "" {
					log.Debug("resourceCreate: WAN resource without --dr-node")
					return fmt.Errorf("WAN resource requires --dr-node")
				}
				if drEndpoint == "" {
					log.Debug("resourceCreate: WAN resource without --dr-endpoint")
					return fmt.Errorf("WAN resource requires --dr-endpoint")
				}
				log.Debug("resourceCreate: WAN resource", zap.Strings("primary_site", nodeList), zap.String("dr_node", drNode))
			}

			if pool == "" && profile == "" {
				log.Debug("resourceCreate: no --pool or --profile, using data-pool")
				pool = "data-pool"
			}

			sizeBytes, err := util.ParseSize(size)
			if err != nil {
				log.Debug("resourceCreate: size not parsable", zap.String("size", size), zap.Error(err))
				return fmt.Errorf("invalid size format: %s: %w", size, err)
			}
			// Round up: a volume smaller than asked for is the one outcome a
			// size must never have. --exact-size keeps the exact byte count.
			sizeGiB := util.BytesToGiB(sizeBytes + (1<<30 - 1))
			if sizeGiB == 0 || (!exactSize && sizeBytes < 1<<30) {
				log.Debug("resourceCreate: size below 1 GiB without --exact-size", zap.Uint64("size_bytes", sizeBytes))
				return fmt.Errorf("size too small (minimum 1 GiB, or use --exact-size)")
			}
			var exactBytes uint64
			if exactSize {
				exactBytes = sizeBytes
			}
			log.Debug("resourceCreate: size", zap.Uint64("size_bytes", sizeBytes), zap.Uint64("size_gib", sizeGiB), zap.Uint64("exact_bytes", exactBytes))

			// Say it here rather than only in the docs. The distinction between
			// at-rest and in-transit is the one an operator is most likely to
			// get wrong about this flag, and getting it wrong means believing
			// replication traffic is protected when it is not.
			if encrypt {
				log.Debug("resourceCreate: --encrypt, printing the at-rest-only note")
				fmt.Fprintf(os.Stderr,
					"Note: --encrypt encrypts each replica's backing volume (at rest).\n"+
						"      DRBD sits above the crypt layer, so replication between nodes stays PLAINTEXT.\n"+
						"      Each node keeps its own key in /etc/haify/luks (root-only); deleting the\n"+
						"      resource destroys those keys, and there is no central escrow.\n")
			}

			grpcClient, conn, err := newResourceGRPCClient()
			if err != nil {
				log.Debug("resourceCreate: newResourceGRPCClient failed", zap.Error(err))
				return fmt.Errorf("failed to connect to controller: %w", err)
			}
			defer closeClient(conn)

			requestReplicas := replicas
			if profile != "" && !cmd.Flags().Changed("replicas") {
				requestReplicas = 0
			}
			requestPool := pool
			requestStorageType := storageType
			requestProtocol := protocol
			if profile != "" {
				if !cmd.Flags().Changed("pool") {
					requestPool = ""
				}
				if !cmd.Flags().Changed("storage-type") {
					requestStorageType = ""
				}
				if !cmd.Flags().Changed("protocol") {
					requestProtocol = ""
				}
			}
			if wan {
				requestProtocol = "A"
			}
			log.Debug("resourceCreate: request built", zap.Uint32("replicas", requestReplicas), zap.String("pool", requestPool),
				zap.String("storage_type", requestStorageType), zap.String("protocol", requestProtocol), zap.String("profile", profile))
			resp, err := grpcClient.CreateResource(ctx, &haifypb.CreateResourceRequest{
				Name:                name,
				Port:                port,
				Nodes:               nodeList,
				Protocol:            requestProtocol,
				SizeGb:              uint32(sizeGiB),
				SizeBytes:           exactBytes,
				Pool:                requestPool,
				StorageType:         requestStorageType,
				DrbdOptions:         drbdOptions,
				Wan:                 wan,
				DrNode:              drNode,
				DrEndpoint:          drEndpoint,
				WanPort:             wanPort,
				WanEgressAddress:    wanEgress,
				Replicas:            requestReplicas,
				ReplicasOnDifferent: replicasOnDifferent,
				ReplicasOnSame:      replicasOnSame,
				DoNotPlaceWith:      doNotPlaceWith,
				Labels:              labels,
				Profile:             profile,
				Encrypt:             encrypt,
			})
			if err != nil {
				log.Debug("resourceCreate: CreateResource failed", zap.Error(err))
				return fmt.Errorf("failed to create resource: %w", err)
			}
			if !resp.Success {
				log.Debug("resourceCreate: controller reported failure", zap.String("message", resp.Message))
				return fmt.Errorf("failed to create resource: %s", resp.Message)
			}

			log.Debug("resourceCreate: resource created", zap.String("resource", name), zap.String("message", resp.Message))
			fmt.Printf("Resource created successfully\n")
			fmt.Printf("  Name:        %s\n", name)
			fmt.Printf("  Port:        %d\n", port)
			fmt.Printf("  Storage:     %s\n", profileCreateValue(requestStorageType, profile))
			fmt.Printf("  Pool:        %s\n", profileCreateValue(requestPool, profile))
			if len(nodeList) == 0 {
				if requestReplicas == 0 {
					fmt.Printf("  Nodes:       auto-placed (replicas from profile; see 'resource get %s')\n", name)
				} else {
					fmt.Printf("  Nodes:       auto-placed (%d replicas by free space; see 'resource get %s')\n", requestReplicas, name)
				}
			} else {
				fmt.Printf("  Nodes:       %v\n", nodeList)
			}
			if _, warning, ok := strings.Cut(resp.Message, "; warning: "); ok {
				fmt.Printf("  Warning:     %s\n", warning)
			}
			if wan {
				fmt.Printf("  Protocol:    A (WAN)\n")
				fmt.Printf("  DR node:     %s\n", drNode)
				fmt.Printf("  DR endpoint: %s\n", drEndpoint)
				if wanPort != 0 {
					fmt.Printf("  WAN port:    %d\n", wanPort)
				} else {
					fmt.Printf("  WAN port:    auto (random >3000)\n")
				}
			} else {
				fmt.Printf("  Protocol:    %s\n", profileCreateValue(requestProtocol, profile))
			}
			fmt.Printf("  Size:        %d GiB (%s)\n", sizeGiB, util.FormatBytes(sizeBytes))
			if encrypt {
				fmt.Printf("  Encryption:  LUKS2 at rest (replication traffic is NOT encrypted)\n")
			}
			if len(drbdOptions) > 0 {
				fmt.Printf("  Options:     %v\n", drbdOptions)
			}
			if profile != "" {
				fmt.Printf("  Profile:     %s\n", profile)
			}
			if len(labels) > 0 {
				fmt.Printf("  Labels:      %s\n", formatLabels(labels))
			}
			fmt.Printf("\nNext steps:\n")
			fmt.Printf("  1. haify resource get %s\n", name)
			fmt.Printf("  2. haify resource primary %s <node>\n", name)

			log.Debug("resourceCreate: done")
			return nil
		},
	}

	cmd.Flags().StringVar(&name, "name", "", "Resource name (required)")
	cmd.Flags().Uint32Var(&port, "port", 0, "DRBD port (required)")
	cmd.Flags().StringVar(&nodes, "nodes", "", "Node names (comma-separated); omit to auto-place by free space")
	cmd.Flags().Uint32Var(&replicas, "replicas", 2, "Replica count for auto-placement (used only when --nodes is omitted)")
	cmd.Flags().StringSliceVar(&replicasOnDifferent, "replicas-on-different", nil, "Node-label key(s) to spread replicas across (e.g. rack); each replica gets a distinct value. Repeatable. Auto-placement only")
	cmd.Flags().StringSliceVar(&replicasOnSame, "replicas-on-same", nil, "Node-label key(s) all replicas must share (e.g. zone). Repeatable. Auto-placement only")
	cmd.Flags().StringSliceVar(&doNotPlaceWith, "do-not-place-with", nil, "Resource name(s) whose nodes to avoid (anti-affinity). Repeatable. Auto-placement only")
	cmd.Flags().StringVar(&pool, "pool", "", "Storage pool name (default: data-pool)")
	cmd.Flags().StringVar(&storageType, "storage-type", "lvm", "Storage type: lvm, lvm-thin, or zfs")
	cmd.Flags().StringVar(&protocol, "protocol", "C", "DRBD protocol (A, B, or C)")
	cmd.Flags().StringVar(&size, "size", "", "Volume size (e.g., 1G, 10GB, 1TB, 1GiB, required); rounded up to whole GiB")
	cmd.Flags().BoolVar(&exactSize, "exact-size", false, "Make the device exactly --size bytes (rounded up to 512) instead of whole GiB")
	cmd.Flags().StringToStringVar(&drbdOptions, "drbd-options", nil, "DRBD options as key=value pairs (e.g., on-no-quorum=suspend-io)")
	cmd.Flags().StringVar(&profile, "profile", "", "Resource profile name")
	cmd.Flags().StringToStringVar(&labels, "label", nil, "Resource label as key=value (repeatable)")
	cmd.Flags().BoolVar(&encrypt, "encrypt", false,
		"Encrypt each replica's backing volume with LUKS2 (DRBD -> LUKS -> LVM). "+
			"AT REST ONLY: DRBD is above the crypt layer, so replication traffic between nodes stays plaintext. "+
			"Each node generates and keeps its own key under /etc/haify/luks (root-only, never sent anywhere); "+
			"there is no central escrow and it cannot be enabled later. LVM pools only.")
	cmd.Flags().BoolVar(&wan, "wan", false, "Enable opt-in WAN replication (async protocol A to --dr-node via haify-proxy; --nodes may list several primary-site replicas)")
	cmd.Flags().StringVar(&drNode, "dr-node", "", "DR-site node name (requires --wan; must be a registered node)")
	cmd.Flags().StringVar(&drEndpoint, "dr-endpoint", "", "DR site's public WAN address the primary dials (requires --wan)")
	cmd.Flags().Uint32Var(&wanPort, "wan-port", 0, "WAN mTLS port (requires --wan; 0 = auto-pick a random port >3000)")
	cmd.Flags().StringVar(&wanEgress, "wan-egress-address", "",
		"source IP the primary's proxy dials out from, pinning WAN replication to one interface (requires --wan; empty = routing table decides)")

	_ = cmd.MarkFlagRequired("name")
	_ = cmd.MarkFlagRequired("port")
	// --nodes is intentionally NOT required: omitting it triggers auto-placement.
	_ = cmd.MarkFlagRequired("size")

	return cmd
}

func resourceAdopt() *cobra.Command {
	var nodes string
	var port uint32
	var protocol string

	cmd := &cobra.Command{
		Use:   "adopt <name>",
		Short: "Adopt an existing (foreign) DRBD resource into Haify management",
		Long: "Import an already-existing DRBD resource (created outside Haify) into\n" +
			"Haify management by recording its metadata. This never creates or\n" +
			"modifies the DRBD resource or its data — it only reads the live\n" +
			"/etc/drbd.d/<name>.res and records what it finds. Nodes, port and\n" +
			"protocol are auto-discovered from the config when not supplied.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			log := zap.L()
			log.Debug("resourceAdopt: running command", zap.Strings("args", args), zap.String("nodes", nodes), zap.Uint32("port", port), zap.String("protocol", protocol))
			name := args[0]

			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()

			var nodeList []string
			if nodes != "" {
				nodeList = strings.Split(nodes, ",")
			}

			haifyClient, err := newHaifyClient()
			if err != nil {
				log.Debug("resourceAdopt: newHaifyClient failed", zap.Error(err))
				return fmt.Errorf("failed to connect to controller: %w", err)
			}
			defer closeClient(haifyClient)

			resp, err := haifyClient.AdoptResource(ctx, name, nodeList, port, protocol)
			if err != nil {
				log.Debug("resourceAdopt: AdoptResource failed", zap.Error(err))
				return fmt.Errorf("failed to adopt resource: %w", err)
			}

			log.Debug("resourceAdopt: adopted", zap.String("resource", name), zap.Strings("nodes", resp.Nodes), zap.Uint32("port", resp.Port),
				zap.String("protocol", resp.Protocol), zap.Uint32("volumes", resp.Volumes))
			fmt.Printf("Resource '%s' adopted into Haify management\n", name)
			fmt.Printf("  Nodes:    %v\n", resp.Nodes)
			fmt.Printf("  Port:     %d\n", resp.Port)
			fmt.Printf("  Protocol: %s\n", resp.Protocol)
			fmt.Printf("  Volumes:  %d\n", resp.Volumes)
			fmt.Printf("\nThe DRBD resource and its data were not modified.\n")
			fmt.Printf("Next: haify ha create %s\n", name)

			log.Debug("resourceAdopt: done")
			return nil
		},
	}

	cmd.Flags().StringVar(&nodes, "nodes", "", "Node names (comma-separated); auto-discovered from the .res when omitted")
	cmd.Flags().Uint32Var(&port, "port", 0, "DRBD port; auto-discovered from the .res when omitted")
	cmd.Flags().StringVar(&protocol, "protocol", "", "DRBD protocol (A, B, or C); defaults to C when omitted")

	return cmd
}
