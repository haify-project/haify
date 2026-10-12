package main

import (
	"fmt"
	"sort"
	"strings"

	haifypb "github.com/haify-project/haify/api/proto/v1"
	"github.com/spf13/cobra"
	"go.uber.org/zap"
)

func profileCreateValue(value, profile string) string {
	if value == "" && profile != "" {
		return "from profile " + profile
	}
	return displayValue(value)
}

func resourceProfileCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "profile",
		Short: "Resource profile management",
	}
	cmd.AddCommand(resourceProfileCreate())
	cmd.AddCommand(resourceProfileGet())
	cmd.AddCommand(resourceProfileList())
	cmd.AddCommand(resourceProfileDelete())
	cmd.AddCommand(resourceProfileSetOptions())
	cmd.AddCommand(resourceProfileAdjust())
	cmd.AddCommand(resourceProfileMaxSize())
	return cmd
}

func printMemberResults(members []*haifypb.ProfileMemberResult) {
	log := zap.L()
	log.Debug("printMemberResults: printing member results", zap.Int("members", len(members)))
	for _, m := range members {
		log.Debug("printMemberResults: member", zap.String("resource", m.Resource), zap.Bool("success", m.Success), zap.String("message", m.Message))
		mark := "ok    "
		if !m.Success {
			mark = "FAILED"
		}
		fmt.Printf("  %s %-24s %s\n", mark, m.Resource, m.Message)
	}
}

func resourceProfileSetOptions() *cobra.Command {
	var options map[string]string
	cmd := &cobra.Command{
		Use:   "set-options <profile> --drbd-options key=value[,...]",
		Short: "Set DRBD options on a profile and on every resource in it",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			log := zap.L()
			// Option values are not traced: net/shared-secret is one of them.
			log.Debug("resourceProfileSetOptions: running command", zap.Strings("args", args), zap.Int("options", len(options)))
			if len(options) == 0 {
				log.Debug("resourceProfileSetOptions: no --drbd-options given")
				return fmt.Errorf("give at least one --drbd-options key=value")
			}
			haifyClient, err := newHaifyClient()
			if err != nil {
				log.Debug("resourceProfileSetOptions: newHaifyClient failed", zap.Error(err))
				return fmt.Errorf("failed to connect to controller: %w", err)
			}
			defer closeClient(haifyClient)
			resp, err := haifyClient.SetResourceProfileOptions(cmd.Context(), args[0], options)
			if err != nil {
				log.Debug("resourceProfileSetOptions: SetResourceProfileOptions failed", zap.Error(err))
				return err
			}
			fmt.Println(resp.Message)
			printMemberResults(resp.Members)
			if !resp.Success {
				log.Debug("resourceProfileSetOptions: not every member took the options", zap.String("message", resp.Message))
				return fmt.Errorf("not every member took the options")
			}
			log.Debug("resourceProfileSetOptions: done", zap.Int("members", len(resp.Members)))
			return nil
		},
	}
	cmd.Flags().StringToStringVar(&options, "drbd-options", nil, "DRBD options as key=value pairs (e.g., net/max-buffers=8000)")
	return cmd
}

func resourceProfileAdjust() *cobra.Command {
	var dryRun bool
	cmd := &cobra.Command{
		Use:   "adjust <profile>",
		Short: "Bring every resource in a profile into line with it (options, missing replicas)",
		Long: "Applies the profile's DRBD options to each member and adds replicas to members\n" +
			"with fewer than the profile asks for, placed by its pool and label constraints.\n" +
			"Replicas beyond the profile's count are reported, never removed.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			log := zap.L()
			log.Debug("resourceProfileAdjust: running command", zap.Strings("args", args), zap.Bool("dry_run", dryRun))
			haifyClient, err := newHaifyClient()
			if err != nil {
				log.Debug("resourceProfileAdjust: newHaifyClient failed", zap.Error(err))
				return fmt.Errorf("failed to connect to controller: %w", err)
			}
			defer closeClient(haifyClient)
			resp, err := haifyClient.AdjustResourceProfile(cmd.Context(), args[0], dryRun)
			if err != nil {
				log.Debug("resourceProfileAdjust: AdjustResourceProfile failed", zap.Error(err))
				return err
			}
			fmt.Println(resp.Message)
			printMemberResults(resp.Members)
			if !resp.Success {
				log.Debug("resourceProfileAdjust: not every member could be adjusted", zap.String("message", resp.Message))
				return fmt.Errorf("not every member could be adjusted")
			}
			log.Debug("resourceProfileAdjust: done", zap.Int("members", len(resp.Members)))
			return nil
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Show what would change without changing it")
	return cmd
}

func resourceProfileMaxSize() *cobra.Command {
	return &cobra.Command{
		Use:   "max-size <profile>",
		Short: "Largest volume a new resource in this profile could get now",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			log := zap.L()
			log.Debug("resourceProfileMaxSize: running command", zap.Strings("args", args))
			haifyClient, err := newHaifyClient()
			if err != nil {
				log.Debug("resourceProfileMaxSize: newHaifyClient failed", zap.Error(err))
				return fmt.Errorf("failed to connect to controller: %w", err)
			}
			defer closeClient(haifyClient)
			resp, err := haifyClient.GetResourceProfileMaxSize(cmd.Context(), args[0])
			if err != nil {
				log.Debug("resourceProfileMaxSize: GetResourceProfileMaxSize failed", zap.Error(err))
				return err
			}
			log.Debug("resourceProfileMaxSize: max size", zap.String("profile", args[0]), zap.Uint64("max_size_gb", resp.MaxSizeGb),
				zap.Strings("nodes", resp.Nodes), zap.Bool("thin", resp.Thin))
			note := ""
			if resp.Thin {
				note = " (thin pool: what fits without overcommitting)"
			}
			fmt.Printf("%d GB on %s%s\n", resp.MaxSizeGb, strings.Join(resp.Nodes, ", "), note)
			log.Debug("resourceProfileMaxSize: done")
			return nil
		},
	}
}

func resourceProfileCreate() *cobra.Command {
	profile := &haifypb.ResourceProfile{}
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create or replace a resource profile",
		RunE: func(cmd *cobra.Command, args []string) error {
			log := zap.L()
			log.Debug("resourceProfileCreate: running command", zap.Strings("args", args), zap.String("name", profile.Name),
				zap.String("protocol", profile.Protocol), zap.String("storage_type", profile.StorageType), zap.String("pool", profile.Pool),
				zap.Uint32("replicas", profile.Replicas), zap.Strings("replicas_on_different", profile.ReplicasOnDifferent),
				zap.Strings("replicas_on_same", profile.ReplicasOnSame), zap.Int("drbd_options", len(profile.DrbdOptions)),
				zap.Int("labels", len(profile.Labels)))
			if strings.TrimSpace(profile.Name) == "" {
				log.Debug("resourceProfileCreate: --name missing")
				return fmt.Errorf("profile name is required")
			}
			grpcClient, conn, err := newResourceGRPCClient()
			if err != nil {
				log.Debug("resourceProfileCreate: newResourceGRPCClient failed", zap.Error(err))
				return fmt.Errorf("failed to connect to controller: %w", err)
			}
			defer closeClient(conn)
			resp, err := grpcClient.CreateResourceProfile(cmd.Context(), &haifypb.CreateResourceProfileRequest{Profile: profile})
			if err != nil {
				log.Debug("resourceProfileCreate: CreateResourceProfile failed", zap.Error(err))
				return fmt.Errorf("failed to create resource profile: %w", err)
			}
			if !resp.Success {
				log.Debug("resourceProfileCreate: controller reported failure", zap.String("message", resp.Message))
				return fmt.Errorf("failed to create resource profile: %s", resp.Message)
			}
			fmt.Printf("Resource profile '%s' saved\n", profile.Name)
			printResourceProfile(resp.Profile)
			log.Debug("resourceProfileCreate: done")
			return nil
		},
	}
	cmd.Flags().StringVar(&profile.Name, "name", "", "Profile name (required)")
	cmd.Flags().StringVar(&profile.Protocol, "protocol", "", "Default DRBD protocol (A, B, or C)")
	cmd.Flags().StringVar(&profile.StorageType, "storage-type", "", "Default storage type: lvm, lvm-thin, or zfs")
	cmd.Flags().StringVar(&profile.Pool, "pool", "", "Default storage pool")
	cmd.Flags().Uint32Var(&profile.Replicas, "replicas", 0, "Default replica count")
	cmd.Flags().StringSliceVar(&profile.ReplicasOnDifferent, "replicas-on-different", nil, "Node-label key(s) to spread replicas across (repeatable)")
	cmd.Flags().StringSliceVar(&profile.ReplicasOnSame, "replicas-on-same", nil, "Node-label key(s) all replicas must share (repeatable)")
	cmd.Flags().StringToStringVar(&profile.DrbdOptions, "drbd-options", nil, "Default DRBD options as key=value pairs")
	cmd.Flags().StringToStringVar(&profile.Labels, "label", nil, "Default resource label as key=value (repeatable)")
	_ = cmd.MarkFlagRequired("name")
	return cmd
}

func resourceProfileGet() *cobra.Command {
	return &cobra.Command{
		Use:   "get <name>",
		Short: "Get a resource profile",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			log := zap.L()
			log.Debug("resourceProfileGet: running command", zap.Strings("args", args))
			grpcClient, conn, err := newResourceGRPCClient()
			if err != nil {
				log.Debug("resourceProfileGet: newResourceGRPCClient failed", zap.Error(err))
				return fmt.Errorf("failed to connect to controller: %w", err)
			}
			defer closeClient(conn)
			resp, err := grpcClient.GetResourceProfile(cmd.Context(), &haifypb.GetResourceProfileRequest{Name: args[0]})
			if err != nil {
				log.Debug("resourceProfileGet: GetResourceProfile failed", zap.Error(err))
				return fmt.Errorf("failed to get resource profile: %w", err)
			}
			if !resp.Success {
				log.Debug("resourceProfileGet: controller reported failure", zap.String("message", resp.Message))
				return fmt.Errorf("failed to get resource profile: %s", resp.Message)
			}
			printResourceProfile(resp.Profile)
			members, err := grpcClient.ListResources(cmd.Context(), &haifypb.ListResourcesRequest{Profile: args[0]})
			if err == nil && members.Success {
				names := make([]string, 0, len(members.Resources))
				for _, r := range members.Resources {
					names = append(names, r.Name)
				}
				sort.Strings(names)
				log.Debug("resourceProfileGet: members", zap.String("profile", args[0]), zap.Strings("members", names))
				fmt.Printf("  Members: %s\n", displayValue(strings.Join(names, ", ")))
			} else {
				log.Debug("resourceProfileGet: members unavailable, not shown", zap.Error(err), zap.String("message", members.GetMessage()))
			}
			log.Debug("resourceProfileGet: done")
			return nil
		},
	}
}

func resourceProfileList() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List resource profiles",
		RunE: func(cmd *cobra.Command, args []string) error {
			log := zap.L()
			log.Debug("resourceProfileList: running command", zap.Strings("args", args))
			grpcClient, conn, err := newResourceGRPCClient()
			if err != nil {
				log.Debug("resourceProfileList: newResourceGRPCClient failed", zap.Error(err))
				return fmt.Errorf("failed to connect to controller: %w", err)
			}
			defer closeClient(conn)
			resp, err := grpcClient.ListResourceProfiles(cmd.Context(), &haifypb.ListResourceProfilesRequest{})
			if err != nil {
				log.Debug("resourceProfileList: ListResourceProfiles failed", zap.Error(err))
				return fmt.Errorf("failed to list resource profiles: %w", err)
			}
			if !resp.Success {
				log.Debug("resourceProfileList: controller reported failure", zap.String("message", resp.Message))
				return fmt.Errorf("failed to list resource profiles: %s", resp.Message)
			}
			if len(resp.Profiles) == 0 {
				log.Debug("resourceProfileList: no profiles")
				fmt.Println("No resource profiles found")
				return nil
			}
			sort.Slice(resp.Profiles, func(i, j int) bool { return resp.Profiles[i].Name < resp.Profiles[j].Name })
			for _, profile := range resp.Profiles {
				log.Debug("resourceProfileList: profile", zap.String("profile", profile.Name), zap.String("pool", profile.Pool), zap.Uint32("replicas", profile.Replicas))
				fmt.Println(formatResourceProfile(profile))
			}
			log.Debug("resourceProfileList: done", zap.Int("count", len(resp.Profiles)))
			return nil
		},
	}
}

func resourceProfileDelete() *cobra.Command {
	return &cobra.Command{
		Use:   "delete <name>",
		Short: "Delete a resource profile",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			log := zap.L()
			log.Debug("resourceProfileDelete: running command", zap.Strings("args", args))
			grpcClient, conn, err := newResourceGRPCClient()
			if err != nil {
				log.Debug("resourceProfileDelete: newResourceGRPCClient failed", zap.Error(err))
				return fmt.Errorf("failed to connect to controller: %w", err)
			}
			defer closeClient(conn)
			resp, err := grpcClient.DeleteResourceProfile(cmd.Context(), &haifypb.DeleteResourceProfileRequest{Name: args[0]})
			if err != nil {
				log.Debug("resourceProfileDelete: DeleteResourceProfile failed", zap.Error(err))
				return fmt.Errorf("failed to delete resource profile: %w", err)
			}
			if !resp.Success {
				log.Debug("resourceProfileDelete: controller reported failure", zap.String("message", resp.Message))
				return fmt.Errorf("failed to delete resource profile: %s", resp.Message)
			}
			fmt.Printf("Resource profile '%s' deleted successfully\n", args[0])
			log.Debug("resourceProfileDelete: done")
			return nil
		},
	}
}

func printResourceProfile(profile *haifypb.ResourceProfile) {
	log := zap.L()
	if profile == nil {
		log.Debug("printResourceProfile: no profile to print")
		return
	}
	log.Debug("printResourceProfile: printing profile", zap.String("profile", profile.Name), zap.String("pool", profile.Pool),
		zap.Uint32("replicas", profile.Replicas), zap.Int("drbd_options", len(profile.DrbdOptions)), zap.Int("labels", len(profile.Labels)))
	fmt.Printf("Profile: %s\n", profile.Name)
	fmt.Printf("  Protocol:              %s\n", displayValue(profile.Protocol))
	fmt.Printf("  Storage type:          %s\n", displayValue(profile.StorageType))
	fmt.Printf("  Pool:                  %s\n", displayValue(profile.Pool))
	fmt.Printf("  Replicas:              %d\n", profile.Replicas)
	fmt.Printf("  Replicas on different: %s\n", formatStringSlice(profile.ReplicasOnDifferent))
	fmt.Printf("  Replicas on same:      %s\n", formatStringSlice(profile.ReplicasOnSame))
	fmt.Printf("  DRBD options:          %s\n", formatLabels(profile.DrbdOptions))
	fmt.Printf("  Labels:                %s\n", formatLabels(profile.Labels))
}

func formatResourceProfile(profile *haifypb.ResourceProfile) string {
	if profile == nil {
		return "(invalid profile)"
	}
	return fmt.Sprintf("%s (protocol=%s, storage-type=%s, pool=%s, replicas=%d, replicas-on-different=%s, replicas-on-same=%s, drbd-options=%s, labels=%s)",
		profile.Name, displayValue(profile.Protocol), displayValue(profile.StorageType), displayValue(profile.Pool), profile.Replicas,
		formatStringSlice(profile.ReplicasOnDifferent), formatStringSlice(profile.ReplicasOnSame), formatLabels(profile.DrbdOptions), formatLabels(profile.Labels))
}

func formatStringSlice(values []string) string {
	if len(values) == 0 {
		return "(none)"
	}
	result := append([]string(nil), values...)
	sort.Strings(result)
	return strings.Join(result, ", ")
}

func resourceSetProfile() *cobra.Command {
	var none bool
	cmd := &cobra.Command{
		Use:   "set-profile <resource> [profile]",
		Short: "Make a resource a member of a profile (its options are applied), or --none to take it out",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			log := zap.L()
			log.Debug("resourceSetProfile: running command", zap.Strings("args", args), zap.Bool("none", none))
			profile := ""
			switch {
			case none && len(args) == 2:
				log.Debug("resourceSetProfile: both a profile and --none given")
				return fmt.Errorf("give a profile or --none, not both")
			case !none && len(args) == 1:
				log.Debug("resourceSetProfile: neither a profile nor --none given")
				return fmt.Errorf("give a profile, or --none to take %s out of its profile", args[0])
			case len(args) == 2:
				profile = args[1]
			}
			log.Debug("resourceSetProfile: setting profile", zap.String("resource", args[0]), zap.String("profile", profile))
			haifyClient, err := newHaifyClient()
			if err != nil {
				log.Debug("resourceSetProfile: newHaifyClient failed", zap.Error(err))
				return fmt.Errorf("failed to connect to controller: %w", err)
			}
			defer closeClient(haifyClient)
			if err := haifyClient.SetResourceProfile(cmd.Context(), args[0], profile); err != nil {
				log.Debug("resourceSetProfile: SetResourceProfile failed", zap.Error(err))
				return err
			}
			if profile == "" {
				fmt.Printf("%s is in no profile\n", args[0])
			} else {
				fmt.Printf("%s is a member of %s; run resource profile adjust %s to bring its replicas into line\n", args[0], profile, profile)
			}
			log.Debug("resourceSetProfile: done")
			return nil
		},
	}
	cmd.Flags().BoolVar(&none, "none", false, "Take the resource out of its profile")
	return cmd
}
