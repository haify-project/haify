package controller

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/haify-project/haify/pkg/database"
	"github.com/haify-project/haify/pkg/wanproxy"
)

// DRBD options for a connection that crosses the WAN through haify-proxy.
//
// The proxy's buffer only helps if DRBD lets it fill. DRBD counts every write
// it has sent and the peer has not yet acknowledged as in flight, and the
// proxy's backlog is exactly that, so three DRBD settings decide what the
// buffer can do:
//
//   - congestion-fill: with on-congestion pull-ahead, DRBD goes Ahead once this
//     much is in flight. It used to be 2 MiB (400 MiB on multi-replica legs)
//     against a 4 GiB buffer, so a burst pushed DRBD into Ahead and a bitmap
//     resync within seconds while the buffer held 3 MiB. It is now 90% of the
//     proxy buffer, so DRBD goes Ahead just before the buffer would be full.
//   - ko-count: DRBD drops a peer that leaves a write unacknowledged for
//     ko-count x timeout (42 s by default). A backlog that takes longer than
//     that to drain is not a dead peer, and the default turned a burst the
//     buffer had absorbed into a disconnect and a resync. 0 turns it off;
//     protocol A with pull-ahead never makes the primary wait for the DR, and
//     a dead link is still found by the ping and the proxy's own timeouts.
//   - c-fill-target and c-plan-ahead: the resync controller keeps
//     c-fill-target in flight. DRBD's default of 50 KiB is sized for a LAN: on a
//     40 Mbit/s link with a 40 ms round trip it resyncs at 1.8 MiB/s, against
//     4.5 MiB/s (94% of the link) with these values, which are the ones LINBIT
//     gives for DRBD Proxy: 15 MiB in flight, planned 20 s ahead.

const (
	wanCPlanAhead   = "200" // tenths of a second
	wanCFillTarget  = "15M"
	wanKoCount      = "0"
	wanFillFraction = 0.9
	// drbdMaxCongestionFill is the largest congestion-fill DRBD accepts.
	drbdMaxCongestionFill = 10 << 30
)

// wanCongestionFill is the congestion-fill for a proxy buffer: 90% of it, in
// MiB, within what DRBD accepts.
func wanCongestionFill(t wanproxy.Tuning) string {
	buffer, _ := t.Effective()
	fill := int64(float64(buffer) * wanFillFraction)
	if fill > drbdMaxCongestionFill {
		fill = drbdMaxCongestionFill
	}
	return fmt.Sprintf("%dM", fill>>20)
}

// writeWANLegOptions writes the net and disk blocks of one explicit WAN
// connection, indented for a `connection` section.
func writeWANLegOptions(b *strings.Builder, t wanproxy.Tuning) {
	b.WriteString("        net {\n")
	b.WriteString("            protocol A;\n")
	b.WriteString("            on-congestion pull-ahead;\n")
	fmt.Fprintf(b, "            congestion-fill %s;\n", wanCongestionFill(t))
	fmt.Fprintf(b, "            ko-count %s;\n", wanKoCount)
	fmt.Fprintf(b, "            csums-alg %s;\n", wanCsumsAlg)
	b.WriteString("        }\n")
	b.WriteString("        disk {\n")
	fmt.Fprintf(b, "            c-plan-ahead %s;\n", wanCPlanAhead)
	fmt.Fprintf(b, "            c-fill-target %s;\n", wanCFillTarget)
	b.WriteString("        }\n")
}

var (
	congestionFillLine = regexp.MustCompile(`^(\s*)congestion-fill\s+[^;]*;`)
	koCountLine        = regexp.MustCompile(`^\s*ko-count\s`)
	netOpenLine        = regexp.MustCompile(`^(\s*)net\s*\{\s*$`)
)

// applyWANOptions converges a live resource config on the WAN options for t:
// in every net block that carries `on-congestion pull-ahead` (the WAN ones,
// which only Haify writes) congestion-fill follows the buffer and ko-count is
// added when absent; and when no c-fill-target is set anywhere, a disk block
// with the WAN resync settings follows each such net block. A value the
// operator set for ko-count, c-fill-target or c-plan-ahead is left alone;
// congestion-fill belongs to the buffer and is always rewritten.
//
// It works on lines because it has to apply to configs written by every
// earlier version as well as the current one. The result of applying it twice
// is the result of applying it once.
func applyWANOptions(content string, t wanproxy.Tuning) string {
	fill := wanCongestionFill(t)
	lines := strings.Split(content, "\n")
	addDisk := !strings.Contains(content, "c-fill-target")
	var out []string
	for i := 0; i < len(lines); i++ {
		m := netOpenLine.FindStringSubmatch(lines[i])
		end := -1
		if m != nil {
			end = blockEnd(lines, i)
		}
		if end < 0 || !strings.Contains(strings.Join(lines[i:end], "\n"), "on-congestion pull-ahead") {
			out = append(out, lines[i])
			continue
		}
		indent := m[1]
		inner := indent + "    "
		hasFill, hasKo := false, false
		out = append(out, lines[i])
		for _, l := range lines[i+1 : end] {
			if fm := congestionFillLine.FindStringSubmatch(l); fm != nil {
				l = fmt.Sprintf("%scongestion-fill %s;", fm[1], fill)
				hasFill = true
			}
			if koCountLine.MatchString(l) {
				hasKo = true
			}
			out = append(out, l)
		}
		if !hasFill {
			out = append(out, fmt.Sprintf("%scongestion-fill %s;", inner, fill))
		}
		if !hasKo {
			out = append(out, fmt.Sprintf("%sko-count %s;", inner, wanKoCount))
		}
		out = append(out, lines[end])
		if addDisk {
			out = append(out, indent+"disk {",
				fmt.Sprintf("%sc-plan-ahead %s;", inner, wanCPlanAhead),
				fmt.Sprintf("%sc-fill-target %s;", inner, wanCFillTarget),
				indent+"}")
		}
		i = end
	}
	return strings.Join(out, "\n")
}

// blockEnd returns the index of the line closing the block opened on line
// start, or -1.
func blockEnd(lines []string, start int) int {
	depth := 0
	for i := start; i < len(lines); i++ {
		depth += strings.Count(lines[i], "{") - strings.Count(lines[i], "}")
		if depth == 0 {
			return i
		}
	}
	return -1
}

// applyWANDRBDOptions rewrites a WAN resource's DRBD config on every member with
// applyWANOptions and has DRBD adjust to it, which changes these options on
// live connections without dropping them. Nothing is written when the config
// already matches.
func (rm *ResourceManager) applyWANDRBDOptions(ctx context.Context, dbRes *database.Resource) (bool, error) {
	log := rm.resourcesLog()
	hosts, err := rm.resourceHosts(ctx, dbRes.Name)
	if err != nil {
		return false, err
	}
	for _, list := range []string{dbRes.DisklessNodes, dbRes.DisklessClients} {
		for _, n := range splitCSV(list) {
			if n != "" {
				hosts = append(hosts, rm.controller.ResolveHost(n))
			}
		}
	}
	resPath := fmt.Sprintf("/etc/drbd.d/%s.res", dbRes.Name)
	start := time.Now()
	res, err := rm.deployment.Exec(ctx, hosts[:1], "cat "+resPath)
	log.Debug("applyWANDRBDOptions: read config", zap.String("resource", dbRes.Name), zap.String("host", hosts[0]),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return false, fmt.Errorf("read %s: %w", resPath, err)
	}
	live, ok := "", false
	for _, r := range res.Hosts {
		live, ok = r.Output, r.Success
	}
	if !ok || strings.TrimSpace(live) == "" {
		return false, fmt.Errorf("read %s on %s: %s", resPath, hosts[0], res.FailureDetails())
	}
	updated := applyWANOptions(live, wanTuningOf(dbRes))
	if updated == live {
		log.Debug("applyWANDRBDOptions: already current", zap.String("resource", dbRes.Name))
		return false, nil
	}
	start = time.Now()
	dist, err := rm.deployment.DistributeConfig(ctx, hosts, updated, resPath)
	log.Debug("applyWANDRBDOptions: distributed", zap.String("resource", dbRes.Name), zap.Strings("hosts", hosts),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return false, fmt.Errorf("distribute %s: %w", resPath, err)
	}
	if dist != nil && len(dist.FailedHosts()) > 0 {
		return false, fmt.Errorf("distribute %s: %s", resPath, dist.FailureDetails())
	}
	return true, rm.execAllSuccess(ctx, hosts, fmt.Sprintf("sudo drbdadm adjust %s", dbRes.Name), "apply the WAN options")
}
