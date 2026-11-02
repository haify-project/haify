#!/bin/bash
# Install Haify on a set of nodes and connect it to the platform they run.
#
# Run as root on one of the nodes (or on any machine with root SSH to them).
# It finds out what the nodes are (Proxmox VE, XCP-ng, Kubernetes, OpenStack,
# OpenNebula, Nomad, KVM/libvirt, Docker/Podman, or none of these), asks when
# it cannot tell or --env says otherwise, and then, step by step:
#
#   1  finds the nodes and checks root SSH to each
#   2  installs DRBD 9, drbd-utils, drbd-reactor and the storage tools
#   3  gives the controller SSH access to every node (dispatch config)
#   4  installs haify-controller and controller.toml
#   5  starts the controller, registers the nodes, creates the pool
#   6  enables controller Self-HA (two or more storage nodes)
#   7  connects the platform (driver, plugin, hook, or the files to apply)
#
# On Proxmox VE the whole run is deploy/proxmox/bootstrap.sh, which this
# hands over to. Every step checks before it changes anything, so a rerun
# changes nothing, and --dry-run prints every change instead of making it.

set -euo pipefail
shopt -s inherit_errexit

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DEPLOY_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
ROOT_DIR="$(cd "$DEPLOY_DIR/.." && pwd)"
BOOT_DIR="$DEPLOY_DIR/proxmox/bootstrap"

# shellcheck source=../proxmox/bootstrap/lib.sh
. "$BOOT_DIR/lib.sh"
# shellcheck source=../proxmox/bootstrap/drbd.sh
. "$BOOT_DIR/drbd.sh"
# shellcheck source=../proxmox/bootstrap/controller.sh
. "$BOOT_DIR/controller.sh"
# shellcheck source=../proxmox/bootstrap/storage.sh
. "$BOOT_DIR/storage.sh"
# shellcheck source=lib/detect.sh
. "$SCRIPT_DIR/lib/detect.sh"
# shellcheck source=lib/prereqs.sh
. "$SCRIPT_DIR/lib/prereqs.sh"
# shellcheck source=lib/integrations.sh
. "$SCRIPT_DIR/lib/integrations.sh"
# shellcheck source=lib/plan.sh
. "$SCRIPT_DIR/lib/plan.sh"
# shellcheck source=lib/probe.sh
. "$SCRIPT_DIR/lib/probe.sh"

usage() {
	cat <<EOF
Usage: $0 [options]

Installs Haify on the nodes and connects it to their platform. Run as root.
Asks for what it needs on a terminal; with --yes every answer comes from the options.

Platform:
  --env ENV               One of: ${ENVS[*]}
                          (default: what the nodes run; asked when they run several)
  --list-envs             Print what each node runs and exit
  --probe-json            Print the nodes, what they run and their disks as JSON, and exit
                          (for the web wizard, haify-installer)

Nodes:
  --nodes N=IP[,N=IP...]  The nodes, by name and address. Default: the platform's own
                          member list (XCP-ng pool, Kubernetes, OpenNebula, Nomad), or
                          this machine alone. A name must equal the platform's name for
                          the host. IP alone takes the node's hostname as its name.
  --storage-nodes N,N     Nodes that get the pool (default: all); the others use
                          volumes as diskless DRBD clients

Storage:
  --devices DEVS          Blank disk(s) for the pool on every storage node, comma-separated
  --node-devices N=DEVS   Different disk(s) on node N (repeatable)
  --pool NAME             Pool name (default: vg0)
  --pool-type TYPE        lvm-thin (default; snapshots) or lvm
  --replicas N            Copies of each volume the platform creates (default: 2, 1 with one node)
  --force-wipe            Wipe partition tables and signatures from the devices first

Controller:
  --vip CIDR              Floating address for the controller under Self-HA, e.g.
                          192.168.1.250/24 (required with two or more storage nodes)
  --no-self-ha            Run the controller on one node only

Run control:
  --dry-run               Run only read-only checks; print every change instead of making it
  --yes                   Do not ask anything
  --from-step N           Resume at step N (1-7) after fixing a failed step
  --verbose               Also print the read-only checks
  -h, --help              This text

Environment:
  HAIFY_BIN_DIR           Linux binaries (default: $ROOT_DIR/bin)
  HAIFY_CONFIG_DIR        Unit files and controller.toml.example (default: $ROOT_DIR/configs)
  HAIFY_OUT_DIR           Where files for the platform to apply go (default: ./haify-install)
  HAIFY_CSI_IMAGE         registry/haify-csi:tag; with helm and kubectl here, installs the CSI chart
EOF
}

ORIG_ARGS=("$@")
ENV=""
NODES_ARG=""
STORAGE_NODES_ARG=""
DEVICES=""
declare -A NODE_DEVICES=()
POOL=vg0
POOL_TYPE=lvm-thin
FORCE_WIPE=0
VIP=""
NO_SELF_HA=0
REPLICAS=""
ASSUME_YES=0
FROM_STEP=1
LIST_ENVS=0
PROBE_JSON=0
PASS_THROUGH=()

need_arg() { [ $# -ge 2 ] && [ -n "$2" ] || die "$1 needs a value (see --help)"; }

while [ $# -gt 0 ]; do
	case "$1" in
		--env) need_arg "$@"; ENV="$2"; shift 2; continue ;;
		--nodes) need_arg "$@"; NODES_ARG="$2"; shift 2; continue ;;
		--list-envs) LIST_ENVS=1; shift; continue ;;
		--probe-json) PROBE_JSON=1; shift; continue ;;
		--storage-nodes) need_arg "$@"; STORAGE_NODES_ARG="$2" ;;
		--devices) need_arg "$@"; DEVICES="$2" ;;
		--node-devices)
			need_arg "$@"
			case "$2" in ?*=?*) NODE_DEVICES["${2%%=*}"]="${2#*=}" ;; *) die "--node-devices takes NODE=DEV[,DEV...]" ;; esac
			;;
		--pool) need_arg "$@"; POOL="$2" ;;
		--pool-type) need_arg "$@"; POOL_TYPE="$2" ;;
		--replicas) need_arg "$@"; REPLICAS="$2" ;;
		--vip) need_arg "$@"; VIP="$2" ;;
		--from-step) need_arg "$@"; FROM_STEP="$2" ;;
		--force-wipe) FORCE_WIPE=1; PASS_THROUGH+=("$1"); shift; continue ;;
		--no-self-ha) NO_SELF_HA=1; PASS_THROUGH+=("$1"); shift; continue ;;
		--dry-run) DRY_RUN=1; PASS_THROUGH+=("$1"); shift; continue ;;
		--yes | -y) ASSUME_YES=1; PASS_THROUGH+=("$1"); shift; continue ;;
		--verbose | -v) VERBOSE=1; PASS_THROUGH+=("$1"); shift; continue ;;
		-h | --help) usage; exit 0 ;;
		*) die "unknown argument '$1' (see --help)" ;;
	esac
	# The options Proxmox's bootstrap shares, passed on as given.
	PASS_THROUGH+=("$1" "$2")
	shift 2
done

[ -z "$ENV" ] || in_list "$ENV" "${ENVS[@]}" || die "--env '$ENV': use one of ${ENVS[*]}"
case "$POOL_TYPE" in lvm-thin | lvm) ;; *) die "--pool-type must be lvm-thin or lvm" ;; esac
[[ "$POOL" =~ ^[A-Za-z0-9][A-Za-z0-9_.-]*$ ]] || die "--pool '$POOL': use letters, digits, '_', '.', '-'"
if [ "$DRY_RUN" = 0 ] && [ "$(id -u)" -ne 0 ]; then
	die "run as root (or try --dry-run)"
fi
# The controller's REST answers are read here; JSON::PP is part of perl.
perl -MJSON::PP -e1 2>/dev/null || die "perl with JSON::PP is needed on this machine (Debian/Ubuntu: perl; EL: dnf install perl-JSON-PP)"

BIN_DIR=${HAIFY_BIN_DIR:-$ROOT_DIR/bin}
CONFIG_DIR=${HAIFY_CONFIG_DIR:-$ROOT_DIR/configs}
OUT_DIR=${HAIFY_OUT_DIR:-$PWD/haify-install}
CONTROLLER_DEB=""
INTERACTIVE=0
[ "$ASSUME_YES" = 0 ] && [ -t 0 ] && INTERACTIVE=1

WORK_DIR=$(mktemp -d "${TMPDIR:-/tmp}/haify-install.XXXXXX")
CURRENT_STEP=0
on_exit() {
	local rc=$?
	ssh_close_masters
	rm -rf "$WORK_DIR"
	if [ "$rc" -ne 0 ] && [ "$CURRENT_STEP" -gt 0 ]; then
		printf '\nStep %s failed. Fix the cause above, then resume with:\n  %s' "$CURRENT_STEP" "$0" >&2
		local a skip=0
		for a in "${ORIG_ARGS[@]}"; do
			if [ "$skip" = 1 ]; then skip=0; continue; fi
			case "$a" in --from-step) skip=1; continue ;; esac
			printf ' %q' "$a" >&2
		done
		printf ' --from-step %s\n' "$CURRENT_STEP" >&2
	fi
	exit "$rc"
}
trap on_exit EXIT

# ---------------------------------------------------------------------------
# Step 1: the nodes, what they run, and root SSH to each
# ---------------------------------------------------------------------------

detect_local
if [ "$PROBE_JSON" = 1 ]; then
	# Only the JSON goes to stdout; the progress lines go with the errors.
	exec 3>&1 1>&2
	find_nodes
	probe_json >&3
	exit 0
fi
if [ "$LOCAL_ENV" = proxmox ] && { [ -z "$ENV" ] || [ "$ENV" = proxmox ]; }; then
	# The PVE bootstrap reads the cluster from pmxcfs and does everything
	# from here on, including the storage plugin and storage.cfg.
	log "this is a Proxmox VE node: handing over to deploy/proxmox/bootstrap.sh"
	[ "$LIST_ENVS" = 1 ] && { echo "$(hostname): proxmox"; exit 0; }
	CURRENT_STEP=0
	HAIFY_BIN_DIR="$BIN_DIR" HAIFY_CONFIG_DIR="$CONFIG_DIR" exec "$DEPLOY_DIR/proxmox/bootstrap.sh" "${PASS_THROUGH[@]}"
fi
case "$FROM_STEP" in [1-7]) ;; *) die "--from-step takes 1-7" ;; esac
CURRENT_STEP=1
step 1 "nodes, platform and root SSH"
find_nodes
for n in "${NODES[@]}"; do
	ensure_root_ssh "$n"
	detect_node "$n"
	note "$n  ${NODE_IP[$n]}  ${NODE_OS[$n]} ${NODE_ARCH[$n]}  runs: ${NODE_ENVS[$n]:-nothing Haify plugs into}$([ "$n" = "$LOCAL_NODE" ] && echo '  (this machine)')"
done
if [ "$LIST_ENVS" = 1 ]; then
	CURRENT_STEP=0
	exit 0
fi
choose_env
CURRENT_STEP=0
make_plan
print_plan
confirm_plan

run_step() {
	local n="$1"
	shift
	[ "$FROM_STEP" -le "$n" ] || return 0
	CURRENT_STEP="$n"
	"$@"
}

if [ "$FROM_STEP" -gt 5 ] && ! locate_controller && [ "$DRY_RUN" = 0 ]; then
	CURRENT_STEP=5
	die "no haify-controller is running on any node"
fi

run_step 2 step_prereqs
run_step 3 step_ssh_and_dispatch
run_step 4 step_controller_install
run_step 5 step_register_and_pool
run_step 6 step_self_ha
run_step 7 step_integration

CURRENT_STEP=0
print_done
