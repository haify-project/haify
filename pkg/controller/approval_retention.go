package controller

import (
	"context"
	"strings"
	"time"

	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"

	haifypb "github.com/haify-project/haify/api/proto/v1"
	"github.com/haify-project/haify/pkg/database"
)

// Replacing a schedule is held back only when it weakens one.
//
// A snapshot or backup schedule prunes, on its next run, whatever its
// retention no longer keeps. Replacing a schedule with one that keeps fewer of
// any kind is therefore DeleteSnapshot or DeleteBackup by another name, and
// one stolen token could empty the history without a second person. Creating
// a schedule, or replacing one with the same or a longer retention, deletes
// nothing and runs at once.

// conditionalApproval holds the listed methods that need approval only when
// the call lowers what an existing schedule keeps.
var conditionalApproval = map[string]func(ctx context.Context, db *database.DB, req proto.Message) bool{
	"CreateSnapshotSchedule": func(ctx context.Context, db *database.DB, req proto.Message) bool {
		log := zap.L()
		r, ok := req.(*haifypb.CreateSnapshotScheduleRequest)
		if !ok {
			log.Debug("conditionalApproval: unexpected request type, approval needed", zap.String("method", "CreateSnapshotSchedule"))
			return true
		}
		start := time.Now()
		old, err := db.GetSnapshotSchedule(ctx, r.Resource)
		if err != nil {
			missing := strings.Contains(err.Error(), "not found")
			log.Debug("conditionalApproval: no existing snapshot schedule readable",
				zap.String("resource", r.Resource), zap.Bool("not_found", missing),
				zap.Bool("needs_approval", !missing), zap.Duration("took", time.Since(start)), zap.Error(err))
			return !missing
		}
		less := keepsLess(gfsFromProto(r.Keep), old.Keep)
		log.Debug("conditionalApproval: compared snapshot schedule retention",
			zap.String("resource", r.Resource), zap.Bool("keeps_less", less), zap.Duration("took", time.Since(start)))
		return less
	},
	"CreateBackupSchedule": func(ctx context.Context, db *database.DB, req proto.Message) bool {
		log := zap.L()
		r, ok := req.(*haifypb.CreateBackupScheduleRequest)
		if !ok {
			log.Debug("conditionalApproval: unexpected request type, approval needed", zap.String("method", "CreateBackupSchedule"))
			return true
		}
		name := database.BackupScheduleName(r.Resource, r.Target)
		start := time.Now()
		old, err := db.GetBackupSchedule(ctx, name)
		if err != nil {
			missing := strings.Contains(err.Error(), "not found")
			log.Debug("conditionalApproval: no existing backup schedule readable",
				zap.String("schedule", name), zap.Bool("not_found", missing),
				zap.Bool("needs_approval", !missing), zap.Duration("took", time.Since(start)), zap.Error(err))
			return !missing
		}
		less := keepsLess(gfsFromProto(r.Keep), old.Keep)
		log.Debug("conditionalApproval: compared backup schedule retention",
			zap.String("schedule", name), zap.Bool("keeps_less", less), zap.Duration("took", time.Since(start)))
		return less
	},
}

// keepsLess reports whether next keeps fewer of any kind than old.
func keepsLess(next, old database.GFSPolicy) bool {
	return next.Hourly < old.Hourly || next.Daily < old.Daily || next.Weekly < old.Weekly ||
		next.Monthly < old.Monthly || next.Yearly < old.Yearly
}
