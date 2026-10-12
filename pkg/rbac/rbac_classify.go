package rbac

import (
	"regexp"
	"strings"

	"go.uber.org/zap"
)

// appMethodRe matches the database application methods (CreateApp,
// ListApps, GetAppStatus, ...) and nothing that merely starts with "App",
// such as the approval methods.
var appMethodRe = regexp.MustCompile(`^[A-Z][a-z]+(App|Apps|AppStatus)$`)

// readVerbs are method-name prefixes that denote a read-only operation.
var readVerbs = []string{"List", "Get", "Describe", "Watch", "Stream", "Check"}

// Classify maps a gRPC full method (e.g. "/v1.HaifyController/CreateNFSGateway")
// to the (object, action) pair used for authorization. Unknown methods map to
// the "system" object and, unless clearly read-only, the "write" action — so a
// newly added RPC is locked down by default rather than silently permitted.
func Classify(fullMethod string) (object, action string) {
	method := fullMethod
	if i := strings.LastIndex(fullMethod, "/"); i >= 0 {
		method = fullMethod[i+1:]
	}
	object, action = classifyObject(method), classifyAction(method)
	zap.L().Debug("Classify: classified", zap.String("method", fullMethod), zap.String("object", object), zap.String("action", action))
	return object, action
}

func classifyAction(method string) string {
	log := zap.L()
	if method == "ApproveRequest" || method == "RejectRequest" {
		log.Debug("classifyAction: approval method", zap.String("method", method))
		return ActApprove
	}
	// An inspection reads the cluster and stores a report; it changes nothing
	// on it, so whoever may read the cluster may run one.
	if strings.Contains(method, "Status") || method == "RunInspection" {
		log.Debug("classifyAction: status or inspection, read", zap.String("method", method))
		return ActRead
	}
	for _, v := range readVerbs {
		if strings.HasPrefix(method, v) {
			log.Debug("classifyAction: read verb prefix", zap.String("method", method), zap.String("verb", v))
			return ActRead
		}
	}
	log.Debug("classifyAction: no read marker, write", zap.String("method", method))
	return ActWrite
}

func classifyObject(method string) string {
	zap.L().Debug("classifyObject: start", zap.String("method", method))
	switch {
	case method == "ListApprovals" || method == "ApproveRequest" || method == "RejectRequest":
		return "approval"
	// Before the snapshot case: SnapshotApp freezes a database and is part of
	// running it, so whoever may run apps may snapshot them.
	case appMethodRe.MatchString(method):
		return "app"
	// Before the snapshot case: off-cluster backups are their own object, so an
	// operator who may ship data off-site can be distinguished from one who may
	// only snapshot in place. Unclassified would land it in "system", where an
	// operator has read only — a working feature nobody but admin could use.
	// Shipping a resource to another cluster sends its data off-site just as
	// a backup does, on both the sending and the receiving controller.
	case strings.Contains(method, "Backup"), strings.Contains(method, "Ship"):
		return "backup"
	case strings.Contains(method, "Snapshot"):
		return "snapshot"
	case containsAny(method, "Gateway", "NFS", "ISCSI", "NVMe", "SMB", "Chap", "Export", "Initiator", "LUN", "Host", "Namespace"):
		return "gateway"
	case containsAny(method, "Pool", "Disk", "ZFSDataset", "ZFSVolume", "ZFSpool"):
		return "pool"
	case containsAny(method, "SelfHa", "Ha"):
		return "ha"
	case containsAny(method, "Node", "Register"):
		return "node"
	case containsAny(method, "Resource", "Volume", "Filesystem", "Primary", "Secondary", "Mount"):
		return "resource"
	default:
		return "system"
	}
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
