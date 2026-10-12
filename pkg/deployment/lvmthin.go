package deployment

import (
	"context"
	"fmt"

	"go.uber.org/zap"
)

// lvmthin — how full a thin pool actually is.
//
// A thin pool's capacity is not its volume group's capacity. Haify gives the
// pool 95% of the VG's free extents at create (all of them on convert-thin),
// so vg_free is near zero from the moment the pool exists and stays there; the
// number that decides whether writes succeed is the pool's own data
// utilisation, which lives on the pool LV.
//
// Both counters here matter independently. A pool whose *metadata* fills up
// fails writes exactly as hard as one whose data fills up, and the two grow
// for unrelated reasons — metadata tracks the number of mapped blocks and
// snapshots, data tracks the bytes in them.

// LVMThinFields is the column list used for thin pool utilisation queries.
//
// vg_name leads so one query can cover every group on a host, matching
// LVMCacheFields: surfacing pool fullness in `pool list` costs one extra SSH
// round for the whole cluster rather than one per pool.
//
// segtype is included so the caller can pick out thin pools by type instead of
// guessing from the name. lv_size is the pool's data capacity, which is what
// data_percent is a percentage of.
const LVMThinFields = "vg_name,lv_name,segtype,lv_size,data_percent,metadata_percent,lv_attr,origin"

// LVSThinReport lists every LV on the given hosts with its thin pool
// utilisation columns. vgName may be empty to cover all volume groups.
//
// data_percent and metadata_percent are blank for anything that is not a thin
// pool, and also for a thin pool that is not currently active — absence of
// information rather than zero, which the parser distinguishes.
func (c *Client) LVSThinReport(ctx context.Context, hosts []string, vgName string) (*ExecResult, error) {
	c.log().Debug("LVSThinReport: reading thin pool usage",
		zap.Strings("hosts", hosts), zap.String("vg", vgName), zap.Bool("all_groups", vgName == ""))
	cmd := fmt.Sprintf("sudo lvs --noheadings --nosuffix --units b --separator '|' -o %s", LVMThinFields)
	if vgName != "" {
		cmd += " " + vgName
	}
	return c.Exec(ctx, hosts, cmd)
}
