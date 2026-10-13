package controller

import (
	"context"
	"fmt"
	"strings"

	"go.uber.org/zap"

	pb "github.com/haify-project/haify/api/proto/v1"
	"github.com/haify-project/haify/pkg/util"
	"github.com/haify-project/haify/pkg/wanproxy"
)

// SetWanTuning changes a WAN resource's proxy buffer and compression, saves
// them on the record (so repair, add-replica and every later re-provision keep
// them) and rebuilds the tunnels on them.
//
// Rebuilding restarts each leg's proxy. What its buffer held is not lost: DRBD
// keeps every protocol A write it has not had acknowledged, marks those blocks
// out of sync when the connection drops, and resyncs them when the proxy is
// back. If the rebuild fails the old settings are put back.
func (s *Server) SetWanTuning(ctx context.Context, req *pb.SetWanTuningRequest) (*pb.SetWanTuningResponse, error) {
	log := s.serverLog()
	log.Debug("SetWanTuning: changing", zap.String("resource", req.GetName()), zap.String("buffer", req.GetBuffer()),
		zap.String("compression", req.GetCompression()))
	fail := func(format string, args ...any) (*pb.SetWanTuningResponse, error) {
		msg := fmt.Sprintf(format, args...)
		log.Debug("SetWanTuning: refused", zap.String("resource", req.GetName()), zap.String("message", msg))
		return &pb.SetWanTuningResponse{Success: false, Message: msg}, nil
	}
	name := strings.TrimSpace(req.GetName())
	if name == "" {
		return fail("resource name is required")
	}
	bufferArg, compressionArg := strings.TrimSpace(req.GetBuffer()), strings.TrimSpace(req.GetCompression())
	if bufferArg == "" && compressionArg == "" {
		return fail("nothing to change: give a buffer size, a compression setting, or both")
	}
	if s.ctrl.db == nil {
		return fail("database not available")
	}
	dbRes, err := s.ctrl.db.GetResource(ctx, name)
	if err != nil || dbRes == nil {
		return fail("resource %q not found", name)
	}
	if !dbRes.WANMode {
		return fail("resource %q is not a WAN resource", name)
	}

	old := wanTuningOf(dbRes)
	next := old
	if bufferArg != "" {
		b, err := parseWANBuffer(bufferArg)
		if err != nil {
			return fail("%v", err)
		}
		next.BufferBytes = b
	}
	if compressionArg != "" {
		level, err := wanproxy.ParseCompression(compressionArg)
		if err != nil {
			return fail("%v", err)
		}
		next.ZstdLevel = level
	}
	if err := next.Validate(); err != nil {
		return fail("%v", err)
	}
	reply := func(ok bool, msg string, t wanproxy.Tuning) *pb.SetWanTuningResponse {
		buffer, _ := t.Effective()
		return &pb.SetWanTuningResponse{Success: ok, Message: msg, BufferBytes: uint64(buffer),
			Compression: wanproxy.CompressionString(t.ZstdLevel)}
	}
	rm := s.ctrl.resources
	if next == old {
		// Nothing to change in the proxies, but a resource created by an
		// earlier version may still carry the old DRBD WAN options; bring them
		// up to date without restarting anything.
		changed, err := rm.applyWANDRBDOptions(ctx, dbRes)
		log.Debug("SetWanTuning: proxies already tuned", zap.String("resource", name), zap.Bool("drbd_options_changed", changed),
			zap.Error(err))
		switch {
		case err != nil:
			return reply(false, fmt.Sprintf("proxies already tuned, but the DRBD options were not applied: %v", err), old), nil
		case changed:
			return reply(true, "proxies already tuned; DRBD options brought up to date (congestion-fill, ko-count, resync)", old), nil
		}
		return reply(true, "already set; nothing changed", old), nil
	}

	// DRBD's congestion-fill must stay below the proxy buffer, or DRBD keeps
	// sending into a full buffer instead of going Ahead and the application's
	// writes wait. So a smaller buffer lowers congestion-fill first, and a
	// larger one raises it last.
	oldBuffer, _ := old.Effective()
	nextBuffer, _ := next.Effective()
	shrinking := nextBuffer < oldBuffer
	setTuning := func(t wanproxy.Tuning) error {
		dbRes.WANBufferBytes, dbRes.WANZstdLevel = t.BufferBytes, t.ZstdLevel
		return s.ctrl.db.SaveResource(ctx, dbRes)
	}
	if err := setTuning(next); err != nil {
		return fail("save %s: %v", name, err)
	}
	s.ctrl.logger.Info("Changing WAN proxy tuning", zap.String("resource", name),
		zap.Int64("buffer_bytes_from", oldBuffer), zap.Int64("buffer_bytes_to", nextBuffer),
		zap.Int("zstd_level_from", old.ZstdLevel), zap.Int("zstd_level_to", next.ZstdLevel))
	if shrinking {
		if _, err := rm.applyWANDRBDOptions(ctx, dbRes); err != nil {
			log.Debug("SetWanTuning: lowering congestion-fill failed", zap.String("resource", name), zap.Error(err))
			if serr := setTuning(old); serr != nil {
				return fail("lower congestion-fill: %v; restoring the old tuning also failed: %v", err, serr)
			}
			return reply(false, fmt.Sprintf("not changed: lower congestion-fill: %v", err), old), nil
		}
	}

	rep := s.repairWanLegs(ctx, dbRes, false, false)
	if !rep.Success {
		log.Debug("SetWanTuning: rebuild failed, restoring the old tuning", zap.String("message", rep.Message))
		if err := setTuning(old); err != nil {
			return fail("%s; restoring the old tuning also failed: %v", rep.Message, err)
		}
		back := s.repairWanLegs(ctx, dbRes, false, true)
		msg := fmt.Sprintf("not changed: %s", rep.Message)
		if !back.Success {
			msg = fmt.Sprintf("not changed: %s. Rebuilding the tunnels on the old tuning also failed: %s; run wan repair %s",
				rep.Message, back.Message, name)
		}
		if shrinking {
			if _, err := rm.applyWANDRBDOptions(ctx, dbRes); err != nil {
				msg += fmt.Sprintf(". congestion-fill is still the lowered one: %v", err)
			}
		}
		return reply(false, msg, old), nil
	}

	msg := fmt.Sprintf("%s now buffers up to %s per connection, compression %s; %s", name,
		util.FormatBytes(uint64(nextBuffer)), wanproxy.CompressionString(next.ZstdLevel), rep.Message)
	if !shrinking {
		if _, err := rm.applyWANDRBDOptions(ctx, dbRes); err != nil {
			log.Debug("SetWanTuning: raising congestion-fill failed", zap.String("resource", name), zap.Error(err))
			return reply(false, msg+fmt.Sprintf("; but DRBD's congestion-fill was not raised to match (%v), so DRBD "+
				"goes Ahead before the buffer fills; run wan tune again", err), next), nil
		}
	}
	log.Debug("SetWanTuning: tuned", zap.String("resource", name))
	return reply(true, msg, next), nil
}

// parseWANBuffer reads a buffer size, or "default".
func parseWANBuffer(s string) (int64, error) {
	if strings.EqualFold(s, "default") {
		return 0, nil
	}
	b, err := util.ParseSize(s)
	if err != nil {
		zap.L().Debug("parseWANBuffer: not a size", zap.String("value", s), zap.Error(err))
		return 0, fmt.Errorf("buffer %q: give a size such as 2GiB, or default", s)
	}
	if b > wanproxy.MaxBufferBytes {
		return 0, fmt.Errorf("buffer must be between 16 MiB and 10 GiB")
	}
	return int64(b), nil
}
