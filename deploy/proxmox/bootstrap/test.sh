#!/bin/bash
# Tests for deploy/proxmox/bootstrap.sh, runnable on any Linux machine with
# bash and perl: no PVE, no root, no network.
#
# bootstrap.sh runs every node command through ssh, so a stub ssh on PATH
# (stub-ssh.sh) stands in for the whole cluster: it answers the read-only
# probes as a fresh or a bootstrapped node would. The script runs in --dry-run,
# where changes are only printed, and the tests check that plan.
#
#   ./deploy/proxmox/bootstrap/test.sh

set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROXMOX_DIR="$(cd "$HERE/.." && pwd)"
REPO_ROOT="$(cd "$PROXMOX_DIR/../.." && pwd)"
BOOTSTRAP="$PROXMOX_DIR/bootstrap.sh"

T=$(mktemp -d)
trap 'rm -rf "$T"' EXIT

PASS=0
FAIL=0

# ---------------------------------------------------------------------------
# Fixture: stub commands, a fake /etc/pve, fake haify binaries
# ---------------------------------------------------------------------------

mkdir -p "$T/bin" "$T/pve/nodes/pve1" "$T/haifybin" "$T/tmp"
ln -s "$HERE/stub-ssh.sh" "$T/bin/ssh"
cat > "$T/bin/scp" <<'EOF'
#!/bin/bash
# A dry run copies nothing; any scp is a leak the tests report.
printf 'scp %s\n' "$*" >> "$STUB_DIR/unhandled"
exit 1
EOF
cat > "$T/bin/pvecm" <<'EOF'
#!/bin/bash
# Only `pvecm nodes` is run locally, for the fallback without .members.
[ "$1" = nodes ] || exit 1
printf 'Membership information\n----------------------\n'
printf '    Nodeid      Votes Name\n'
printf '         1          1 pve1 (local)\n'
printf '         2          1 pve2\n'
EOF
cat > "$T/bin/hostname" <<'EOF'
#!/bin/bash
echo pve1
EOF
chmod +x "$T/bin/scp" "$T/bin/pvecm" "$T/bin/hostname"
for f in haify-controller service-ip haify; do
	printf '#!/bin/sh\necho %s\n' "$f" > "$T/haifybin/$f"
	chmod +x "$T/haifybin/$f"
done
: > "$T/pve/nodes/pve1/ssh_known_hosts"

# members <online...>: write .members for pve1..pveN, 1 = online.
members() {
	local i=0 o sep="" list=""
	for o in "$@"; do
		i=$((i + 1))
		list+="$sep\"pve$i\":{\"id\":$i,\"online\":$o,\"ip\":\"10.0.0.1$i\"}"
		sep=","
	done
	printf '{"nodename":"pve1","version":7,"cluster":{"name":"lab","version":3,"nodes":%d,"quorate":1},"nodelist":{%s}}\n' \
		"$#" "$list" > "$T/pve/.members"
}

# done_state <n>: what a bootstrapped controller reports for pve1..pveN.
done_state() {
	local i nodes="" pools="" sep=""
	for i in $(seq 1 "$1"); do
		nodes+="$sep{\"name\":\"pve$i\",\"address\":\"10.0.0.1$i\",\"state\":\"online\"}"
		# The controller's pool list names a node by its address.
		pools+="$sep{\"name\":\"haify_vg0\",\"node\":\"10.0.0.1$i\",\"type\":\"thin_pool\"}"
		sep=","
	done
	export STUB_NODES_JSON="$nodes" STUB_POOLS_JSON="$pools"
}

# run_bootstrap <mode> <args...>: run bootstrap.sh against the stub cluster,
# leaving its output in $T/out and its exit status in $RC.
run_bootstrap() {
	local mode="$1"
	shift
	rm -f "$T/ssh.log" "$T/unhandled"
	RC=0
	env PATH="$T/bin:$PATH" TMPDIR="$T/tmp" STUB_DIR="$T" STUB_MODE="$mode" \
		HAIFY_PVE_DIR="$T/pve" HAIFY_BIN_DIR="$T/haifybin" HAIFY_CONFIG_DIR="$REPO_ROOT/configs" \
		STUB_PLUGIN_SRC="$PROXMOX_DIR" STUB_VOTES="${STUB_VOTES:-3}" \
		bash "$BOOTSTRAP" "$@" > "$T/out" 2>&1 || RC=$?
}

# ---------------------------------------------------------------------------
# Assertions
# ---------------------------------------------------------------------------

CASE=""
case_start() { CASE="$1"; }
ok() { PASS=$((PASS + 1)); }
bad() {
	FAIL=$((FAIL + 1))
	printf 'FAIL [%s]: %s\n' "$CASE" "$1"
	sed 's/^/    | /' "$T/out" | tail -25
}
expect_rc() { if [ "$RC" -eq "$1" ]; then ok; else bad "exit status $RC, want $1"; fi; }
expect_out() { if grep -qF -- "$1" "$T/out"; then ok; else bad "output lacks: $1"; fi; }
expect_no_out() { if grep -qF -- "$1" "$T/out"; then bad "output has: $1"; else ok; fi; }
expect_count() {
	local n
	n=$(grep -cF -- "$2" "$T/out" || true)
	if [ "$n" -eq "$1" ]; then ok; else bad "'$2' appears $n times, want $1"; fi
}
expect_clean_stubs() {
	if [ -s "$T/unhandled" ]; then
		bad "commands reached the stubs that a dry run must not run, or that no stub models:
$(cat "$T/unhandled")"
	else
		ok
	fi
}

DEV=(--devices /dev/sdb)

# ---------------------------------------------------------------------------
# Cases
# ---------------------------------------------------------------------------

case_start "fresh 3-node cluster: full plan"
members 1 1 1
run_bootstrap fresh --dry-run "${DEV[@]}" --vip 10.0.0.250/24
expect_rc 0
expect_clean_stubs
for n in pve1 pve2 pve3; do
	expect_out "[dry-run] $n"
	expect_out "deb [signed-by=/etc/apt/keyrings/linbit.asc] https://packages.linbit.com/public proxmox-8 drbd-9"
done
expect_count 3 "apt-get install -y -o Dpkg::Options::=--force-confdef -o Dpkg::Options::=--force-confold proxmox-headers-6.8.12-1-pve proxmox-default-headers drbd-dkms drbd-utils drbd-reactor sudo"
expect_count 3 "modprobe drbd"
expect_count 3 "systemctl enable --now drbd-reactor"
expect_count 3 "-> /root/.dispatch/config.toml (mode 0600)"
expect_count 3 "haifybin/haify-controller -> /opt/haify/bin/haify-controller (mode 0755)"
expect_count 3 "-> /etc/haify/controller.toml (mode 0644)"
expect_count 1 "systemctl enable --now haify-controller"
expect_out "[dry-run] pve1         systemctl enable --now haify-controller"
expect_out "haify node register --name pve2 --address 10.0.0.12"
expect_count 3 "haify node register"
expect_out "haify pool create --name vg0 --type lvm-thin --nodes pve3 --devices /dev/sdb"
expect_out "haify ha self enable --vip 10.0.0.250/24 --pool vg0 --nodes pve1,pve2,pve3"
expect_count 3 "./preflight.sh 10.0.0.11,10.0.0.12,10.0.0.13"
expect_count 3 "./install.sh"
expect_out "pvesm add haify haify0 --controller 10.0.0.11,10.0.0.12,10.0.0.13 --haifypool vg0 --replicas 2 --storagetype lvm-thin --content images,rootdir --shared 1"
expect_out "OK: corosync has an odd number of votes (3)"
expect_out "dkms status drbd"
expect_out "nothing was changed"

case_start "every probe ran over ssh with PVE's host key alias"
if awk -F'\t' '$1 == "" { bad = 1 } END { exit bad }' "$T/ssh.log"; then ok; else bad "ssh called without HostKeyAlias"; fi

case_start "bootstrapped cluster: rerun changes nothing"
done_state 3
run_bootstrap "done" --dry-run "${DEV[@]}" --vip 10.0.0.250/24
expect_rc 0
expect_clean_stubs
expect_no_out "[dry-run]"
expect_out "LINBIT repository already configured"
expect_out "pool vg0 exists"
expect_out "controller already running on pve1"
expect_out "Self-HA"
expect_out "already enabled"
expect_out "plugin already installed and current"
expect_out "storage 'haify0' (type haify) already present"

case_start "rerun finds the controller where Self-HA moved it"
STUB_ACTIVE_CONTROLLER=pve3 run_bootstrap "done" --dry-run "${DEV[@]}" --vip 10.0.0.250/24
expect_rc 0
expect_out "controller already running on pve3"
expect_no_out "[dry-run]"

case_start "--devices is required"
members 1 1 1
run_bootstrap fresh --dry-run --vip 10.0.0.250/24
expect_rc 1
expect_out "--devices is required"
expect_no_out "Step 1 failed"

case_start "--node-devices covers a node without --devices"
run_bootstrap fresh --dry-run --vip 10.0.0.250/24 --storage-nodes pve1,pve2 \
	--node-devices pve1=/dev/sdb --node-devices pve2=/dev/nvme1n1,/dev/nvme2n1
expect_rc 0
expect_out "--nodes pve2 --devices /dev/nvme1n1,/dev/nvme2n1"
expect_out "haify ha self enable --vip 10.0.0.250/24 --pool vg0 --nodes pve1,pve2"
expect_out "--controller 10.0.0.11,10.0.0.12 "
expect_count 2 "-> /root/.dispatch/config.toml"
# pve3 holds no disk but still gets DRBD, registration and the plugin.
expect_out "haify node register --name pve3 --address 10.0.0.13"
expect_out "[dry-run] pve3         modprobe drbd"

case_start "a disk with partitions is refused without --force-wipe"
STUB_DEVICE_VERDICT="wipeable: has partitions sdb1 " run_bootstrap fresh --dry-run "${DEV[@]}" --vip 10.0.0.250/24
expect_rc 1
expect_out "pve1: /dev/sdb has partitions sdb1"
expect_out "pve3: /dev/sdb has partitions sdb1"
expect_out "then add --force-wipe"
# Refused before step 2: nothing was installed or planned.
expect_no_out "[dry-run]"
expect_no_out "Step 2"

case_start "--force-wipe wipes a disk that carries a signature"
STUB_DEVICE_VERDICT="wipeable: carries LVM2_member " run_bootstrap fresh --dry-run "${DEV[@]}" --vip 10.0.0.250/24 --force-wipe
expect_rc 0
# shellcheck disable=SC2016 # the remote command's text, matched literally
expect_count 3 'wipefs -a "$r"'
expect_out "haify pool create --name vg0"

case_start "a mounted disk is refused even with --force-wipe"
STUB_DEVICE_VERDICT="in-use: mounted at /var/lib/vz" run_bootstrap fresh --dry-run "${DEV[@]}" --vip 10.0.0.250/24 --force-wipe
expect_rc 1
expect_out "mounted at /var/lib/vz; never used for the pool, not even with --force-wipe"
expect_no_out "wipefs -a"

case_start "disks of a node that already has the pool's volume group are not rechecked"
STUB_DEVICE_VERDICT="wipeable: carries LVM2_member " done_state 3
STUB_DEVICE_VERDICT="wipeable: carries LVM2_member " run_bootstrap "done" --dry-run "${DEV[@]}" --vip 10.0.0.250/24
expect_rc 0
expect_no_out "[dry-run]"

case_start "a failed step names the step to resume from"
STUB_PVE_MAJOR=7 run_bootstrap fresh --dry-run "${DEV[@]}" --vip 10.0.0.250/24 --yes
expect_rc 1
expect_out "pve1 runs PVE 7"
expect_out "Step 2 failed"
expect_out "--dry-run --devices /dev/sdb --vip 10.0.0.250/24 --from-step 2"
expect_no_out " --yes"

case_start "an offline member stops the bootstrap"
members 1 0 1
run_bootstrap fresh --dry-run "${DEV[@]}" --vip 10.0.0.250/24
expect_rc 1
expect_out "member(s) offline: pve2"

case_start "a member without root SSH stops the bootstrap"
members 1 1 1
STUB_UNREACHABLE=pve2 run_bootstrap fresh --dry-run "${DEV[@]}" --vip 10.0.0.250/24
expect_rc 1
expect_out "root SSH from pve1 to pve2 (10.0.0.12) failed"
expect_out "pvecm updatecerts"

case_start "several storage nodes need --vip"
run_bootstrap fresh --dry-run "${DEV[@]}"
expect_rc 1
expect_out "--vip is required"

case_start "--no-self-ha runs the controller on this node only"
run_bootstrap fresh --dry-run "${DEV[@]}" --no-self-ha
expect_rc 0
expect_out "skipped (--no-self-ha)"
expect_count 1 "-> /root/.dispatch/config.toml"
expect_out "--controller 10.0.0.11 "
expect_no_out "haify ha self enable"

case_start "PVE 9 uses the proxmox-9 suite"
STUB_PVE_MAJOR=9 run_bootstrap fresh --dry-run "${DEV[@]}" --vip 10.0.0.250/24
expect_rc 0
expect_out "packages.linbit.com/public proxmox-9 drbd-9"

case_start "no headers for the running kernel stops step 2"
STUB_HEADERS="proxmox-default-headers" run_bootstrap fresh --dry-run "${DEV[@]}" --vip 10.0.0.250/24
expect_rc 1
expect_out "no headers package for the running kernel 6.8.12-1-pve"
expect_no_out "apt-get install"

case_start "a real (not dry) rerun on a bootstrapped cluster runs no changing command"
if [ "$(id -u)" -eq 0 ]; then
	members 1 1 1
	done_state 3
	run_bootstrap "done" --yes "${DEV[@]}" --vip 10.0.0.250/24
	expect_rc 0
	expect_clean_stubs
	expect_out "Storage haify0 is available on every node"
else
	echo "skip: not root, the real-run case needs root"
fi

case_start "single-node cluster: no Self-HA, one replica"
members 1
STUB_VOTES=1 run_bootstrap fresh --dry-run "${DEV[@]}"
expect_rc 0
expect_out "skipped: one storage node"
expect_out "--replicas 1"
expect_out "replicas=1: disks have a single copy"

case_start "two-node cluster: QDevice and tiebreaker reported missing"
members 1 1
STUB_VOTES=2 run_bootstrap fresh --dry-run "${DEV[@]}" --vip 10.0.0.250/24
expect_rc 0
expect_out "MISSING: corosync has 2 votes and no QDevice"
expect_out "MISSING: replicas=2 and only 2 haify node(s)"
expect_no_out "pvecm qdevice setup 10"

case_start "two-node cluster with a QDevice"
STUB_QDEVICE=1 STUB_VOTES=3 run_bootstrap fresh --dry-run "${DEV[@]}" --vip 10.0.0.250/24
expect_rc 0
expect_out "OK: corosync has a QDevice"

case_start "membership from pvecm nodes + corosync.conf when .members is unusable"
rm -f "$T/pve/.members"
cat > "$T/pve/corosync.conf" <<'EOF'
nodelist {
  node {
    name: pve1
    nodeid: 1
    quorum_votes: 1
    ring0_addr: 10.0.0.11
  }
  node {
    name: pve2
    nodeid: 2
    quorum_votes: 1
    ring0_addr: 10.0.0.12
  }
}

quorum {
  provider: corosync_votequorum
}
EOF
STUB_VOTES=2 run_bootstrap fresh --dry-run "${DEV[@]}" --vip 10.0.0.250/24
expect_rc 0
expect_out "falling back to pvecm nodes"
expect_out "pve2  10.0.0.12"
expect_out "haify node register --name pve2 --address 10.0.0.12"
rm -f "$T/pve/corosync.conf"

case_start "--from-step 8 only adds the storage"
members 1 1 1
run_bootstrap fresh --dry-run "${DEV[@]}" --vip 10.0.0.250/24 --from-step 8
expect_rc 0
expect_count 1 "[dry-run]"
expect_out "pvesm add haify haify0"

case_start "a storage ID taken by another type is refused"
STUB_STORAGE_EXTRA="nfs haify0" run_bootstrap fresh --dry-run "${DEV[@]}" --vip 10.0.0.250/24 --from-step 8
expect_rc 1
expect_out "already exists with another type"

case_start "controller from a .deb"
mkdir -p "$T/deb/DEBIAN"
printf 'Package: haify-controller\nVersion: 1.2.3\nArchitecture: amd64\nMaintainer: test <test@example.com>\nDescription: test\n' \
	> "$T/deb/DEBIAN/control"
dpkg-deb --build "$T/deb" "$T/haify-controller_1.2.3_amd64.deb" >/dev/null 2>&1 || true
if [ -r "$T/haify-controller_1.2.3_amd64.deb" ]; then
	HAIFY_CONTROLLER_DEB="$T/haify-controller_1.2.3_amd64.deb" run_bootstrap fresh --dry-run "${DEV[@]}" --vip 10.0.0.250/24
	expect_rc 0
	expect_count 3 "apt-get install -y /tmp/haify-controller_1.2.3_amd64.deb"
	expect_no_out "/opt/haify/bin/haify-controller"
	HAIFY_CONTROLLER_DEB="$T/haify-controller_1.2.3_amd64.deb" STUB_DEB_VERSION=1.2.3 \
		run_bootstrap "done" --dry-run "${DEV[@]}" --vip 10.0.0.250/24
	expect_rc 0
	expect_out "haify-controller 1.2.3 already installed"
else
	echo "skip: dpkg-deb unavailable, .deb case not run"
fi

case_start "an unused in-tree DRBD 8.4 is swapped for the DKMS module"
STUB_DRBD_VERSION=8.4.11 run_bootstrap fresh --dry-run "${DEV[@]}" --vip 10.0.0.250/24
expect_rc 0
expect_count 3 "DRBD 8.4.11 is loaded but unused"
expect_count 3 "rmmod drbd"

case_start "a DRBD 8.4 in use stops the run"
STUB_DRBD_VERSION=8.4.11 STUB_DRBD_USE="1 0" run_bootstrap fresh --dry-run "${DEV[@]}" --vip 10.0.0.250/24
expect_rc 1
expect_out "DRBD 8.4.11 is loaded and in use"
expect_no_out "replacing it with the DKMS module"

case_start "missing binaries are reported before anything runs"
HAIFY_BIN_DIR_SAVE="$T/haifybin"
mv "$T/haifybin/haify" "$T/haify.away"
run_bootstrap fresh --dry-run "${DEV[@]}" --vip 10.0.0.250/24
expect_rc 1
expect_out "haifybin/haify not found"
expect_no_out "[dry-run]"
mv "$T/haify.away" "$HAIFY_BIN_DIR_SAVE/haify"

echo
echo "bootstrap tests: $PASS passed, $FAIL failed"
[ "$FAIL" -eq 0 ]
