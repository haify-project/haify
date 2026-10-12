// Package inspect is the cluster inspection (巡检): a fixed set of
// deterministic checks run on a schedule or on demand, each answering pass,
// warn, fail or error with the evidence it saw and the command that fixes it.
//
// It exists for the problems that sit around the alerts rather than in them.
// The 30-second detector in pkg/alert raises a condition when it starts; it
// does not ask whether anyone heard, whether a replica that reports Connected
// has actually finished its handshake, whether the node a resource is
// registered at still owns that address, or whether last week's backup ran.
// Every one of those was found on a real cluster with all alerts green.
//
// The package only judges. The controller gathers an Input — the database's
// view plus one probe per node — and Run turns it into Checks, so every check
// is a pure function testable without SSH.
package inspect

import (
	"sort"
	"time"

	"go.uber.org/zap"
)

// Status is one check's verdict.
type Status string

const (
	StatusPass Status = "pass"
	StatusWarn Status = "warn"
	StatusFail Status = "fail"
	// StatusError means the check itself could not run — a node did not answer
	// its probe, a record could not be read. It is not a verdict on the
	// subject, and it is never folded into pass.
	StatusError Status = "error"
)

// rank orders statuses by how much attention they need. error sits between
// warn and fail: something could not be looked at, which is worse than a known
// warning and better than a known failure.
func (s Status) rank() int {
	switch s {
	case StatusFail:
		return 3
	case StatusError:
		return 2
	case StatusWarn:
		return 1
	default:
		return 0
	}
}

// AtLeast reports whether s needs at least as much attention as min.
func (s Status) AtLeast(min Status) bool { return s.rank() >= min.rank() }

// ParseStatus maps a config or flag value to a Status; unknown values give
// pass, the most permissive threshold, so a typo never hides a failure.
func ParseStatus(v string) Status {
	switch Status(v) {
	case StatusWarn, StatusFail, StatusError:
		zap.L().Debug("ParseStatus: known status", zap.String("value", v))
		return Status(v)
	}
	if v == "warning" {
		zap.L().Debug("ParseStatus: warning taken as warn")
		return StatusWarn
	}
	zap.L().Debug("ParseStatus: unknown or pass, using pass", zap.String("value", v))
	return StatusPass
}

// Area groups checks the way an operator thinks about the cluster.
type Area string

const (
	AreaResources Area = "resources"
	AreaGateways  Area = "gateways"
	AreaNodes     Area = "nodes"
	AreaPools     Area = "pools"
	AreaBackups   Area = "backups"
	AreaAlerts    Area = "alerts"
	AreaSelfHA    Area = "selfha"
	AreaTLS       Area = "tls"
	AreaHygiene   Area = "hygiene"
)

// Areas is every area, in report order.
var Areas = []Area{AreaResources, AreaGateways, AreaNodes, AreaPools, AreaBackups, AreaAlerts, AreaSelfHA, AreaTLS, AreaHygiene}

// Trigger says why a report exists.
type Trigger string

const (
	TriggerSchedule Trigger = "schedule"
	TriggerManual   Trigger = "manual"
)

// Check is one finding. A failing or warning check names one subject; a
// passing check may cover a whole area in one line.
type Check struct {
	// ID is a stable slug ("resource.no_primary") a receiver can key on.
	ID      string `json:"id"`
	Area    Area   `json:"area"`
	Subject string `json:"subject,omitempty"`
	Status  Status `json:"status"`
	Message string `json:"message"`
	// Evidence is what was seen, one fact per line, so the verdict can be
	// checked without rerunning anything.
	Evidence []string `json:"evidence,omitempty"`
	// Fix is a command to run as written: an `haify ...` call or a shell line.
	Fix string `json:"fix,omitempty"`
	// Runbook names a pkg/mcpserver/runbooks entry covering the repair.
	Runbook string `json:"runbook,omitempty"`
}

// Summary counts checks by status.
type Summary struct {
	Pass  int `json:"pass"`
	Warn  int `json:"warn"`
	Fail  int `json:"fail"`
	Error int `json:"error"`
}

// PoolSample is one thin pool's fill level at report time. Kept in the report
// so the next run can tell a pool that is filling from one that is merely full.
type PoolSample struct {
	Node        string  `json:"node"`
	Pool        string  `json:"pool"`
	DataPercent float64 `json:"data_percent"`
	MetaPercent float64 `json:"meta_percent"`
}

// Report is one inspection run.
type Report struct {
	ID         string    `json:"id"`
	Trigger    Trigger   `json:"trigger"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
	// Areas is what was asked for; empty means every area.
	Areas   []Area       `json:"areas,omitempty"`
	Summary Summary      `json:"summary"`
	Checks  []Check      `json:"checks"`
	Pools   []PoolSample `json:"pools,omitempty"`
}

// Summarize counts checks by status.
func Summarize(checks []Check) Summary {
	var s Summary
	for _, c := range checks {
		switch c.Status {
		case StatusFail:
			s.Fail++
		case StatusWarn:
			s.Warn++
		case StatusError:
			s.Error++
		default:
			s.Pass++
		}
	}
	zap.L().Debug("Summarize: counted", zap.Int("count", len(checks)), zap.Int("pass", s.Pass), zap.Int("warn", s.Warn),
		zap.Int("fail", s.Fail), zap.Int("error", s.Error))
	return s
}

// Worst is the most serious status among checks; pass when there are none.
func Worst(checks []Check) Status {
	worst := StatusPass
	for _, c := range checks {
		if c.Status.rank() > worst.rank() {
			worst = c.Status
		}
	}
	return worst
}

// Sort orders checks by area, then most serious first, then by ID and subject,
// so two reports of the same cluster read the same way.
func Sort(checks []Check) {
	order := make(map[Area]int, len(Areas))
	for i, a := range Areas {
		order[a] = i
	}
	sort.SliceStable(checks, func(i, j int) bool {
		a, b := checks[i], checks[j]
		if order[a.Area] != order[b.Area] {
			return order[a.Area] < order[b.Area]
		}
		if a.Status.rank() != b.Status.rank() {
			return a.Status.rank() > b.Status.rank()
		}
		if a.ID != b.ID {
			return a.ID < b.ID
		}
		return a.Subject < b.Subject
	})
}

// ValidArea reports whether s names an area.
func ValidArea(s string) bool {
	for _, a := range Areas {
		if string(a) == s {
			return true
		}
	}
	zap.L().Debug("ValidArea: not an area", zap.String("area", s))
	return false
}
