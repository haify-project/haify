package controller

import (
	"context"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/haify-project/haify/pkg/deployment"
)

// fillZFSCompression sets each pool's compression algorithm and achieved
// ratio, read from its root dataset on its node. addrs maps a pool's Node to
// the address to ask. Best effort: a node that does not answer leaves its
// pools without the figures, which the API reports as unknown.
//
// OpenZFS 2.2 and later compress by default, so a ZFS pool is very likely
// compressing already; without this nothing said whether, or how well.
func (sm *StorageManager) fillZFSCompression(ctx context.Context, pools []*PoolInfo, addrs map[string]string) {
	log := sm.controller.traceLogger()
	byHost := map[string][]*PoolInfo{}
	for _, p := range pools {
		if addr := addrs[p.Node]; addr != "" {
			byHost[addr] = append(byHost[addr], p)
		}
	}
	if len(byHost) == 0 {
		log.Debug("fillZFSCompression: no pool on a known host, nothing to read", zap.Int("pools", len(pools)))
		return
	}
	hosts := make([]string, 0, len(byHost))
	for h := range byHost {
		hosts = append(hosts, h)
	}
	log.Debug("fillZFSCompression: reading pool properties", zap.Strings("hosts", hosts), zap.Int("pools", len(pools)))
	start := time.Now()
	res, err := sm.controller.deployment.Exec(ctx, hosts,
		"echo "+base64Std(deployment.ZFSPoolPropertiesScript)+" | base64 -d | sudo /bin/sh")
	if err != nil || res == nil {
		sm.controller.logger.Debug("Could not read ZFS pool compression", zap.Error(err))
		return
	}
	log.Debug("fillZFSCompression: properties read", zap.Strings("failed_hosts", res.FailedHosts()), zap.Duration("took", time.Since(start)))
	for host, hr := range res.Hosts {
		if hr == nil || !hr.Success {
			log.Debug("fillZFSCompression: host did not answer", zap.String("host", host))
			continue
		}
		props := parseZFSPoolProperties(hr.Output)
		for _, p := range byHost[host] {
			if v, ok := props[p.Name]; ok {
				p.Compression, p.CompressRatio = v.compression, v.ratio
				log.Debug("fillZFSCompression: pool", zap.String("host", host), zap.String("pool", p.Name),
					zap.String("compression", v.compression), zap.Float64("ratio", v.ratio))
			} else {
				log.Debug("fillZFSCompression: pool not in the properties", zap.String("host", host), zap.String("pool", p.Name))
			}
		}
	}
}

type zfsPoolProps struct {
	compression string
	ratio       float64
}

// parseZFSPoolProperties reads `zfs get -H -p -o name,property,value` output.
// compressratio is printed as "1.85" with -p and as "1.85x" without; both are
// accepted.
func parseZFSPoolProperties(out string) map[string]zfsPoolProps {
	props := map[string]zfsPoolProps{}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Split(strings.TrimSpace(line), "\t")
		if len(f) != 3 {
			continue
		}
		v := props[f[0]]
		switch f[1] {
		case "compression":
			v.compression = f[2]
		case "compressratio":
			if r, err := strconv.ParseFloat(strings.TrimSuffix(f[2], "x"), 64); err == nil {
				v.ratio = r
			}
		default:
			continue
		}
		props[f[0]] = v
	}
	zap.L().Debug("parseZFSPoolProperties: parsed", zap.Int("datasets", len(props)))
	return props
}
