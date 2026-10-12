package main

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	haifypb "github.com/haify-project/haify/api/proto/v1"
	"go.uber.org/zap"
)

// Rendering a promoter's live status for `haify ha status`: the service chain
// with what each unit is, whether each standby could take the chain over, and
// what outside the chain uses its mounts.

// printHaChain prints the chain of units on the node running it.
func printHaChain(w io.Writer, p *haifypb.HaPromoterStatus) {
	log := zap.L()
	log.Debug("printHaChain: printing service chain", zap.String("resource", p.GetDrbdResource()), zap.String("active_node", p.GetActiveNode()),
		zap.Int("deps", len(p.GetDeps())))
	where := "no node is running it"
	if p.GetActiveNode() != "" {
		where = "on " + p.GetActiveNode()
	}
	_, _ = fmt.Fprintf(w, "\nService chain (%s):\n", where)
	tw := tabwriter.NewWriter(w, 0, 0, 3, ' ', 0)
	_, _ = fmt.Fprintln(tw, "  KIND\tWHAT\tSTATE\tSINCE\tRESTARTS\tUNIT")
	row := func(s *haifypb.HaServiceStatus) {
		if s == nil || s.GetName() == "" {
			log.Debug("printHaChain: skipping unnamed unit")
			return
		}
		log.Debug("printHaChain: unit", zap.String("unit", s.GetName()), zap.String("kind", s.GetKind()), zap.String("status", s.GetStatus()), zap.Int32("restarts", s.GetRestarts()))
		_, _ = fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%d\t%s\n",
			dashIfEmpty(s.GetKind()), dashIfEmpty(s.GetLabel()), unitState(s),
			dashIfEmpty(sinceLocal(s.GetSince())), s.GetRestarts(), s.GetName())
	}
	for _, d := range p.GetDeps() {
		row(d)
	}
	row(p.GetTarget())
	_ = tw.Flush()
}

// printHaStandbys prints whether each standby could take the chain over.
func printHaStandbys(w io.Writer, p *haifypb.HaPromoterStatus) {
	log := zap.L()
	log.Debug("printHaStandbys: printing standbys", zap.Int("count", len(p.GetStandbys())))
	_, _ = fmt.Fprintf(w, "\nStandbys:\n")
	if len(p.GetStandbys()) == 0 {
		log.Debug("printHaStandbys: no standby can take over")
		_, _ = fmt.Fprintf(w, "  none: no other node holds an enabled promoter, so nothing can take over\n")
		return
	}
	tw := tabwriter.NewWriter(w, 0, 0, 3, ' ', 0)
	_, _ = fmt.Fprintln(tw, "  NODE\tREADY\tDISK\tWHY NOT")
	for _, s := range p.GetStandbys() {
		log.Debug("printHaStandbys: standby", zap.String("node", s.GetNode()), zap.Bool("ready", s.GetReady()),
			zap.String("disk_state", s.GetDiskState()), zap.Strings("reasons", s.GetReasons()))
		ready := "✗ no"
		if s.GetReady() {
			ready = "● yes"
		}
		_, _ = fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\n", s.GetNode(), ready,
			dashIfEmpty(s.GetDiskState()), dashIfEmpty(strings.Join(s.GetReasons(), "; ")))
	}
	_ = tw.Flush()
}

// printHaOutside prints the services outside the chain using its mounts.
func printHaOutside(w io.Writer, p *haifypb.HaPromoterStatus) {
	log := zap.L()
	if p.GetActiveNode() == "" {
		log.Debug("printHaOutside: chain runs nowhere, nothing to check")
		return
	}
	log.Debug("printHaOutside: printing outside users", zap.String("active_node", p.GetActiveNode()),
		zap.Int("units", len(p.GetOutside())), zap.Int32("sessions", p.GetOutsideSessions()))
	_, _ = fmt.Fprintf(w, "\nUsing the chain's mounts from outside it (on %s):\n", p.GetActiveNode())
	if len(p.GetOutside()) == 0 && p.GetOutsideSessions() == 0 {
		log.Debug("printHaOutside: nothing outside the chain uses its mounts")
		_, _ = fmt.Fprintf(w, "  nothing\n")
		return
	}
	if len(p.GetOutside()) > 0 {
		tw := tabwriter.NewWriter(w, 0, 0, 3, ' ', 0)
		_, _ = fmt.Fprintln(tw, "  UNIT\tPROCESSES\tMOUNTS\tDESCRIPTION")
		for _, o := range p.GetOutside() {
			_, _ = fmt.Fprintf(tw, "  %s\t%d\t%s\t%s\n", o.GetUnit(), o.GetProcesses(),
				strings.Join(o.GetMountPoints(), ","), dashIfEmpty(o.GetDescription()))
		}
		_ = tw.Flush()
	}
	if n := p.GetOutsideSessions(); n > 0 {
		_, _ = fmt.Fprintf(w, "  %d login %s (%d %s)\n", n, plural(int(n), "session", "sessions"),
			p.GetOutsideSessionProcesses(), plural(int(p.GetOutsideSessionProcesses()), "process", "processes"))
	}
	_, _ = fmt.Fprintf(w, "  These hold the unmount up on a failover; stop them or move them into the chain.\n")
}

// unitState is "● active (running)".
func unitState(s *haifypb.HaServiceStatus) string {
	st := s.GetStatus()
	if sub := s.GetSubState(); sub != "" && sub != st {
		return fmt.Sprintf("%s %s (%s)", statusIcon(st), st, sub)
	}
	return fmt.Sprintf("%s %s", statusIcon(st), st)
}

// sinceLocal shows an RFC 3339 time in local time.
func sinceLocal(rfc3339 string) string {
	t, err := time.Parse(time.RFC3339, rfc3339)
	if err != nil {
		return ""
	}
	return t.Local().Format("2006-01-02 15:04")
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
