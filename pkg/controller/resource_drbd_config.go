package controller

import (
	"fmt"
	"sort"
	"strings"

	"go.uber.org/zap"
)

// generateDrbdConfig generates a DRBD resource configuration file for one or
// more volumes (volume 0..N). Diskful nodes share the resource-level volume
// blocks; diskless tiebreaker nodes override each with `disk none`.
//
// wan is nil for a normal LAN resource (output unchanged). When set, the config
// is rendered in WAN mode (protocol A + pull-ahead + loopback addresses).
func (rm *ResourceManager) generateDrbdConfig(name string, port uint32, volumes []resolvedVolume, nodes, disklessNodes []string, protocol, storageType string, options map[string]string, wan *wanConfig) string {
	log := rm.resourcesLog()
	// Option keys only: a value can be a DRBD shared-secret.
	log.Debug("generateDrbdConfig: start", zap.String("resource", name), zap.Uint32("port", port),
		zap.Int("volumes", len(volumes)), zap.Strings("nodes", nodes), zap.Strings("diskless_nodes", disklessNodes),
		zap.String("protocol", protocol), zap.String("storage_type", storageType), zap.Int("options", len(options)),
		zap.Bool("wan", wan != nil), zap.Bool("multi_primary", wan.multiPrimary()))
	var config strings.Builder

	// Organize options by section -> key -> value
	sections := make(map[string]map[string]string)

	// Helper to set option
	setOption := func(section, key, value string) {
		if sections[section] == nil {
			sections[section] = make(map[string]string)
		}
		sections[section][key] = value
	}

	// Add defaults
	setOption("options", "auto-promote", "no")
	// Always "majority" here, even for a WAN resource whose steady-state quorum
	// should exclude the DR. A fixed number is absolute from the very first
	// moment, including before any peer has ever connected, so it blocks the
	// force-promote that gives a brand-new resource its first UpToDate
	// generation. `majority` is adaptive to the membership DRBD has actually
	// seen, so it lets creation proceed. The numeric value is applied once the
	// resource is up and its peers are connected — see applyLocalSiteQuorum.
	setOption("options", "quorum", "majority")
	setOption("options", "on-no-quorum", "io-error")
	setOption("options", "on-no-data-accessible", "io-error")
	setOption("options", "on-suspended-primary-outdated", "force-secondary")

	setOption("net", "rr-conflict", "retry-connect")

	// WAN mode: force async protocol A and enable DRBD's own congestion
	// pull-ahead so the primary goes Ahead (keeps writing) instead of blocking
	// when the WAN buffer fills. These are defaults — the user-options loop below
	// still overrides any of them. See the design doc for the rationale.
	//
	// Only for the two-endpoint shape, where every connection crosses the WAN.
	// With a multi-replica primary site these belong to the WAN legs alone and
	// are emitted per connection: applying them at resource level would quietly
	// downgrade the synchronous primary-site mesh to async, which is the exact
	// guarantee those replicas exist to provide.
	if wan != nil && !wan.multiPrimary() {
		log.Debug("generateDrbdConfig: two-endpoint WAN; async with pull-ahead", zap.String("resource", name))
		protocol = "A"
		setOption("net", "on-congestion", "pull-ahead")
		setOption("net", "congestion-fill", wanCongestionFill(wan.Tuning))
		setOption("net", "congestion-extents", "500")
		setOption("net", "ko-count", wanKoCount)
		setOption("net", "ping-timeout", "20")
		setOption("net", "csums-alg", wanCsumsAlg)
	}

	// Process user options
	for k, v := range options {
		parts := strings.SplitN(k, "/", 2)
		if len(parts) == 2 {
			// section/key format (e.g. disk/on-io-error)
			section := strings.ToLower(parts[0])
			key := parts[1]
			setOption(section, key, v)
		} else {
			// default to options section
			setOption("options", k, v)
		}
	}

	fmt.Fprintf(&config, "resource %s {\n", name)

	// Write configuration sections
	knownSections := []string{"options", "net", "startup", "handlers"} // disk handled separately inside volume
	processed := make(map[string]bool)

	for _, s := range knownSections {
		opts, ok := sections[s]

		// Always write net section to include protocol
		if s == "net" {
			config.WriteString("\n    net {\n")
			fmt.Fprintf(&config, "        protocol %s;\n", protocol)
			// Without a verify algorithm `drbdadm verify` refuses to start,
			// so the one tool that proves two replicas hold the same data is
			// unavailable exactly when someone needs it. It costs nothing
			// until a verify runs. An explicit option still wins.
			if _, set := opts["verify-alg"]; !set {
				config.WriteString("        verify-alg crc32c;\n")
			}
			if ok {
				var keys []string
				for k := range opts {
					keys = append(keys, k)
				}
				sort.Strings(keys)
				for _, k := range keys {
					fmt.Fprintf(&config, "        %s %s;\n", k, opts[k])
				}
			}
			config.WriteString("    }\n")
			processed[s] = true
			// The resync settings of a two-endpoint WAN resource sit beside its
			// net block, where they apply to its one, WAN, peer (see
			// wan_drbd_options.go). An explicit disk option still wins.
			if wan != nil && !wan.multiPrimary() {
				config.WriteString("\n    disk {\n")
				for _, kv := range [][2]string{{"c-fill-target", wanCFillTarget}, {"c-plan-ahead", wanCPlanAhead}} {
					if _, set := sections["disk"][kv[0]]; !set {
						fmt.Fprintf(&config, "        %s %s;\n", kv[0], kv[1])
					}
				}
				config.WriteString("    }\n")
			}
			continue
		}

		if ok && len(opts) > 0 {
			fmt.Fprintf(&config, "\n    %s {\n", s)

			// Sort keys for deterministic output
			var keys []string
			for k := range opts {
				keys = append(keys, k)
			}
			sort.Strings(keys)

			for _, k := range keys {
				fmt.Fprintf(&config, "        %s %s;\n", k, opts[k])
			}
			config.WriteString("    }\n")
			processed[s] = true
		}
	}

	// Write any other custom sections (excluding disk which is handled in volume)
	var customSections []string
	for s := range sections {
		if !processed[s] && s != "disk" {
			customSections = append(customSections, s)
		}
	}
	sort.Strings(customSections)

	for _, s := range customSections {
		// Generic write
		opts := sections[s]
		fmt.Fprintf(&config, "\n    %s {\n", s)
		var keys []string
		for k := range opts {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(&config, "        %s %s;\n", k, opts[k])
		}
		config.WriteString("    }\n")
	}

	// Gather disk options once; they are applied to every volume block.
	var diskOptKeys []string
	if diskOpts, ok := sections["disk"]; ok && len(diskOpts) > 0 {
		for k := range diskOpts {
			diskOptKeys = append(diskOptKeys, k)
		}
		sort.Strings(diskOptKeys)
	}

	// Generate a resource-level block for each volume (volume 0..N).
	for _, v := range volumes {
		fmt.Fprintf(&config, "\n    volume %d {\n", v.id)
		fmt.Fprintf(&config, "        device    minor %d;\n", v.minor)

		// The ZFS or LVM device path per storage type — or, for an encrypted
		// volume, the crypt container that sits on top of it. DRBD must attach
		// to the mapping and never to the LV underneath: pointing it at the LV
		// would have it write plaintext straight past the layer that exists to
		// encrypt it.
		fmt.Fprintf(&config, "        disk      %s;\n", v.backingDevice(storageType))
		config.WriteString("        meta-disk internal;\n")

		if len(diskOptKeys) > 0 || v.exactBytes > 0 {
			diskOpts := sections["disk"]
			config.WriteString("        disk {\n")
			for _, k := range diskOptKeys {
				if k == "size" && v.exactBytes > 0 {
					continue
				}
				fmt.Fprintf(&config, "            %s %s;\n", k, diskOpts[k])
			}
			if v.exactBytes > 0 {
				fmt.Fprintf(&config, "            size %s;\n", drbdSizeSectors(v.exactBytes))
			}
			config.WriteString("        }\n")
		}

		config.WriteString("    }\n")
	}

	// Generate on sections for each node. Diskful nodes come first and share
	// the resource-level volume blocks above; diskless tiebreaker nodes follow
	// and override every volume with `disk none` so they join quorum without
	// storing data. node-id is the position in this combined ordering.
	allNodes := append(append([]string{}, nodes...), disklessNodes...)
	diskless := make(map[string]bool, len(disklessNodes))
	for _, n := range disklessNodes {
		diskless[n] = true
	}

	for i, node := range allNodes {
		// The REPLICATION address, which is the management address unless the node
		// was registered with a dedicated one. Using it here (and only here) is
		// what puts DRBD traffic on its own NIC/subnet while the controller keeps
		// reaching the node over the management address for SSH.
		ip := rm.controller.nodes.GetReplicationAddressByName(node)

		// Fallback: try direct lookup in hostMap
		if ip == "" {
			rm.mu.RLock()
			ip = rm.hostMap[node]
			rm.mu.RUnlock()
		}

		// Final fallback to node name if still not found
		if ip == "" {
			ip = node
		}

		// Resolve hostname to IP address if not already an IP
		ip = resolveToIP(ip)
		log.Debug("generateDrbdConfig: node stanza", zap.String("resource", name), zap.String("node", node),
			zap.String("address", ip), zap.Int("node_id", i), zap.Bool("diskless", diskless[node]))

		// `on <name>` must be the node's real hostname: drbdadm only applies a
		// resource to a host that finds itself in one of these sections. The
		// Haify node name is an operator-chosen label and may differ.
		fmt.Fprintf(&config, "\n    on %s {\n", rm.controller.nodes.GetDRBDNameByRef(node))
		if wan.multiPrimary() {
			// Multi-replica primary site: the per-node `address` is the LAN
			// address the other replicas reach it on. The WAN legs cannot use it
			// (they go through a loopback proxy) and are therefore written as
			// explicit `connection` sections after the host stanzas — DRBD lets
			// a connection override the endpoint addresses per peer pair, which
			// is the only way one node can speak LAN to its siblings and
			// loopback-proxy to the DR at the same time.
			if node == wan.DRNode {
				// The DR has no LAN peers; every one of its connections is a
				// WAN leg, so this address is never used. Keep it on loopback
				// so a stray direct connect cannot leave the tunnel.
				fmt.Fprintf(&config, "        address   127.0.0.1:%d;\n", port)
			} else {
				fmt.Fprintf(&config, "        address   %s:%d;\n", ip, port)
			}
		} else if wan != nil {
			// WAN: route through the local per-resource haify-proxy on loopback
			// instead of the peer's real IP. The DR node binds `port` (the
			// acceptor dials it there); the primary binds `port+9` and connects
			// out to `port` = the local dialer's drbd_listen. Both addresses are
			// loopback so each node reaches its own local proxy.
			addrPort := port + 9
			if node == wan.DRNode {
				addrPort = port
			}
			fmt.Fprintf(&config, "        address   127.0.0.1:%d;\n", addrPort)
		} else {
			fmt.Fprintf(&config, "        address   %s:%d;\n", ip, port)
		}
		fmt.Fprintf(&config, "        node-id   %d;\n", i)
		if diskless[node] {
			for _, v := range volumes {
				fmt.Fprintf(&config, "        volume %d {\n", v.id)
				fmt.Fprintf(&config, "            device    minor %d;\n", v.minor)
				config.WriteString("            disk      none;\n")
				config.WriteString("        }\n")
			}
		}
		config.WriteString("    }\n")
	}

	if wan.multiPrimary() {
		// Two-site topology: a synchronous mesh inside the primary site, plus
		// one asynchronous leg from every primary replica to the DR.
		//
		// The primary-site mesh is spelled out rather than left to
		// connection-mesh because connection-mesh would also pair each replica
		// with the DR using the host stanza addresses, which for a WAN peer are
		// meaningless (the DR is only reachable through the local proxy).
		drName := rm.controller.nodes.GetDRBDNameByRef(wan.DRNode)

		if len(allNodes) > 2 {
			// LAN mesh: every primary-site node with every other, including any
			// diskless tiebreaker, on their real addresses.
			lanHosts := make([]string, 0, len(allNodes))
			for _, node := range allNodes {
				if node == wan.DRNode {
					continue
				}
				lanHosts = append(lanHosts, rm.controller.nodes.GetDRBDNameByRef(node))
			}
			if len(lanHosts) > 1 {
				config.WriteString("\n    connection-mesh {\n")
				config.WriteString("        hosts")
				for _, h := range lanHosts {
					fmt.Fprintf(&config, " %s", h)
				}
				config.WriteString(";\n")
				config.WriteString("    }\n")
			}
		}

		// One explicit connection per WAN leg. Leg i uses loopback port
		// (port + wanLegPortStride*i) on the primary and the same +
		// wanDRLoopbackOffset on the DR, matching wanproxy's leg layout: each
		// tunnel gets a private pair of loopback endpoints so two legs cannot
		// collide on the DR, which terminates all of them.
		for i, primary := range wan.PrimaryNodes {
			primaryName := rm.controller.nodes.GetDRBDNameByRef(primary)
			legPort := port + uint32(i)
			config.WriteString("\n    connection {\n")
			// The primary binds a port the proxy does not use, and connects to
			// legPort — which on its own loopback is the local dialer. The DR
			// binds legPort, where its local acceptor dials it. Both ends read
			// the same numbers as their own loopback, which is what lets one
			// connection stanza describe a tunnel with two different endpoints.
			fmt.Fprintf(&config, "        host %s address 127.0.0.1:%d;\n", primaryName, wan.bindPort(int(legPort), i))
			fmt.Fprintf(&config, "        host %s address 127.0.0.1:%d;\n", drName, legPort)
			// Async across the WAN, whatever the LAN mesh uses. pull-ahead lets
			// a stalled tunnel drop behind instead of blocking the primary.
			writeWANLegOptions(&config, wan.Tuning)
			config.WriteString("    }\n")
		}
	} else if len(allNodes) > 2 {
		// Add connection-mesh for multi-node DRBD 9
		// DRBD 9 requires a full mesh of connections between all nodes
		config.WriteString("\n    connection-mesh {\n")
		config.WriteString("        hosts")
		for _, node := range allNodes {
			// Same rule as the `on` sections: the mesh lists DRBD host names.
			fmt.Fprintf(&config, " %s", rm.controller.nodes.GetDRBDNameByRef(node))
		}
		config.WriteString(";\n")
		config.WriteString("    }\n")
	}

	config.WriteString("}\n")

	log.Debug("generateDrbdConfig: generated", zap.String("resource", name), zap.String("protocol", protocol),
		zap.Int("members", len(allNodes)), zap.Int("bytes", config.Len()))
	return config.String()
}
