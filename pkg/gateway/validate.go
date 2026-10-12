package gateway

import (
	"fmt"
	"regexp"
	"strings"

	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// The identifiers a gateway is created with — the IQN, the NQN, the NVMe
// transport type — are never interpreted by the controller. They are copied
// verbatim into the drbd-reactor promoter config and only reach a parser when
// drbd-reactor promotes the resource and hands them to the OCF agents.
//
// That is why they are validated here, at the gateway API boundary, rather than
// in a request handler: by the time an unparseable value would be noticed, the
// DRBD resource exists, the cluster-private state volume has been provisioned,
// and the config file has been distributed to every node — and the failure
// surfaces as a promoter that will not start, nowhere near the request that
// caused it. Rejecting the value costs nothing here and a full gateway teardown
// one step later.
//
// The gateway managers are reached from the CLI, the gRPC server and the MCP
// server alike (all three go through pkg/controller's handlers), so this is the
// single layer that covers every caller.

// invalidArgument reports a rejected caller-supplied value.
//
// The gateway managers' errors are returned straight through pkg/controller's
// gRPC handlers, so an unclassified error reaches the operator as
// codes.Unknown — indistinguishable from a node being unreachable. Tagging it
// InvalidArgument is what tells a caller (and the CLI's error rendering) that
// retrying will not help until the request itself is fixed.
func invalidArgument(err error) error {
	return status.Error(codes.InvalidArgument, err.Error())
}

// iSCSI node names, as LIO accepts them.
//
// Target and initiator names both end up in targetcli (`/iscsi create`,
// `acls create`), which checks them with rtslib's normalize_wwn for the iSCSI
// fabric: the name is lower-cased, then must be one of
//
//	iqn  iqn\.[0-9]{4}-[0-1][0-9]\..*\..*  with no space and no "_"
//	naa  naa\.[125][0-9a-f]{15}
//	eui  eui\.[0-9a-f]{16}
//
// A name that fails is not refused when haify writes the config but when the
// agent starts the gateway: the iSCSITarget start fails with "WWN not valid as:
// iqn, naa, eui", and the gateway stays down on every node it is tried on. On
// sdt, `initiator add iqn.2026-10.test:probe` did exactly that — "test:probe"
// has no second dot. So the rules here are rtslib's, not a looser reading of
// RFC 3720: an RFC-legal name LIO rejects is an outage, not a valid name.
//
// naa: rtslib-fb accepts a first digit of 1, 2, 5 and, in newer releases,
// c-f; only the digits every release accepts are accepted here. A 32-digit
// (NAA type 6) name is a LUN WWN, never an iSCSI name LIO will take.
//
// The prefix must be lower case, as RFC 3720 writes it; the rest may be any
// case (LIO lower-cases it). Quotes, backslashes, "$" and "`" are refused on
// top: LIO would take them, but the name is carried inside a quoted OCF
// parameter in the promoter config and a shell command on the nodes.
const (
	iqnPrefix = "iqn."
	euiPrefix = "eui."
	naaPrefix = "naa."
)

// iscsiNameFormats is the operator-facing description of what validateIQN
// accepts, kept in one place so that every rejection names all three formats.
const iscsiNameFormats = `accepted formats are ` +
	`"iqn." + year-month + "." + reversed domain with at least two labels, optionally ":" + local name (iqn.2024-01.com.example:haify.data), ` +
	`"eui." + 16 hex digits (eui.0123456789abcdef), or ` +
	`"naa." + 16 hex digits starting with 1, 2 or 5 (naa.5001405f1b2c3d4e)`

var (
	lioIQNRE = regexp.MustCompile(`^iqn\.[0-9]{4}-[0-1][0-9]\..*\..*`)
	lioEUIRE = regexp.MustCompile(`^eui\.[0-9a-f]{16}$`)
	lioNAARE = regexp.MustCompile(`^naa\.[125][0-9a-f]{15}$`)
)

// validateIQN validates an iSCSI target or initiator name against the rules
// LIO applies (see above).
func validateIQN(name string) error {
	log := zap.L()
	if strings.ContainsAny(name, "\"'\\$`") {
		log.Debug("validateIQN: shell-unsafe characters", zap.String("iqn", name))
		return fmt.Errorf("invalid iSCSI name %q: quotes, backslashes, \"$\" and \"`\" are not allowed", name)
	}
	lower := strings.ToLower(name)
	switch {
	case strings.HasPrefix(name, iqnPrefix):
		if !lioIQNRE.MatchString(lower) || strings.ContainsAny(lower, " \t_") {
			log.Debug("validateIQN: iqn name LIO refuses", zap.String("iqn", name))
			return fmt.Errorf("invalid iSCSI name %q: LIO (targetcli) refuses it — an %q name needs a year-month "+
				"date, a reversed domain of at least two labels, and no spaces or \"_\"; %s", name, iqnPrefix, iscsiNameFormats)
		}
	case strings.HasPrefix(name, euiPrefix):
		if !lioEUIRE.MatchString(lower) {
			log.Debug("validateIQN: malformed eui name", zap.String("iqn", name))
			return fmt.Errorf("invalid iSCSI name %q: %q must be followed by exactly 16 hexadecimal digits; %s",
				name, euiPrefix, iscsiNameFormats)
		}
	case strings.HasPrefix(name, naaPrefix):
		if !lioNAARE.MatchString(lower) {
			log.Debug("validateIQN: malformed naa name", zap.String("iqn", name))
			return fmt.Errorf("invalid iSCSI name %q: %q must be followed by 16 hexadecimal digits, the first "+
				"1, 2 or 5; %s", name, naaPrefix, iscsiNameFormats)
		}
	default:
		log.Debug("validateIQN: unknown name format", zap.String("iqn", name))
		return fmt.Errorf("invalid iSCSI name %q: %s", name, iscsiNameFormats)
	}
	return nil
}

// maxNQNLength is NVMF_NQN_SIZE: the NVMe base specification caps an NQN at
// 223 bytes.
const maxNQNLength = 223

var (
	hostNQNRE     = regexp.MustCompile(`^nqn\.[0-9]{4}-(0[1-9]|1[0-2])\.[A-Za-z0-9-]+(\.[A-Za-z0-9-]+)+(:\S+)?$`)
	uuidNQNPrefix = "nqn.2014-08.org.nvmexpress:uuid:"
	uuidRE        = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
)

// validateHostNQN validates an NVMe host NQN for a subsystem's allow-list:
// nqn.<yyyy-mm>.<reversed domain>[:<name>], or the UUID form
// nqn.2014-08.org.nvmexpress:uuid:<uuid> that nvme-cli writes to
// /etc/nvme/hostnqn. The NQN becomes a directory under
// /sys/kernel/config/nvmet/hosts and a word in the promoter config's
// allowed_initiators list, so "/", quotes and blanks are refused too.
func validateHostNQN(nqn string) error {
	const formats = `expected nqn.<yyyy-mm>.<reversed domain>[:<name>] (nqn.2024-01.com.example:host1) ` +
		`or nqn.2014-08.org.nvmexpress:uuid:<uuid>`
	log := zap.L()
	switch {
	case len(nqn) > maxNQNLength:
		log.Debug("validateHostNQN: too long", zap.Int("length", len(nqn)))
		return fmt.Errorf("invalid host NQN %q: longer than %d bytes", nqn, maxNQNLength)
	case strings.ContainsAny(nqn, "/\"'\\$`"):
		log.Debug("validateHostNQN: unsafe characters", zap.String("host_nqn", nqn))
		return fmt.Errorf("invalid host NQN %q: \"/\", quotes, backslashes, \"$\" and \"`\" are not allowed", nqn)
	case strings.HasPrefix(nqn, uuidNQNPrefix):
		if !uuidRE.MatchString(strings.TrimPrefix(nqn, uuidNQNPrefix)) {
			log.Debug("validateHostNQN: UUID form without a UUID", zap.String("host_nqn", nqn))
			return fmt.Errorf("invalid host NQN %q: what follows %q must be a UUID", nqn, uuidNQNPrefix)
		}
	case !hostNQNRE.MatchString(nqn):
		log.Debug("validateHostNQN: malformed", zap.String("host_nqn", nqn))
		return fmt.Errorf("invalid host NQN %q: %s", nqn, formats)
	}
	return nil
}

// validateNQN validates an NQN format
func validateNQN(nqn string) error {
	if !strings.HasPrefix(nqn, "nqn.") {
		zap.L().Debug("validateNQN: missing nqn. prefix", zap.String("nqn", nqn))
		return fmt.Errorf("invalid NQN format: must start with 'nqn.'")
	}

	parts := strings.Split(nqn, ":")
	if len(parts) < 2 {
		zap.L().Debug("validateNQN: missing colon", zap.String("nqn", nqn))
		return fmt.Errorf("invalid NQN format: missing colon separator")
	}

	return nil
}

// parseTransportType parses and validates an NVMe transport type. fc is not
// accepted: the generated nvmet-port line addresses the port by the service
// IP, and an FC port is addressed by WWNN/WWPN.
func parseTransportType(transport string) error {
	validTypes := []string{"tcp", "rdma"}
	for _, t := range validTypes {
		if transport == t {
			return nil
		}
	}
	zap.L().Debug("parseTransportType: unknown transport", zap.String("transport", transport))
	return fmt.Errorf("invalid transport type: %s (valid: %v)", transport, validTypes)
}

// validateISCSIImplementation normalises the requested iSCSI target
// implementation and refuses the ones the generated config does not support.
//
// "lio" historically meant the long-gone lio_node toolchain; every current
// distribution ships targetcli, which the OCF agents call "lio-t". tgt and iet
// are refused rather than passed through: with either, the iSCSITarget agent
// ignores portals= and listens on every address instead of the service IP, tgt
// needs a tgtd daemon that nothing in the promoter chain starts, and IET is not
// packaged by current distributions. The stop order the chain relies on
// (service IP first, then LUNs, then target) has only been verified on LIO.
func validateISCSIImplementation(implementation string) (string, error) {
	zap.L().Debug("validateISCSIImplementation: checking", zap.String("implementation", implementation))
	switch implementation {
	case "", "lio", "lio-t":
		return "lio-t", nil
	case "tgt":
		return "", fmt.Errorf("iSCSI implementation \"tgt\" is not supported: Haify gateways run on LIO (targetcli) only; " +
			"with tgt the iSCSITarget agent ignores the service-IP portal and needs a tgtd daemon the promoter chain " +
			"does not start. Use --implementation lio")
	case "iet":
		return "", fmt.Errorf("iSCSI implementation \"iet\" is not supported: Haify gateways run on LIO (targetcli) only; " +
			"with iet the iSCSITarget agent ignores the service-IP portal, and IET is not packaged by current " +
			"distributions. Use --implementation lio")
	default:
		return "", fmt.Errorf("unknown iSCSI implementation %q: Haify gateways run on LIO (targetcli) only; use --implementation lio", implementation)
	}
}
