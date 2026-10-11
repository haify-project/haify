package controller

import (
	"context"
	"fmt"
	"os/exec"
	"strings"

	"go.uber.org/zap"
)

// findActiveNode finds the node where the DRBD resource is currently Primary
func (rm *ResourceManager) findActiveNode(ctx context.Context, resource string, hosts []string) (string, error) {
	rm.controller.logger.Info("findActiveNode called",
		zap.String("resource", resource),
		zap.Int("hosts_count", len(hosts)))

	// Prefer drbd-reactor's structured JSON status for HA resources when available.
	for _, host := range hosts {
		promoter, err := rm.deployment.ReactorPromoterStatusByResource(ctx, host, resource)
		if err != nil || promoter == nil {
			continue
		}
		// A standby's drbd-reactor names a Primary it cannot place as
		// "unknown"; that is no node, so keep looking.
		if promoter.PrimaryOn != "" && !strings.EqualFold(promoter.PrimaryOn, "unknown") {
			if primaryHost := rm.getNodeHost(promoter.PrimaryOn); primaryHost != "" {
				rm.controller.logger.Info("Found active node from reactor status",
					zap.String("resource", resource),
					zap.String("primary_on", promoter.PrimaryOn),
					zap.String("resolved_host", primaryHost))
				return primaryHost, nil
			}
			rm.controller.logger.Info("Using reactor primary_on directly",
				zap.String("resource", resource),
				zap.String("primary_on", promoter.PrimaryOn))
			return promoter.PrimaryOn, nil
		}
	}

	var localHostname string

	// First, check local node using os/exec (not dispatch)
	// Get local hostname
	hostnameBytes, err := exec.Command("hostname").Output()
	if err != nil {
		rm.controller.logger.Warn("Failed to get local hostname", zap.Error(err))
		localHostname = ""
	} else {
		localHostname = strings.TrimSpace(string(hostnameBytes))
		rm.controller.logger.Info("Local hostname", zap.String("hostname", localHostname))

		// Check if local node is Primary
		// No need for sudo since haify-controller runs as root
		checkCmd := exec.Command("drbdsetup", "status", resource)
		output, err := checkCmd.Output()
		if err != nil {
			// Only an ExitError carries Stderr; a missing binary yields an
			// *exec.Error, so guard the type assertion (it used to panic when
			// drbdsetup was absent, e.g. in unit tests).
			var stderr string
			if ee, ok := err.(*exec.ExitError); ok {
				stderr = string(ee.Stderr)
			}
			rm.controller.logger.Warn("Failed to check local DRBD status",
				zap.Error(err),
				zap.String("stderr", stderr))
		} else {
			lines := strings.Split(string(output), "\n")
			rm.controller.logger.Info("Local DRBD status",
				zap.String("first_line", lines[0]),
				zap.Int("line_count", len(lines)))
			if len(lines) > 0 && strings.Contains(lines[0], "role:Primary") {
				rm.controller.logger.Info("Local node is Primary",
					zap.String("hostname", localHostname))
				return localHostname, nil
			}
			rm.controller.logger.Info("Local node is not Primary, checking remote hosts")
		}
	}

	rm.controller.logger.Info("Checking remote hosts",
		zap.Strings("hosts", hosts),
		zap.String("local_hostname", localHostname))

	for _, host := range hosts {
		// Skip if this is the local host
		if host == localHostname {
			rm.controller.logger.Info("Skipping local host",
				zap.String("host", host))
			continue
		}

		rm.controller.logger.Info("Checking remote host via dispatch",
			zap.String("host", host),
			zap.String("resource", resource))

		// Get DRBD role - check if this host is Primary
		cmd := fmt.Sprintf("drbdadm status %s", resource)
		result, err := rm.deployment.Exec(ctx, []string{host}, cmd)
		if err != nil {
			rm.controller.logger.Debug("Failed to check DRBD status",
				zap.String("host", host),
				zap.Error(err))
			continue
		}

		// Parse DRBD status output to find the Primary node
		if result != nil && len(result.Hosts) > 0 {
			for _, hr := range result.Hosts {
				if !hr.Success {
					continue
				}
				output := string(hr.Output)
				rm.controller.logger.Debug("Host result",
					zap.String("host", host),
					zap.Bool("success", hr.Success),
					zap.String("output", output))

				// Parse DRBD status to find which node is Primary
				// Output format:
				//   resource_name role:Secondary
				//     nodename1 role:Primary
				//     nodename2 role:Secondary
				lines := strings.Split(output, "\n")
				rm.controller.logger.Debug("Parsing DRBD status",
					zap.String("host", host),
					zap.Int("line_count", len(lines)))
				for i, line := range lines {
					trimmed := strings.TrimSpace(line)
					rm.controller.logger.Debug("Checking DRBD line",
						zap.String("host", host),
						zap.Int("line_index", i),
						zap.String("line", trimmed))
					// Check if this line defines a node's role
					// Format: "nodename role:Role" or "resource role:Role"
					if strings.Contains(trimmed, " role:Primary") {
						parts := strings.SplitN(trimmed, " ", 2)
						rm.controller.logger.Debug("Split result",
							zap.Int("parts_count", len(parts)),
							zap.String("part0", parts[0]),
							zap.String("part1", parts[1]))
						if len(parts) >= 2 && strings.HasPrefix(parts[1], "role:Primary") {
							primaryNode := strings.TrimSpace(parts[0])
							rm.controller.logger.Info("Found Primary node",
								zap.String("primary_node", primaryNode),
								zap.String("original_line", trimmed))

							// Get IP/hostname for the primary node
							primaryHost := rm.getNodeHost(primaryNode)
							if primaryHost != "" {
								rm.controller.logger.Info("Resolved primary node to host",
									zap.String("node", primaryNode),
									zap.String("host", primaryHost))
								return primaryHost, nil
							}
							// If not found in hosts map, return the node name directly
							rm.controller.logger.Info("Using node name directly",
								zap.String("node", primaryNode))
							return primaryNode, nil
						}
					}
				}
			}
		}
	}

	return "", fmt.Errorf("no active (Primary) node found for resource %s", resource)
}
