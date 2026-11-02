# shellcheck shell=bash
# shellcheck disable=SC2153 # CONTROLLER_NODES and the other globals live in lib.sh
# Step 5 (start the controller, register the nodes, create the pool) and
# step 6 (Self-HA).

REST_PORT=${HAIFY_REST_PORT:-3375}

# haify_get <path>: GET from the controller's REST API on the controller node.
# Empty output when the controller does not answer (yet).
haify_get() {
	check_on "$CONTROLLER_NODE" "curl -sf -m 10 http://127.0.0.1:$REST_PORT$1" 2>/dev/null || true
}

# registered_nodes prints "<name>\t<address>" per haify node.
registered_nodes() {
	# shellcheck disable=SC2016 # Perl, not shell
	haify_get /v1/nodes | json_query '
		printf "%s\t%s\n", $_->{name} // "", $_->{address} // "" for @{ $j->{nodes} || [] };
	' || true
}

# pools prints "<name>\t<node>" per pool and node.
pools() {
	# shellcheck disable=SC2016 # Perl, not shell
	haify_get /v1/pools | json_query '
		printf "%s\t%s\n", $_->{name} // "", $_->{node} // "" for @{ $j->{pools} || [] };
	' || true
}

# running_controllers prints every node with an active haify-controller.
running_controllers() {
	local n
	for n in "${NODES[@]}"; do
		if check_on "$n" "systemctl is-active --quiet haify-controller" >/dev/null 2>&1; then
			printf '%s\n' "$n"
		fi
	done
}

# self_ha_present: the Self-HA metadata resource exists on some node, i.e.
# drbd-reactor, not systemd, decides where the controller runs.
self_ha_present() {
	local n
	for n in "${CONTROLLER_NODES[@]}"; do
		if check_on "$n" "drbdadm role haify-meta" >/dev/null 2>&1; then
			return 0
		fi
	done
	return 1
}

# locate_controller points CONTROLLER_NODE at the node that runs the
# controller now. Under Self-HA that is wherever drbd-reactor put it, which
# need not be where bootstrap started it. Prints nothing; returns 1 when no
# controller runs anywhere.
locate_controller() {
	local running=()
	mapfile -t running < <(running_controllers)
	case "${#running[@]}" in
		0) return 1 ;;
		1) CONTROLLER_NODE="${running[0]}" ;;
		*) die "haify-controller is running on several nodes (${running[*]}), each with its own database. Stop all but one." ;;
	esac
	return 0
}

start_controller() {
	if locate_controller; then
		log "controller already running on $CONTROLLER_NODE"
		return 0
	fi
	if self_ha_present; then
		die "Self-HA is configured (haify-meta exists) but no controller is running. drbd-reactor starts it;
do not start it by hand. Check on each node: drbd-reactorctl status haify-meta; journalctl -u haify-controller"
	fi
	run_on "$CONTROLLER_NODE" "systemctl enable --now haify-controller"
	# The REST gateway comes up a moment after the unit is active.
	run_on "$CONTROLLER_NODE" "for i in \$(seq 1 30); do haify node list >/dev/null 2>&1 && exit 0; sleep 2; done; echo 'haify-controller did not answer within 60s: journalctl -u haify-controller' >&2; exit 1"
}

register_nodes() {
	local reg addr n regname
	reg=$(registered_nodes)
	for n in "${NODES[@]}"; do
		addr=$(printf '%s\n' "$reg" | awk -F'\t' -v n="$n" '$1 == n { print $2 }')
		regname=$(printf '%s\n' "$reg" | awk -F'\t' -v a="${NODE_IP[$n]}" '$2 == a { print $1 }' | head -1)
		if [ -n "$addr" ]; then
			note "$n: registered ($addr)"
			[ "$addr" = "${NODE_IP[$n]}" ] ||
				warn "$n is registered with $addr, PVE has ${NODE_IP[$n]}; left as is (change it with: haify node set-address $n <ip>)"
		elif [ -n "$regname" ]; then
			# Platforms attach and promote a disk by their own name for the
			# host, so the same machine under another haify name is unusable.
			die "${NODE_IP[$n]} is registered in haify as '$regname', but the cluster calls it '$n'; haify node names must equal the cluster's.
Unregister it (haify node unregister $regname, once nothing uses it) and rerun with --from-step 5."
		else
			run_on "$CONTROLLER_NODE" "haify node register --name $(q "$n") --address $(q "${NODE_IP[$n]}")"
		fi
	done
}

# The device probe runs on the node and prints one verdict. It never changes
# anything: a disk is wiped only after the verdict, and only with
# --force-wipe.
# shellcheck disable=SC2016 # expanded by the node's shell, not here
DEVICE_PROBE='
r=$(readlink -f -- "$d")
if [ ! -b "$r" ]; then echo missing; exit 0; fi
n=${r##*/}
for h in /sys/class/block/"$n"/holders/* /sys/class/block/"$n"/"$n"*/holders/*; do
	if [ -e "$h" ]; then echo "in-use: held by ${h##*/}"; exit 0; fi
done
m=$(lsblk -nro MOUNTPOINT "$r" | grep -v "^$" | head -1)
if [ -n "$m" ]; then echo "in-use: mounted at $m"; exit 0; fi
p=$(lsblk -nro NAME,TYPE "$r" | awk "\$2 == \"part\" { print \$1 }" | tr "\n" " ")
if [ -n "$p" ]; then echo "wipeable: has partitions $p"; exit 0; fi
s=$(wipefs -n -i -O TYPE "$r" 2>/dev/null | tr "\n" " ")
if [ -n "$s" ]; then echo "wipeable: carries $s"; exit 0; fi
echo clean'

# shellcheck disable=SC2016 # expanded by the node's shell, not here
DEVICE_WIPE='
r=$(readlink -f -- "$d")
for p in $(lsblk -nrpo NAME,TYPE "$r" | awk "\$2 == \"part\" { print \$1 }"); do wipefs -a "$p"; done
wipefs -a "$r"
blockdev --rereadpt "$r" 2>/dev/null || true
udevadm settle'

device_verdict() {
	check_on "$1" "d=$(q "$2"); $DEVICE_PROBE" || echo unreadable
}

# device_problem <node> <device> <verdict>: prints why the disk cannot take
# the pool, nothing when it can. A disk in use (held by LVM/dm/md, or mounted) is
# refused even with --force-wipe: that is the node's own storage, never the
# pool's.
device_problem() {
	local node="$1" dev="$2" verdict="$3"
	case "$verdict" in
		clean) ;;
		missing) echo "$node: $dev is not a block device" ;;
		in-use:*) echo "$node: $dev is ${verdict#in-use: }; never used for the pool, not even with --force-wipe" ;;
		wipeable:*)
			[ "$FORCE_WIPE" = 1 ] ||
				echo "$node: $dev ${verdict#wipeable: }; check it holds nothing you need, then add --force-wipe"
			;;
		*) echo "$node: could not inspect $dev ($verdict)" ;;
	esac
}

# pool_vg_present <node>: the pool's volume group is already on the node, so
# its disks rightly carry LVM signatures and are not checked again.
pool_vg_present() {
	check_on "$1" "vgs $(q "haify_${POOL#haify_}")" >/dev/null 2>&1
}

# verify_devices runs before step 2: a disk that cannot be used stops the
# bootstrap before it has installed or changed anything.
verify_devices() {
	local n dev devs=() problems="" p
	for n in "${STORAGE_NODES[@]}"; do
		if pool_vg_present "$n"; then
			continue
		fi
		split_csv "${NODE_DEVICES[$n]:-$DEVICES}" devs
		for dev in "${devs[@]}"; do
			p=$(device_problem "$n" "$dev" "$(device_verdict "$n" "$dev")")
			[ -z "$p" ] || problems+="  $p"$'\n'
		done
	done
	[ -z "$problems" ] || die "refusing to create the pool on these disks:
$problems"
	return 0
}

# prepare_devices <node> <device...>: check again right before the pool is
# created (the node may have changed since the start), and wipe what
# --force-wipe allows.
prepare_devices() {
	local node="$1" dev p verdict
	shift
	for dev in "$@"; do
		verdict=$(device_verdict "$node" "$dev")
		p=$(device_problem "$node" "$dev" "$verdict")
		[ -z "$p" ] || die "$p"
		if [ "$verdict" != clean ]; then
			warn "$node: $dev ${verdict#wipeable: }; wiping it (--force-wipe)"
			run_on "$node" "d=$(q "$dev"); $DEVICE_WIPE"
		else
			note "$node: $dev is blank"
		fi
	done
}

# pool_on <pools output> <node>: true when the bootstrap pool exists there.
# The controller stores LVM pools with an haify_ prefix (vg0 -> haify_vg0), and
# its pool list names a node by address, not by name.
pool_on() {
	printf '%s\n' "$1" | awk -F'\t' -v a="$POOL" -v b="haify_${POOL#haify_}" -v n="$2" -v ip="$(node_ip "$2")" \
		'($1 == a || $1 == b) && ($2 == n || $2 == ip) { found = 1 } END { exit !found }'
}

create_pools() {
	local have n devs=() created=0
	have=$(pools)
	for n in "${STORAGE_NODES[@]}"; do
		if pool_on "$have" "$n"; then
			note "$n: pool $POOL exists"
			continue
		fi
		split_csv "${NODE_DEVICES[$n]:-$DEVICES}" devs
		prepare_devices "$n" "${devs[@]}"
		run_on "$CONTROLLER_NODE" "haify pool create --name $(q "$POOL") --type $(q "$POOL_TYPE") --nodes $(q "$n") --devices $(q "$(join_by , "${devs[@]}")")"
		created=1
	done
	[ "$created" = 1 ] && [ "$DRY_RUN" = 0 ] || return 0
	# `haify pool create` reports a per-node failure on stderr and still exits 0
	# when another node succeeded, so the result is read back, not assumed.
	have=$(pools)
	for n in "${STORAGE_NODES[@]}"; do
		pool_on "$have" "$n" || die "pool $POOL was not created on $n; see the output above and journalctl -u haify-controller on $CONTROLLER_NODE"
	done
}

step_register_and_pool() {
	step 5 "start the controller on $CONTROLLER_NODE, register nodes, create pool $POOL"
	start_controller
	register_nodes
	create_pools
}

# ---------------------------------------------------------------------------
# Step 6: Self-HA. The controller's database moves onto a DRBD resource
# (haify-meta) and drbd-reactor runs the controller on one storage node at a
# time behind the VIP, so losing the controller node loses no control plane.
# ---------------------------------------------------------------------------

step_self_ha() {
	step 6 "controller Self-HA"
	if [ "$SELF_HA" != 1 ]; then
		if [ "${#STORAGE_NODES[@]}" -lt 2 ]; then
			note "skipped: one storage node; Self-HA needs at least two. The controller on $CONTROLLER_NODE is a"
			note "single point of failure for creating, starting and migrating guests (running guests keep running)."
		else
			note "skipped (--no-self-ha): the controller on $CONTROLLER_NODE is a single point of failure"
		fi
		return 0
	fi
	local status vip_addr
	status=$(check_on "$CONTROLLER_NODE" "haify ha self status" 2>/dev/null || true)
	if printf '%s\n' "$status" | grep -q 'self-HA: enabled'; then
		note "already enabled; active controller on $CONTROLLER_NODE"
		return 0
	fi
	vip_addr="$(rest_addr "${VIP%/*}"):3374"
	run_on "$CONTROLLER_NODE" "haify ha self enable --vip $(q "$VIP") --pool $(q "$POOL") --nodes $(q "$(join_by , "${STORAGE_NODES[@]}")")"
	# enable returns once the handoff has started; the controller then
	# restarts under drbd-reactor. Wait until it answers on the VIP again.
	run_on "$CONTROLLER_NODE" "for i in \$(seq 1 60); do haify -c $(q "$vip_addr") ha self status 2>/dev/null | grep 'Active node:' | grep -qv unknown && exit 0; sleep 3; done; echo 'the controller did not come back on the VIP within 3 minutes: see /var/log/haify/selfha-handoff.log on $(q "$CONTROLLER_NODE") and journalctl -u haify-controller' >&2; exit 1"
	if [ "$DRY_RUN" = 0 ]; then
		locate_controller || die "Self-HA reports an active node, but no node runs haify-controller"
		log "Self-HA enabled; controller active on $CONTROLLER_NODE, VIP $VIP"
	fi
}
