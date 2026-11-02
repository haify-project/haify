# shellcheck shell=bash
# shellcheck disable=SC2153 # the globals live in lib.sh and haify-install.sh
# Step 1 and the plan: which machines are the nodes, root SSH to them, which
# platform to connect, which disks take the pool, where the controller runs.

# ssh_opts_for replaces the PVE one (lib.sh): outside PVE there is no
# cluster-kept known_hosts, so a node's key is learnt on first contact and
# checked from then on.
#
# Each node gets one master connection that every later ssh and scp to it
# rides: an install makes dozens of calls per node, and a fresh handshake
# each time made a dry run of three nodes take over a minute. The master is
# started on its own with /dev/null for all three streams, because one
# spawned by ControlMaster=auto inside $(...) would hold the capture open
# until ControlPersist ran out. A node it cannot reach is not retried.
ssh_opts_for() {
	SSH_OPTS=(-o BatchMode=yes -o ConnectTimeout=10 -o StrictHostKeyChecking=accept-new)
	[ -n "${WORK_DIR:-}" ] && [ -d "$WORK_DIR" ] || return 0
	local node="$1" sock="$WORK_DIR/ssh-$1"
	if [ ! -S "$sock" ] && [ ! -e "$sock.none" ]; then
		if ssh "${SSH_OPTS[@]}" -o ControlMaster=yes -o "ControlPath=$sock" -o ControlPersist=300 \
			-fN "root@$(node_ip "$node")" </dev/null >/dev/null 2>&1 && [ -S "$sock" ]; then
			:
		else
			: > "$sock.none"
		fi
	fi
	[ -S "$sock" ] && SSH_OPTS+=(-o ControlMaster=no -o "ControlPath=$sock")
	return 0
}

# ssh_close_masters ends the master connections before WORK_DIR goes.
ssh_close_masters() {
	local s
	for s in "${WORK_DIR:-/nonexistent}"/ssh-*; do
		[ -S "$s" ] && ssh -o "ControlPath=$s" -O exit x >/dev/null 2>&1
	done
	return 0
}

LOCAL_ENV=plain
detect_local() {
	local out e
	# HAIFY_LOCAL_ENV stands in for this machine's probe (the tests).
	if [ -n "${HAIFY_LOCAL_ENV:-}" ]; then
		LOCAL_ENV="$HAIFY_LOCAL_ENV"
		return 0
	fi
	out=$(bash -c "$DETECT_PROBE" 2>/dev/null || true)
	for e in "${ENVS[@]}"; do
		printf '%s\n' "$out" | grep -qx "$e" && { LOCAL_ENV="$e"; return 0; }
	done
	return 0
}

local_addresses() { hostname -I 2>/dev/null | tr ' ' '\n'; ip -o addr show 2>/dev/null | awk '{ sub(/\/.*/, "", $4); print $4 }'; }

# add_node <name> <ip>
add_node() {
	in_list "$1" "${NODES[@]}" && die "node '$1' is listed twice"
	NODES+=("$1")
	NODE_IP["$1"]="$2"
	# Matched against a captured list: under pipefail, grep -q leaving early
	# would fail the pipe it reads from.
	local mine
	mine=$(local_addresses)
	if printf '%s\n' "$mine" | grep -qxF "$2"; then LOCAL_NODE="$1"; fi
}

find_nodes() {
	local item name ip members env="${ENV:-$LOCAL_ENV}"
	if [ -n "$NODES_ARG" ]; then
		local items=()
		split_csv "$NODES_ARG" items
		for item in "${items[@]}"; do
			case "$item" in
				?*=?*) add_node "${item%%=*}" "${item#*=}" ;;
				*)
					# An address alone: the node is called what it calls itself.
					NODE_IP["$item"]="$item"
					if [ "$PROBE_JSON" = 1 ] && ! reachable "$item"; then
						# Reported as unreachable under its address.
						unset 'NODE_IP[$item]'
						add_node "$item" "$item"
						continue
					fi
					ensure_root_ssh "$item"
					name=$(check_on "$item" "hostname -s")
					unset 'NODE_IP[$item]'
					add_node "$name" "$item"
					;;
			esac
		done
		return 0
	fi
	members=$(members_of "$env" || true)
	if [ -n "$members" ]; then
		log "nodes from $(env_title "$env")'s own member list (--nodes to choose others)"
		while IFS=$'\t' read -r name ip; do
			[ -n "$name" ] && [ -n "$ip" ] && add_node "$name" "$ip"
		done <<< "$members"
		return 0
	fi
	ip=$(ip -4 route get 1.1.1.1 2>/dev/null | sed -n 's/.* src \([0-9.]*\).*/\1/p')
	[ -n "$ip" ] || die "cannot tell this machine's address; name the nodes with --nodes"
	log "no member list to read: installing on this machine alone (--nodes to add others)"
	add_node "$(hostname -s)" "$ip"
}

# ensure_root_ssh <node>: root SSH in batch mode works, or, on a terminal,
# is set up with ssh-copy-id (it asks for the node's root password once).
ensure_root_ssh() {
	local node="$1" ip
	reachable "$node" && return 0
	ip=$(node_ip "$node")
	if [ "$INTERACTIVE" = 1 ] && [ "$DRY_RUN" = 0 ]; then
		[ -f /root/.ssh/id_ed25519 ] || [ -f /root/.ssh/id_rsa ] || ssh-keygen -q -t ed25519 -N "" -f /root/.ssh/id_ed25519
		printf 'Root SSH to %s (%s) does not work yet; copying this machine'"'"'s key there.\n' "$node" "$ip"
		ssh-copy-id -o StrictHostKeyChecking=accept-new "root@$ip" >/dev/null || true
		reachable "$node" && return 0
	fi
	die "root SSH from this machine to $node ($ip) failed in batch mode. Set it up, then rerun:
  ssh-copy-id root@$ip && ssh -o BatchMode=yes root@$ip true"
}

choose_env() {
	local n e cands=() counts=() i choice best=""
	for n in "${NODES[@]}"; do
		e=$(primary_env ${NODE_ENVS[$n]})
		in_list "$e" "${cands[@]}" || cands+=("$e")
	done
	if [ -n "$ENV" ]; then
		in_list "$ENV" "${cands[@]}" || warn "--env $ENV, but no node looks like $(env_title "$ENV"); detected: ${cands[*]}"
	elif [ "${#cands[@]}" -eq 1 ] && [ "$INTERACTIVE" = 0 ]; then
		ENV="${cands[0]}"
	elif [ "$INTERACTIVE" = 0 ]; then
		die "the nodes run different platforms (${cands[*]}); choose one with --env"
	else
		best="${cands[0]}"
		printf '\nWhich platform should Haify plug into?\n'
		i=1
		for e in "${ENVS[@]}"; do
			counts=()
			for n in "${NODES[@]}"; do in_list "$e" ${NODE_ENVS[$n]} && counts+=("$n"); done
			printf '  %2d) %s%s\n' "$i" "$(env_title "$e")" "$([ "${#counts[@]}" -gt 0 ] && echo "   [found on ${counts[*]}]")"
			i=$((i + 1))
		done
		printf 'Choice [%s]: ' "$best"
		read -r choice
		case "$choice" in
			"") ENV="$best" ;;
			*[!0-9]*) in_list "$choice" "${ENVS[@]}" || die "no platform '$choice'"; ENV="$choice" ;;
			*) [ "$choice" -ge 1 ] && [ "$choice" -le "${#ENVS[@]}" ] || die "no choice $choice"; ENV="${ENVS[$((choice - 1))]}" ;;
		esac
	fi
	case "$ENV" in
		proxmox) die "Proxmox VE is installed from one of its nodes: run this there (it hands over to deploy/proxmox/bootstrap.sh)" ;;
		harvester) die "Harvester's nodes are immutable: DRBD is built in a Pod and the CSI chart installed from kubectl. Follow docs/harvester.md" ;;
	esac
	log "platform: $(env_title "$ENV")"
}

# blank_disks <node>: whole disks with nothing on them, as "<path> <size>".
blank_disks() {
	local node="$1" d
	while read -r d size; do
		[ "$(device_verdict "$node" "$d" < /dev/null)" = clean ] && printf '%s %s\n' "$d" "$size"
	done < <(check_on "$node" "lsblk -dpno NAME,SIZE,TYPE | awk '\$3 == \"disk\" && \$1 !~ /^\/dev\/(drbd|zram|nbd|rbd|loop)/ { print \$1, \$2 }'" || true)
}

# ask_devices fills in the disks the options left out, offering the blank
# ones the node has.
ask_devices() {
	local n list def answer
	for n in "${STORAGE_NODES[@]}"; do
		[ -n "${NODE_DEVICES[$n]:-$DEVICES}" ] && continue
		pool_vg_present "$n" && continue
		[ "$INTERACTIVE" = 1 ] || die "--devices is required (no disk given for $n). The installer never picks a disk itself."
		list=$(blank_disks "$n")
		[ -n "$list" ] || die "$n has no blank disk for the pool; free one, or name a disk to wipe with --node-devices $n=DEV --force-wipe"
		printf '\nBlank disks on %s:\n%s\n' "$n" "$(printf '%s\n' "$list" | sed 's/^/  /')"
		def=""
		[ "$(printf '%s\n' "$list" | wc -l)" -eq 1 ] && def="${list%% *}"
		printf 'Disk(s) for the pool on %s, comma-separated [%s]: ' "$n" "$def"
		read -r answer
		answer="${answer:-$def}"
		[ -n "$answer" ] || die "no disk for $n"
		NODE_DEVICES[$n]="$answer"
	done
}

# bin_arch: the CPU the binaries to install were built for, from the ELF header.
bin_arch() {
	case "$(od -An -tx1 -j18 -N2 "$BIN_DIR/haify-controller" 2>/dev/null | tr -d ' \n')" in
		3e00) echo x86_64 ;;
		b700) echo aarch64 ;;
		*) echo unknown ;;
	esac
}

make_plan() {
	local n arch f
	if [ -n "$STORAGE_NODES_ARG" ]; then
		split_csv "$STORAGE_NODES_ARG" STORAGE_NODES
		for n in "${STORAGE_NODES[@]}"; do in_list "$n" "${NODES[@]}" || die "--storage-nodes: '$n' is not one of the nodes (${NODES[*]})"; done
	else
		STORAGE_NODES=("${NODES[@]}")
	fi
	[ -n "$REPLICAS" ] || { REPLICAS=2; [ "${#STORAGE_NODES[@]}" -lt 2 ] && REPLICAS=1; }
	[[ "$REPLICAS" =~ ^[0-9]+$ ]] && [ "$REPLICAS" -ge 1 ] && [ "$REPLICAS" -le "${#STORAGE_NODES[@]}" ] ||
		die "--replicas $REPLICAS needs that many storage nodes; there are ${#STORAGE_NODES[@]}"
	if [ -n "$LOCAL_NODE" ] && in_list "$LOCAL_NODE" "${STORAGE_NODES[@]}"; then
		CONTROLLER_NODE="$LOCAL_NODE"
	else
		CONTROLLER_NODE="${STORAGE_NODES[0]}"
	fi
	SELF_HA=0
	CONTROLLER_NODES=("$CONTROLLER_NODE")
	if [ "${#STORAGE_NODES[@]}" -ge 2 ] && [ "$NO_SELF_HA" = 0 ]; then
		SELF_HA=1
		CONTROLLER_NODES=("${STORAGE_NODES[@]}")
		if [ -z "$VIP" ] && [ "$INTERACTIVE" = 1 ]; then
			printf '\nThe controller moves between the storage nodes behind a floating address.\n'
			printf 'A free address in their subnet, in CIDR form (e.g. 192.168.1.250/24), or "none" for no Self-HA: '
			read -r VIP
			[ "$VIP" = none ] && { VIP=""; SELF_HA=0; CONTROLLER_NODES=("$CONTROLLER_NODE"); }
		fi
		if [ "$SELF_HA" = 1 ]; then
			[ -n "$VIP" ] || die "--vip is required with ${#STORAGE_NODES[@]} storage nodes (or --no-self-ha)"
			[[ "$VIP" =~ ^[0-9a-fA-F.:]+/[0-9]+$ ]] || die "--vip must be in CIDR form, e.g. 192.168.1.250/24"
		fi
	fi
	[ "$FROM_STEP" -le 5 ] && ask_devices
	if [ "$FROM_STEP" -le 4 ]; then
		check_artifacts
		arch=$(bin_arch)
		for n in "${CONTROLLER_NODES[@]}"; do
			[ "${NODE_ARCH[$n]}" = "$arch" ] || die "$n is ${NODE_ARCH[$n]}, the binaries in $BIN_DIR are $arch"
		done
	fi
	case "$ENV" in
		docker) f=haify-docker ;;
		opennebula) f=haify-opennebula ;;
		*) f="" ;;
	esac
	[ -z "$f" ] || [ -x "$BIN_DIR/$f" ] || die "$BIN_DIR/$f not found; it is part of the release bundle, or 'make build' builds it"
	[ "$FROM_STEP" -le 5 ] && verify_devices
	return 0
}

print_plan() {
	local n
	printf '\nPlan:\n'
	printf '  platform:          %s\n' "$(env_title "$ENV")"
	printf '  nodes:             %s\n' "${NODES[*]}"
	printf '  storage nodes:     %s\n' "${STORAGE_NODES[*]}"
	for n in "${STORAGE_NODES[@]}"; do
		printf '    %-15s  %s\n' "$n" "${NODE_DEVICES[$n]:-${DEVICES:-(pool exists)}}"
	done
	printf '  pool:              %s (%s), %s replica(s) per volume\n' "$POOL" "$POOL_TYPE" "$REPLICAS"
	printf '  controller:        starts on %s; may run on %s\n' "$CONTROLLER_NODE" "${CONTROLLER_NODES[*]}"
	printf '  Self-HA:           %s\n' "$([ "$SELF_HA" = 1 ] && echo "on, VIP $VIP" || echo off)"
	printf '  steps:             %s-7\n' "$FROM_STEP"
}

confirm_plan() {
	local answer
	if [ "$DRY_RUN" = 1 ]; then
		printf '\nDry run: the read-only checks run; each change is printed, with its node, instead of made.\n'
		return 0
	fi
	[ "$ASSUME_YES" = 1 ] && return 0
	[ -t 0 ] || die "not on a terminal; pass --yes to proceed without confirmation"
	printf '\nThis installs packages and changes storage on the nodes above. Proceed? [y/N] '
	read -r answer
	case "$answer" in y | Y | yes | YES) ;; *) echo "aborted"; exit 1 ;; esac
}

# step_ssh_and_dispatch: every node that can run the controller gets a root
# key that every node accepts, then the dispatch config that uses it. PVE
# keeps this trust itself; elsewhere nothing does.
step_ssh_and_dispatch() {
	step 3 "controller SSH access to every node, and the dispatch config"
	local c n pub key file want have
	for c in "${CONTROLLER_NODES[@]}"; do
		if ! check_on "$c" "test -f /root/.ssh/id_ed25519 || test -f /root/.ssh/id_rsa" >/dev/null; then
			run_on "$c" "mkdir -p -m 700 /root/.ssh && ssh-keygen -q -t ed25519 -N '' -f /root/.ssh/id_ed25519"
		fi
		[ "$DRY_RUN" = 1 ] && continue
		pub=$(check_on "$c" "cat /root/.ssh/id_ed25519.pub 2>/dev/null || cat /root/.ssh/id_rsa.pub")
		for n in "${NODES[@]}"; do
			if ! check_on "$n" "grep -qxF $(q "$pub") /root/.ssh/authorized_keys 2>/dev/null" >/dev/null; then
				run_on "$n" "mkdir -p -m 700 /root/.ssh && printf '%s\\n' $(q "$pub") >> /root/.ssh/authorized_keys && chmod 600 /root/.ssh/authorized_keys"
			fi
		done
	done
	# A dry run made no key above; the config shows the one it would make.
	key=$(dispatch_key_path 2>/dev/null) || {
		[ "$DRY_RUN" = 1 ] || die "$CONTROLLER_NODE has no root SSH key after this step made one"
		key=/root/.ssh/id_ed25519
	}
	file="$WORK_DIR/dispatch-config.toml"
	write_dispatch_config "$file" "$key"
	want=$(local_sha256 "$file")
	for n in "${CONTROLLER_NODES[@]}"; do
		have=$(remote_sha256 "$n" "$DISPATCH_CONFIG")
		if [ "$have" = "$want" ]; then
			note "$n: $DISPATCH_CONFIG up to date"
		elif [ -n "$have" ]; then
			warn "$n: $DISPATCH_CONFIG exists and differs from what the installer would write; keeping it"
		else
			run_on "$n" "mkdir -p /root/.dispatch && chmod 700 /root/.dispatch"
			copy_to "$n" "$file" "$DISPATCH_CONFIG" 0600
		fi
	done
}

print_done() {
	printf '\nDone.'
	if [ "$DRY_RUN" = 1 ]; then
		printf ' (dry run: nothing was changed)\n'
		return 0
	fi
	printf '\n  web UI:  http://%s:3376/\n' "$(controller_ip)"
	printf '  CLI:     haify -c %s:3374 resource list   (on any node)\n' "$(controller_ip)"
}
