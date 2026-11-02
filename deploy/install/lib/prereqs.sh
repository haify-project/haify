# shellcheck shell=bash
# Step 2 for every platform but Proxmox (which has its own, in
# deploy/proxmox/bootstrap/drbd.sh): DRBD 9, drbd-utils, drbd-reactor, the OCF
# agents and the LVM tools on every node, from the repository that carries
# them for the node's distribution.
#
# Every node needs DRBD, also one without a disk for the pool: a volume used
# there attaches as a diskless DRBD client.

UBUNTU_PPA=ppa:linbit/linbit-drbd9-stack

# pkg_missing <node> <pkg...>: prints the packages the node lacks.
pkg_missing() {
	local node="$1" p cmd=""
	shift
	for p in "$@"; do
		cmd+="if command -v dpkg-query >/dev/null; then dpkg-query -W -f='\${Status}' $(q "$p") 2>/dev/null | grep -q 'install ok installed'; else rpm -q $(q "$p") >/dev/null 2>&1; fi || echo $(q "$p"); "
	done
	check_on "$node" "$cmd"
}

apt_install() {
	# confold keeps a configuration file the node already has (a hand-written
	# /etc/drbd-reactor.toml) instead of stopping at dpkg's conffile prompt,
	# which over SSH fails and rolls back the whole transaction.
	run_on "$1" "DEBIAN_FRONTEND=noninteractive apt-get install -y -o Dpkg::Options::=--force-confdef -o Dpkg::Options::=--force-confold $2"
}

# Ubuntu: LINBIT's PPA has drbd-dkms, drbd-utils and drbd-reactor built for
# every supported release.
prereqs_ubuntu() {
	local node="$1" kernel ocf=resource-agents-extra missing
	if ! check_on "$node" "grep -rqs linbit-drbd9-stack /etc/apt/sources.list.d/" >/dev/null; then
		run_on "$node" "DEBIAN_FRONTEND=noninteractive apt-get install -y software-properties-common && add-apt-repository -y $UBUNTU_PPA"
	else
		note "$node: $UBUNTU_PPA already configured"
	fi
	kernel=$(check_on "$node" "uname -r")
	# 22.04 ships every agent in resource-agents; 24.04 split the ones Haify
	# uses (Filesystem, IPaddr2, nfsserver, iSCSITarget) into -extra.
	check_on "$node" "apt-cache show resource-agents-extra >/dev/null 2>&1" >/dev/null || ocf=resource-agents
	# The headers metapackage keeps DKMS building DRBD for every future
	# kernel; without it the next kernel boots with the in-tree DRBD 8.4.
	missing=$(pkg_missing "$node" "linux-headers-$kernel" linux-headers-generic drbd-dkms drbd-utils drbd-reactor \
		"$ocf" lvm2 thin-provisioning-tools sudo | tr '\n' ' ')
	missing="${missing% }"
	if [ -z "$missing" ]; then
		note "$node: packages already installed"
		return 0
	fi
	run_on "$node" "apt-get update"
	apt_install "$node" "$missing"
}

# Debian: no LINBIT suite for plain Debian, but the Proxmox ones are built on
# it (proxmox-8 on bookworm, proxmox-9 on trixie) and need nothing of PVE.
prereqs_debian() {
	local node="$1" ver major kernel arch missing
	ver="${NODE_OS[$node]#* }"
	case "${ver%%.*}" in
		12) major=8 ;;
		13) major=9 ;;
		*) die "$node: Debian $ver; DRBD 9 packages exist for Debian 12 and 13 (set HAIFY_LINBIT_REPO to use another repository)" ;;
	esac
	if linbit_repo_present "$node" "$major"; then
		note "$node: LINBIT repository already configured"
	else
		add_linbit_repo "$node" "$major"
	fi
	kernel=$(check_on "$node" "uname -r")
	arch=$(check_on "$node" "dpkg --print-architecture")
	missing=$(pkg_missing "$node" "linux-headers-$kernel" "linux-headers-$arch" drbd-dkms drbd-utils drbd-reactor \
		resource-agents lvm2 thin-provisioning-tools sudo | tr '\n' ' ')
	missing="${missing% }"
	if [ -z "$missing" ]; then
		note "$node: packages already installed"
		return 0
	fi
	run_on "$node" "apt-get update"
	apt_install "$node" "$missing"
}

# EL (Rocky, Alma, RHEL): ELRepo builds the DRBD 9 kernel module (kABI
# tracking, no compiler on the node) and the utilities; the OCF agents are in
# the distribution's HighAvailability repository.
prereqs_el() {
	local node="$1" rel missing
	rel=$(check_on "$node" "rpm -E %rhel")
	if ! check_on "$node" "rpm -q elrepo-release >/dev/null" >/dev/null; then
		run_on "$node" "rpm --import https://www.elrepo.org/RPM-GPG-KEY-v2-elrepo.org && dnf install -y https://www.elrepo.org/elrepo-release-$(q "$rel").el$(q "$rel").elrepo.noarch.rpm"
	fi
	missing=$(pkg_missing "$node" kmod-drbd9x drbd9x-utils drbd-reactor resource-agents lvm2 device-mapper-persistent-data sudo | tr '\n' ' ')
	missing="${missing% }"
	if [ -z "$missing" ]; then
		note "$node: packages already installed"
		return 0
	fi
	run_on "$node" "dnf install -y --enablerepo=elrepo,highavailability $missing" ||
		die "$node: could not install $missing. drbd-reactor is not in every EL repository; build it from https://github.com/LINBIT/drbd-reactor and rerun with --from-step 2"
}

# XCP-ng: the xcp-ng-linstor repository carries DRBD built for dom0's kernel.
prereqs_xcpng() {
	local node="$1" missing
	if ! check_on "$node" "rpm -q xcp-ng-release-linstor >/dev/null" >/dev/null; then
		run_on "$node" "yum install -y xcp-ng-release-linstor && rpm --import /etc/pki/rpm-gpg/RPM-GPG-KEY-xcpng"
	fi
	missing=$(pkg_missing "$node" kmod-drbd drbd-utils drbd-udev drbd-reactor | tr '\n' ' ')
	missing="${missing% }"
	if [ -n "$missing" ]; then
		run_on "$node" "yum install -y --enablerepo=xcp-ng-linstor $missing"
	else
		note "$node: packages already installed"
	fi
	# dom0's firewall drops everything not listed: DRBD's ports, and the
	# controller's on the hosts that can run it.
	if ! check_on "$node" "grep -q 'dport 7000:8999' /etc/sysconfig/iptables" >/dev/null; then
		run_on "$node" "sed -i '/-A RH-Firewall-1-INPUT -j REJECT/i -A RH-Firewall-1-INPUT -p tcp -m conntrack --ctstate NEW -m tcp --dport 7000:8999 -j ACCEPT\\n-A RH-Firewall-1-INPUT -p tcp -m conntrack --ctstate NEW -m tcp --dport 3374:3376 -j ACCEPT' /etc/sysconfig/iptables && systemctl restart iptables"
	fi
}

step_prereqs() {
	step 2 "DRBD 9, drbd-utils, drbd-reactor and the storage tools on every node"
	local node fam
	for node in "${NODES[@]}"; do
		fam=$(os_family "$node")
		log "$node: ${NODE_OS[$node]} ($fam)"
		case "$fam" in
			ubuntu) prereqs_ubuntu "$node" ;;
			debian) prereqs_debian "$node" ;;
			el) prereqs_el "$node" ;;
			xcp-ng) prereqs_xcpng "$node" ;;
			*) die "$node runs ${NODE_OS[$node]}, which the installer has no packages for. Install DRBD 9, drbd-utils and drbd-reactor
by hand (docs/node-prerequisites.md), then rerun with --from-step 3" ;;
		esac
		ensure_drbd9_loaded "$node"
		ensure_drbd_reactor_config "$node"
	done
}
