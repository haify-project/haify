package controller

import (
	"context"
	"fmt"
	"net"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	haifypb "github.com/haify-project/haify/api/proto/v1"
	"github.com/haify-project/haify/pkg/database"
	"github.com/haify-project/haify/pkg/gateway"
	"go.uber.org/zap"
)

func gatewayServiceHost(serviceIP string) string {
	log := zap.L()
	serviceIP = strings.TrimSpace(serviceIP)
	if serviceIP == "" {
		log.Debug("gatewayServiceHost: no service IP")
		return ""
	}

	ip, _, err := net.ParseCIDR(serviceIP)
	if err == nil {
		log.Debug("gatewayServiceHost: parsed CIDR", zap.String("service_ip", serviceIP), zap.String("host", ip.String()))
		return ip.String()
	}

	host, _, found := strings.Cut(serviceIP, "/")
	if found {
		log.Debug("gatewayServiceHost: not a valid CIDR, took the part before the slash",
			zap.String("service_ip", serviceIP), zap.String("host", host), zap.Error(err))
		return host
	}
	log.Debug("gatewayServiceHost: bare address", zap.String("service_ip", serviceIP))
	return serviceIP
}

func gatewayExportDirectory(resource, exportPath string) string {
	log := zap.L()
	exportPath = strings.TrimSpace(exportPath)
	if exportPath == "" {
		log.Debug("gatewayExportDirectory: no export path, using the default under the base",
			zap.String("resource", resource), zap.String("base", gateway.DefaultExportBasePath))
		return filepath.Join(gateway.DefaultExportBasePath, resource)
	}
	if strings.HasPrefix(exportPath, gateway.DefaultExportBasePath+string(filepath.Separator)) {
		log.Debug("gatewayExportDirectory: export path already under the base",
			zap.String("resource", resource), zap.String("export_path", exportPath))
		return exportPath
	}
	log.Debug("gatewayExportDirectory: export path joined under the resource directory",
		zap.String("resource", resource), zap.String("export_path", exportPath))
	return filepath.Join(gateway.DefaultExportBasePath, resource, strings.TrimPrefix(exportPath, "/"))
}

func stringifyGatewayValue(value interface{}) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case string:
		return typed
	case fmt.Stringer:
		return typed.String()
	case []string:
		return strings.Join(typed, ",")
	case []interface{}:
		parts := make([]string, 0, len(typed))
		for _, item := range typed {
			if value := stringifyGatewayValue(item); value != "" {
				parts = append(parts, value)
			}
		}
		return strings.Join(parts, ",")
	case map[string]string:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		slices.Sort(keys)
		parts := make([]string, 0, len(keys))
		for _, key := range keys {
			parts = append(parts, fmt.Sprintf("%s=%s", key, typed[key]))
		}
		return strings.Join(parts, ",")
	case map[string]interface{}:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		slices.Sort(keys)
		parts := make([]string, 0, len(keys))
		for _, key := range keys {
			value := stringifyGatewayValue(typed[key])
			if value == "" {
				continue
			}
			parts = append(parts, fmt.Sprintf("%s=%s", key, value))
		}
		return strings.Join(parts, ",")
	case bool:
		return strconv.FormatBool(typed)
	case int:
		return strconv.Itoa(typed)
	case int32:
		return strconv.FormatInt(int64(typed), 10)
	case int64:
		return strconv.FormatInt(typed, 10)
	case uint:
		return strconv.FormatUint(uint64(typed), 10)
	case uint32:
		return strconv.FormatUint(uint64(typed), 10)
	case uint64:
		return strconv.FormatUint(typed, 10)
	case float32:
		return strconv.FormatFloat(float64(typed), 'f', -1, 32)
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64)
	default:
		return fmt.Sprint(typed)
	}
}

func formatGatewayNodeStates(nodeStates map[string]*ResourceNodeState) string {
	if len(nodeStates) == 0 {
		zap.L().Debug("formatGatewayNodeStates: no node states")
		return ""
	}

	keys := make([]string, 0, len(nodeStates))
	for key := range nodeStates {
		keys = append(keys, key)
	}
	slices.Sort(keys)

	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		state := nodeStates[key]
		if state == nil {
			zap.L().Debug("formatGatewayNodeStates: node has no state, skipped", zap.String("node", key))
			continue
		}
		parts = append(parts, fmt.Sprintf(
			"%s:%s/%s/%s",
			key,
			state.Role,
			state.DiskState,
			state.Replication,
		))
	}

	zap.L().Debug("formatGatewayNodeStates: formatted", zap.Int("count", len(parts)))
	return strings.Join(parts, ",")
}

func (s *Server) gatewayConfigOptions(ctx context.Context, resource string) map[string]string {
	log := s.ctrl.traceLogger()
	log.Debug("Server.gatewayConfigOptions: start", zap.String("resource", resource))
	options := make(map[string]string)
	if s.ctrl.db == nil {
		log.Debug("Server.gatewayConfigOptions: no database, no options", zap.String("resource", resource))
		return options
	}

	start := time.Now()
	dbGateway, err := s.ctrl.db.GetGatewayByResource(ctx, resource)
	log.Debug("Server.gatewayConfigOptions: database read gateway", zap.String("resource", resource),
		zap.Bool("found", err == nil), zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return options
	}

	// Values are not logged: a gateway's config can hold CHAP or SMB secrets.
	for key, value := range dbGateway.Config {
		switch typed := value.(type) {
		case map[string]interface{}:
			for subKey, subValue := range typed {
				stringValue := stringifyGatewayValue(subValue)
				if stringValue == "" {
					continue
				}
				options[key+"."+subKey] = stringValue
			}
		default:
			stringValue := stringifyGatewayValue(value)
			if stringValue == "" {
				continue
			}
			options[key] = stringValue
		}
	}

	if dbGateway.Status != "" {
		options["gateway_status"] = dbGateway.Status
	}
	if dbGateway.ActiveNode != "" {
		options["active_node"] = dbGateway.ActiveNode
	}

	log.Debug("Server.gatewayConfigOptions: options built", zap.String("resource", resource),
		zap.Int("count", len(options)), zap.String("status", dbGateway.Status), zap.String("active_node", dbGateway.ActiveNode))
	return options
}

func gatewayInfoFromDBRecord(record *database.Gateway) *gateway.GatewayInfo {
	if record == nil {
		return nil
	}
	return &gateway.GatewayInfo{
		ID:       record.Resource,
		Name:     record.Name,
		Type:     string(record.Type),
		Resource: record.Resource,
	}
}

func (s *Server) getGatewayInfo(ctx context.Context, resource string) (*gateway.GatewayInfo, error) {
	log := s.ctrl.traceLogger()
	log.Debug("Server.getGatewayInfo: start", zap.String("resource", resource),
		zap.Bool("has_gateway_manager", s.gateway != nil), zap.Bool("has_db", s.ctrl.db != nil))
	if s.gateway != nil {
		start := time.Now()
		gw, err := s.gateway.GetGateway(ctx, resource)
		log.Debug("Server.getGatewayInfo: gateway manager lookup", zap.String("resource", resource),
			zap.Bool("found", err == nil), zap.Duration("took", time.Since(start)), zap.Error(err))
		if err == nil {
			return gw, nil
		}
	}

	if s.ctrl.db != nil {
		start := time.Now()
		record, err := s.ctrl.db.GetGatewayByResource(ctx, resource)
		log.Debug("Server.getGatewayInfo: database read gateway", zap.String("resource", resource),
			zap.Bool("found", err == nil), zap.Duration("took", time.Since(start)), zap.Error(err))
		if err == nil {
			return gatewayInfoFromDBRecord(record), nil
		}
	}

	log.Debug("Server.getGatewayInfo: gateway not found", zap.String("resource", resource))
	return nil, fmt.Errorf("gateway not found: %s", resource)
}

func (s *Server) listGatewayInfos(ctx context.Context) ([]*gateway.GatewayInfo, error) {
	log := s.ctrl.traceLogger()
	log.Debug("Server.listGatewayInfos: start",
		zap.Bool("has_gateway_manager", s.gateway != nil), zap.Bool("has_db", s.ctrl.db != nil))
	if s.gateway != nil {
		start := time.Now()
		gateways, err := s.gateway.ListGateways(ctx)
		log.Debug("Server.listGatewayInfos: gateway manager list", zap.Int("count", len(gateways)),
			zap.Duration("took", time.Since(start)), zap.Error(err))
		if err == nil && len(gateways) > 0 {
			return gateways, nil
		}
		log.Debug("Server.listGatewayInfos: gateway manager returned none, falling back to the database")
	}

	if s.ctrl.db != nil {
		start := time.Now()
		records, err := s.ctrl.db.ListGateways(ctx)
		log.Debug("Server.listGatewayInfos: database list gateways", zap.Int("count", len(records)),
			zap.Duration("took", time.Since(start)), zap.Error(err))
		if err != nil {
			return nil, err
		}
		gateways := make([]*gateway.GatewayInfo, 0, len(records))
		for _, record := range records {
			log.Debug("Server.listGatewayInfos: gateway from database", zap.String("resource", record.Resource),
				zap.String("type", string(record.Type)))
			gateways = append(gateways, gatewayInfoFromDBRecord(record))
		}
		return gateways, nil
	}

	log.Debug("Server.listGatewayInfos: no gateway manager and no database, empty list")
	return []*gateway.GatewayInfo{}, nil
}

func (s *Server) gatewayRuntimeInfo(ctx context.Context, resource string) (string, string, map[string]string) {
	log := s.ctrl.traceLogger()
	log.Debug("Server.gatewayRuntimeInfo: start", zap.String("resource", resource))
	options := make(map[string]string)
	state := "configured"
	activeNode := ""

	if s.resources == nil {
		log.Debug("Server.gatewayRuntimeInfo: no resource manager, reporting configured", zap.String("resource", resource))
		return state, activeNode, options
	}

	start := time.Now()
	info, err := s.resources.GetResource(ctx, resource)
	log.Debug("Server.gatewayRuntimeInfo: resource lookup", zap.String("resource", resource),
		zap.Bool("found", err == nil && info != nil), zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil || info == nil {
		return state, activeNode, options
	}

	if info.Role != "" {
		options["role"] = info.Role
	}
	if len(info.Nodes) > 0 {
		options["nodes"] = strings.Join(info.Nodes, ",")
	}
	if len(info.Volumes) > 0 {
		options["volumes"] = strconv.Itoa(len(info.Volumes))
	}
	if nodeStates := formatGatewayNodeStates(info.NodeStates); nodeStates != "" {
		options["node_states"] = nodeStates
	}

	for nodeName, nodeState := range info.NodeStates {
		if nodeState != nil && strings.EqualFold(nodeState.Role, "Primary") {
			activeNode = nodeName
			break
		}
	}
	// "started" must mean the promoter's services actually came up, not merely
	// that the DRBD resource is Primary. A gateway whose target failed to start
	// (missing agent, mount error, ...) is reported as "failed" so the UI does
	// not show a broken gateway as running.
	if activeNode != "" {
		start := time.Now()
		if s.gateway != nil && s.gateway.GatewayServiceActive(ctx, s.ctrl.ResolveHost(activeNode), resource) {
			state = "started"
		} else {
			state = "failed"
		}
		log.Debug("Server.gatewayRuntimeInfo: checked the promoter's services on the Primary",
			zap.String("resource", resource), zap.String("node", activeNode), zap.String("state", state),
			zap.Bool("has_gateway_manager", s.gateway != nil), zap.Duration("took", time.Since(start)))
	} else {
		log.Debug("Server.gatewayRuntimeInfo: no Primary, gateway stays configured", zap.String("resource", resource))
	}

	log.Debug("Server.gatewayRuntimeInfo: done", zap.String("resource", resource), zap.String("state", state),
		zap.String("active_node", activeNode), zap.Int("options", len(options)))
	return state, activeNode, options
}

func (s *Server) enrichGatewayInfo(ctx context.Context, gw *gateway.GatewayInfo) *haifypb.GatewayInfo {
	log := s.ctrl.traceLogger()
	log.Debug("Server.enrichGatewayInfo: start", zap.String("resource", gw.Resource), zap.String("type", gw.Type))
	options := s.gatewayConfigOptions(ctx, gw.Resource)
	state, node, runtimeOptions := s.gatewayRuntimeInfo(ctx, gw.Resource)
	for key, value := range runtimeOptions {
		options[key] = value
	}
	if state == "configured" && options["gateway_status"] != "" {
		log.Debug("Server.enrichGatewayInfo: no live state, using the recorded status",
			zap.String("resource", gw.Resource), zap.String("status", options["gateway_status"]))
		state = options["gateway_status"]
	}
	if node == "" && options["active_node"] != "" {
		log.Debug("Server.enrichGatewayInfo: no live Primary, using the recorded active node",
			zap.String("resource", gw.Resource), zap.String("node", options["active_node"]))
		node = options["active_node"]
	}

	info := &haifypb.GatewayInfo{
		Id:       gw.ID,
		Name:     gw.Name,
		Type:     gw.Type,
		State:    state,
		Node:     node,
		Resource: gw.Resource,
		Options:  options,
	}

	if gw.Type == "nfs" || info.Path == "" {
		info.Path = options["export_directory"]
	}

	log.Debug("Server.enrichGatewayInfo: done", zap.String("resource", gw.Resource), zap.String("state", state),
		zap.String("node", node), zap.String("path", info.Path))
	return info
}
