package main

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"
	"go.uber.org/zap"

	haifypb "github.com/haify-project/haify/api/proto/v1"
	"github.com/haify-project/haify/pkg/util"
)

func nfsExportQuota() *cobra.Command {
	var resource, path, size string
	cmd := &cobra.Command{
		Use:   "quota",
		Short: "Cap an export directory's size with an ext4 project quota (--size 0 removes it)",
		Long: `Cap an NFS export directory's size with an ext4 project quota, so one export
cannot fill the whole gateway volume. Gateways created from this version on
support it; an older one needs its filesystem given the feature while
unmounted (tune2fs -O quota,project <device>) and the gateway recreated. The
gateway nodes need the quota package (setquota).`,
		RunE: func(cmd *cobra.Command, args []string) error {
			log := zap.L()
			log.Debug("nfsExportQuota: running command", zap.Strings("args", args), zap.String("resource", resource), zap.String("path", path), zap.String("size", size))
			if resource == "" || size == "" {
				log.Debug("nfsExportQuota: returning early", zap.String("condition", "resource == \"\" || size == \"\""))
				return fmt.Errorf("--resource and --size are required")
			}
			bytes, err := util.ParseSize(size)
			if size == "0" {
				log.Debug("nfsExportQuota: size 0, removing the quota")
				bytes, err = 0, nil
			}
			if err != nil {
				log.Debug("nfsExportQuota: size not parsable", zap.String("size", size), zap.Error(err))
				return fmt.Errorf("invalid --size %q: %w", size, err)
			}
			log.Debug("nfsExportQuota: setting quota", zap.String("resource", resource), zap.Uint64("size_bytes", bytes))
			return withController(2*time.Minute, func(ctx context.Context, c haifypb.HaifyControllerClient) error {
				r, err := c.SetNFSExportQuota(ctx, &haifypb.SetNFSExportQuotaRequest{Resource: resource, ExportPath: path, SizeBytes: bytes})
				if err != nil {
					log.Debug("nfsExportQuota: SetNFSExportQuota failed", zap.Error(err))
					return err
				}
				return resultLine(r.Success, r.Message)
			})
		},
	}
	cmd.Flags().StringVar(&resource, "resource", "", "Resource the NFS gateway serves")
	cmd.Flags().StringVar(&path, "path", "", "Export path (default: the gateway's own)")
	cmd.Flags().StringVar(&size, "size", "", "Limit, e.g. 100G; 0 removes it")
	return cmd
}
