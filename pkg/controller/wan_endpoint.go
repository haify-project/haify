package controller

import (
	"context"
	"fmt"
	"net"
	"regexp"
	"strings"

	pb "github.com/haify-project/haify/api/proto/v1"
	"go.uber.org/zap"
)

// The DR endpoint and the egress address are given once, at create or add-dr,
// and until SetWanEndpoint nothing could change them afterwards. A DR site
// whose public address changed — a new lease, a moved VPS, a NAT rule redone —
// left the resource dialling the old one for good: wan repair rebuilds the
// tunnels from the record, so it faithfully rebuilt them onto the dead address.

// hostnamePattern accepts a DNS name: dot-separated labels of letters, digits
// and hyphens.
var hostnamePattern = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?(\.[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?)*$`)

// validDREndpoint accepts an IP address or a host name, and nothing with a
// port or a scheme: the port is the resource's WAN port, and a second one
// written here would be silently ignored.
func validDREndpoint(s string) error {
	if net.ParseIP(s) != nil {
		zap.L().Debug("validDREndpoint: an IP address", zap.String("endpoint", s))
		return nil
	}
	if strings.Contains(s, ":") || strings.Contains(s, "/") {
		zap.L().Debug("validDREndpoint: has a port or scheme", zap.String("endpoint", s))
		return fmt.Errorf("DR endpoint %q: give an address or a host name, without a port or scheme", s)
	}
	if !hostnamePattern.MatchString(s) {
		zap.L().Debug("validDREndpoint: not a host name", zap.String("endpoint", s))
		return fmt.Errorf("DR endpoint %q is not an IP address or host name", s)
	}
	zap.L().Debug("validDREndpoint: a host name", zap.String("endpoint", s))
	return nil
}

// SetWanEndpoint changes where a WAN resource's primary site reaches its DR
// site and rebuilds the tunnels on the new address. Unless told to skip the
// check, a new endpoint that does not answer is refused and the old one put
// back: the old tunnel was working, and a typo should not take it down.
func (s *Server) SetWanEndpoint(ctx context.Context, req *pb.SetWanEndpointRequest) (*pb.SetWanEndpointResponse, error) {
	log := s.serverLog()
	log.Debug("SetWanEndpoint: changing", zap.String("resource", req.GetName()), zap.String("dr_endpoint", req.GetDrEndpoint()),
		zap.String("egress_address", req.GetEgressAddress()), zap.Bool("clear_egress", req.GetClearEgress()),
		zap.Bool("skip_reachability_check", req.GetSkipReachabilityCheck()))
	fail := func(format string, args ...any) (*pb.SetWanEndpointResponse, error) {
		msg := fmt.Sprintf(format, args...)
		log.Debug("SetWanEndpoint: refused", zap.String("resource", req.GetName()), zap.String("message", msg))
		return &pb.SetWanEndpointResponse{Success: false, Message: msg}, nil
	}
	name := strings.TrimSpace(req.GetName())
	endpoint := strings.TrimSpace(req.GetDrEndpoint())
	egress := strings.TrimSpace(req.GetEgressAddress())
	if name == "" {
		return fail("resource name is required")
	}
	if endpoint == "" && egress == "" && !req.GetClearEgress() {
		return fail("nothing to change: give a DR endpoint, an egress address, or clear the egress")
	}
	if egress != "" && req.GetClearEgress() {
		return fail("give an egress address or clear it, not both")
	}
	if endpoint != "" {
		if err := validDREndpoint(endpoint); err != nil {
			return fail("%v", err)
		}
	}
	if egress != "" && net.ParseIP(egress) == nil {
		return fail("egress address %q is not an IP address", egress)
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

	oldEndpoint, oldEgress := dbRes.DREndpoint, dbRes.WANEgressAddress
	if endpoint != "" {
		dbRes.DREndpoint = endpoint
	}
	switch {
	case egress != "":
		dbRes.WANEgressAddress = egress
	case req.GetClearEgress():
		dbRes.WANEgressAddress = ""
	}
	if dbRes.DREndpoint == oldEndpoint && dbRes.WANEgressAddress == oldEgress {
		log.Debug("SetWanEndpoint: already set, nothing changed", zap.String("resource", name))
		return &pb.SetWanEndpointResponse{Success: true, Message: "already set; nothing changed",
			DrEndpoint: oldEndpoint, EgressAddress: oldEgress}, nil
	}
	if err := s.ctrl.db.SaveResource(ctx, dbRes); err != nil {
		return fail("save %s: %v", name, err)
	}
	log.Debug("SetWanEndpoint: new endpoint saved, rebuilding tunnels", zap.String("resource", name))
	s.ctrl.logger.Info("Changing WAN endpoint",
		zap.String("resource", name),
		zap.String("dr_endpoint_from", oldEndpoint), zap.String("dr_endpoint_to", dbRes.DREndpoint),
		zap.String("egress_from", oldEgress), zap.String("egress_to", dbRes.WANEgressAddress))

	rep := s.repairWanLegs(ctx, dbRes, false, req.GetSkipReachabilityCheck())
	if rep.Success {
		log.Debug("SetWanEndpoint: tunnels rebuilt on the new endpoint", zap.String("resource", name), zap.String("dr_endpoint", dbRes.DREndpoint))
		return &pb.SetWanEndpointResponse{
			Success:       true,
			Message:       fmt.Sprintf("%s now replicates to %s; %s", name, dbRes.DREndpoint, rep.Message),
			DrEndpoint:    dbRes.DREndpoint,
			EgressAddress: dbRes.WANEgressAddress,
		}, nil
	}
	if req.GetSkipReachabilityCheck() {
		log.Debug("SetWanEndpoint: rebuild failed, check skipped so the new endpoint stays", zap.String("message", rep.Message))
		// Nothing to fall back on: the operator asked for the new address
		// whatever it answers, and the record already says so.
		return &pb.SetWanEndpointResponse{Success: false,
			Message:    fmt.Sprintf("saved, but rebuilding the tunnels failed: %s; run wan repair %s once fixed", rep.Message, name),
			DrEndpoint: dbRes.DREndpoint, EgressAddress: dbRes.WANEgressAddress}, nil
	}

	// Put the old endpoint back and rebuild on it.
	log.Debug("SetWanEndpoint: new endpoint failed, restoring the old one", zap.String("dr_endpoint", oldEndpoint),
		zap.String("message", rep.Message))
	dbRes.DREndpoint, dbRes.WANEgressAddress = oldEndpoint, oldEgress
	if err := s.ctrl.db.SaveResource(ctx, dbRes); err != nil {
		return fail("%s; restoring the old endpoint also failed: %v", rep.Message, err)
	}
	back := s.repairWanLegs(ctx, dbRes, false, true)
	log.Debug("SetWanEndpoint: old endpoint rebuilt", zap.Bool("success", back.Success), zap.String("message", back.Message))
	msg := fmt.Sprintf("not changed: %s. %s still replicates to %s", rep.Message, name, oldEndpoint)
	if !back.Success {
		msg = fmt.Sprintf("not changed: %s. Rebuilding the tunnels on the old endpoint %s also failed: %s; run wan repair %s",
			rep.Message, oldEndpoint, back.Message, name)
	}
	return &pb.SetWanEndpointResponse{Success: false, Message: msg, DrEndpoint: oldEndpoint, EgressAddress: oldEgress}, nil
}
