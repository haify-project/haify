# shellcheck shell=bash
# What a node is: its operating system and the platform Haify plugs into on
# it. Every probe is read-only and runs through check_on, so a dry run sees
# the nodes as they are.

# The platforms the installer knows, in the order a node that has several is
# classified: a Proxmox host also runs libvirt-free KVM, an OpenStack compute
# node also runs libvirt, a Kubernetes node often has Docker.
ENVS=(proxmox xcp-ng harvester kubernetes openstack opennebula nomad libvirt docker plain)

env_title() {
	case "$1" in
		proxmox) echo "Proxmox VE" ;;
		xcp-ng) echo "XCP-ng" ;;
		harvester) echo "Harvester" ;;
		kubernetes) echo "Kubernetes (also k3s, RKE2, MicroShift, OpenShift)" ;;
		openstack) echo "OpenStack (Cinder)" ;;
		opennebula) echo "OpenNebula" ;;
		nomad) echo "Nomad" ;;
		libvirt) echo "KVM / libvirt" ;;
		docker) echo "Docker / Podman" ;;
		plain) echo "Linux, no platform (block devices, NFS/iSCSI/NVMe-oF/SMB gateways, HA services)" ;;
		*) echo "$1" ;;
	esac
}

# The probe prints one word per platform found, then "os <id> <version>
# <arch>". It is one ssh round trip per node.
# shellcheck disable=SC2016 # expanded by the node's shell, not here
DETECT_PROBE='
have() { command -v "$1" >/dev/null 2>&1; }
active() { systemctl is-active --quiet "$1" 2>/dev/null; }
[ -d /etc/pve ] && have pveversion && echo proxmox
[ -f /etc/xensource-inventory ] && echo xcp-ng
[ -f /etc/harvester-release.yaml ] && echo harvester
if have k3s || have rke2 || have microshift || [ -f /etc/kubernetes/kubelet.conf ] || active kubelet; then echo kubernetes; fi
if [ -f /etc/nova/nova.conf ] || [ -f /etc/cinder/cinder.conf ] || active devstack@n-cpu; then echo openstack; fi
if [ -f /etc/one/oned.conf ]; then echo opennebula; echo opennebula-frontend; fi
if [ -d /var/lib/one ] && id oneadmin >/dev/null 2>&1; then echo opennebula; fi
if have nomad && active nomad; then echo nomad; fi
# libvirtd and virtqemud are socket-activated and exit when idle, so an
# idle KVM host shows only its enabled socket.
enabled() { systemctl is-enabled --quiet "$1" 2>/dev/null; }
if active libvirtd || active virtqemud || enabled libvirtd.socket || enabled virtqemud.socket; then echo libvirt; fi
if have docker || have podman; then echo docker; fi
. /etc/os-release 2>/dev/null
echo "os ${ID:-unknown} ${VERSION_ID:-0} $(uname -m)"'

# detect_node <node>: fill NODE_ENVS[node] (space-separated, in ENVS order),
# NODE_OS[node] ("<id> <version>") and NODE_ARCH[node].
declare -gA NODE_ENVS=() NODE_OS=() NODE_ARCH=() NODE_FRONTEND=()
detect_node() {
	local node="$1" out found="" e line
	out=$(check_on "$node" "$DETECT_PROBE") || out=""
	for e in "${ENVS[@]}"; do
		printf '%s\n' "$out" | grep -qx "$e" && found+="$e "
	done
	printf '%s\n' "$out" | grep -qx opennebula-frontend && NODE_FRONTEND[$node]=1
	NODE_ENVS[$node]="${found% }"
	line=$(printf '%s\n' "$out" | awk '$1 == "os" { print $2, $3, $4 }')
	NODE_OS[$node]="${line% *}"
	NODE_ARCH[$node]="${line##* }"
	return 0
}

# primary_env <envs...>: the first in ENVS order, "plain" for none.
primary_env() {
	local e
	for e in "${ENVS[@]}"; do
		in_list "$e" "$@" && { echo "$e"; return 0; }
	done
	echo plain
}

# os_family <node>: debian, ubuntu, el, xcp-ng or unknown.
os_family() {
	local id="${NODE_OS[$1]%% *}"
	case "$id" in
		ubuntu) echo ubuntu ;;
		debian) echo debian ;;
		rocky | almalinux | rhel | centos | ol | fedora) echo el ;;
		xcp-ng | xenenterprise) echo xcp-ng ;;
		*) echo unknown ;;
	esac
}

# ---------------------------------------------------------------------------
# Membership: the hosts a platform already knows about, as "<name>\t<ip>"
# lines. Read on this node; empty when the platform keeps no such list here
# (then --nodes names them).
# ---------------------------------------------------------------------------

members_of() {
	case "$1" in
		xcp-ng)
			# xe prints "name-label ( RO): x" blocks; the pool master knows every host.
			xe host-list params=name-label,address 2>/dev/null | awk -F': ' '
				/name-label/ { n = $2 } /address/ { a = $2 }
				n != "" && a != "" { print n "\t" a; n = ""; a = "" }'
			;;
		kubernetes)
			kubectl get nodes -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.status.addresses[?(@.type=="InternalIP")].address}{"\n"}{end}' 2>/dev/null
			;;
		opennebula)
			# KVM hosts as OpenNebula names them; the frontend is added by the caller.
			sudo -u oneadmin onehost list --csv -l NAME 2>/dev/null | tail -n +2 | while read -r h; do
				printf '%s\t%s\n' "$h" "$(getent ahostsv4 "$h" | awk 'NR == 1 { print $1 }')"
			done
			;;
		nomad)
			nomad node status -json 2>/dev/null | json_query 'printf "%s\t%s\n", $_->{Name}, $_->{Address} for @$j;'
			;;
	esac
}
