#!/bin/bash
# Tests for deploy/install/haify-install.sh, runnable on any machine with
# bash 4+ and perl: no root, no network, no nodes.
#
# The installer reaches every node through ssh, so a stub ssh on PATH
# (stub-ssh.sh) plays the cluster. It runs in --dry-run, where changes are
# printed rather than made, and the tests check that plan.
#
#   bash deploy/install/test.sh

set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$HERE/../.." && pwd)"
INSTALL="$HERE/haify-install.sh"

T=$(mktemp -d)
trap 'rm -rf "$T"' EXIT
PASS=0
FAIL=0

mkdir -p "$T/bin" "$T/haifybin" "$T/tmp" "$T/out-dir"
ln -s "$HERE/stub-ssh.sh" "$T/bin/ssh"
cat > "$T/bin/scp" <<'EOF'
#!/bin/bash
printf 'scp %s\n' "$*" >> "$STUB_DIR/unhandled"
exit 1
EOF
cat > "$T/bin/hostname" <<'EOF'
#!/bin/bash
case "$1" in -I) echo 10.0.1.11 ;; *) echo n1 ;; esac
EOF
cat > "$T/bin/ip" <<'EOF'
#!/bin/bash
case "$*" in
	*"route get"*) echo "1.1.1.1 via 10.0.1.1 dev eth0 src 10.0.1.11 uid 0" ;;
	*) echo "2: eth0    inet 10.0.1.11/24 brd 10.0.1.255 scope global eth0" ;;
esac
EOF
cat > "$T/bin/xe" <<'EOF'
#!/bin/bash
# The pool master's host list, as xe prints it.
for i in 1 2 3; do
	printf 'name-label ( RW)    : n%s\n    address ( RO)    : 10.0.1.1%s\n\n' "$i" "$i"
done
EOF
chmod +x "$T/bin/"*
# x86-64 ELF headers, which is what the installer reads the CPU from.
for f in haify-controller service-ip haify haify-docker haify-opennebula; do
	printf '\x7fELF\x02\x01\x01\x00\x00\x00\x00\x00\x00\x00\x00\x00\x02\x00\x3e\x00' > "$T/haifybin/$f"
	chmod +x "$T/haifybin/$f"
done

# run_install <mode> <args...>: output in $T/out, exit status in $RC.
run_install() {
	local mode="$1"
	shift
	rm -f "$T/ssh.log" "$T/unhandled"
	RC=0
	env PATH="$T/bin:$PATH" TMPDIR="$T/tmp" STUB_DIR="$T" STUB_MODE="$mode" \
		HAIFY_BIN_DIR="$T/haifybin" HAIFY_CONFIG_DIR="$REPO_ROOT/configs" HAIFY_OUT_DIR="$T/out-dir" \
		HAIFY_LOCAL_ENV="${LOCAL_ENV:-plain}" \
		bash "$INSTALL" --dry-run --yes "$@" < /dev/null > "$T/out" 2>&1 || RC=$?
}

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
expect_clean_stubs() {
	if [ -s "$T/unhandled" ]; then bad "unmodelled or forbidden commands:
$(cat "$T/unhandled")"; else ok; fi
}

NODES3=(--nodes n1=10.0.1.11,n2=10.0.1.12,n3=10.0.1.13)

case_start "fresh Ubuntu KVM hosts: full plan"
STUB_DETECT='libvirt\nos ubuntu 24.04 x86_64' run_install fresh "${NODES3[@]}" --devices /dev/sdb --vip 10.0.1.250/24
expect_rc 0
expect_out "platform: KVM / libvirt"
expect_out "add-apt-repository -y ppa:linbit/linbit-drbd9-stack"
expect_out "drbd-dkms drbd-utils drbd-reactor resource-agents-extra"
expect_out "ssh-keygen -q -t ed25519"
expect_out "[dry-run] n1           systemctl enable --now haify-controller"
expect_out "haify node register --name n3 --address 10.0.1.13"
expect_out "haify pool create --name vg0 --type lvm-thin --nodes n2 --devices /dev/sdb"
expect_out "haify ha self enable --vip 10.0.1.250/24 --pool vg0 --nodes n1,n2,n3"
expect_out "./install.sh --controller 10.0.1.11,10.0.1.12,10.0.1.13 --node n2"
expect_out "nothing was changed"
expect_clean_stubs

case_start "installed KVM hosts: rerun changes nothing"
export STUB_NODES_JSON='{"name":"n1","address":"10.0.1.11"},{"name":"n2","address":"10.0.1.12"},{"name":"n3","address":"10.0.1.13"}'
export STUB_POOLS_JSON='{"name":"haify_vg0","node":"10.0.1.11"},{"name":"haify_vg0","node":"10.0.1.12"},{"name":"haify_vg0","node":"10.0.1.13"}'
STUB_DETECT='libvirt\nos ubuntu 24.04 x86_64' run_install done "${NODES3[@]}" --vip 10.0.1.250/24
expect_rc 0
expect_out "already configured"
expect_out "pool vg0 exists"
expect_out "already enabled"
expect_out "libvirt hook installed"
expect_no_out "[dry-run]"
expect_clean_stubs

case_start "XCP-ng: member list from xe, SR driver and SR"
LOCAL_ENV=xcp-ng STUB_DETECT='xcp-ng\nos xcp-ng 8.3.0 x86_64' run_install fresh --devices /dev/sdb --vip 10.0.1.250/24
expect_rc 0
expect_out "nodes from XCP-ng's own member list"
expect_out "yum install -y --enablerepo=xcp-ng-linstor kmod-drbd drbd-utils drbd-udev drbd-reactor"
expect_out "--dport 7000:8999"
expect_out "xe sr-create type=haify shared=true content-type=user name-label=Haify device-config:controller=10.0.1.250 device-config:pool=vg0 device-config:replicas=2"
expect_clean_stubs

case_start "Debian with Docker: LINBIT repo by release, plugin pointed at the controller"
STUB_DETECT='docker\nos debian 12 x86_64' run_install fresh --nodes n1=10.0.1.11 --devices /dev/sdb
expect_rc 0
expect_out "https://packages.linbit.com/public proxmox-8 drbd-9"
expect_out "linux-headers-amd64"
expect_out "HAIFY_DOCKER_ARGS"
expect_out "--controller 10.0.1.11:3374 --pool vg0"
expect_out "Self-HA:           off"
expect_clean_stubs

case_start "address-only nodes are named by their hostname"
STUB_DETECT='os ubuntu 24.04 x86_64' run_install fresh --nodes 10.0.1.11,10.0.1.12 --devices /dev/sdb --no-self-ha
expect_rc 0
expect_out "haify node register --name n2 --address 10.0.1.12"
expect_out "platform: Linux, no platform"
expect_clean_stubs

case_start "nodes on different platforms need --env"
STUB_DETECT_1='libvirt\nos ubuntu 24.04 x86_64' STUB_DETECT_2='docker\nos ubuntu 24.04 x86_64' \
	run_install fresh --nodes n1=10.0.1.11,n2=10.0.1.12 --devices /dev/sdb --no-self-ha
expect_rc 1
expect_out "choose one with --env"
STUB_DETECT_1='libvirt\nos ubuntu 24.04 x86_64' STUB_DETECT_2='docker\nos ubuntu 24.04 x86_64' \
	run_install fresh --nodes n1=10.0.1.11,n2=10.0.1.12 --devices /dev/sdb --no-self-ha --env libvirt
expect_rc 0
expect_out "platform: KVM / libvirt"

case_start "a disk in use is refused before anything changes"
STUB_DEVICE_VERDICT="in-use: mounted at /srv" STUB_DETECT='os ubuntu 24.04 x86_64' run_install fresh --nodes n1=10.0.1.11 --devices /dev/sdb
expect_rc 1
expect_out "never used for the pool"
expect_no_out "[dry-run]"

case_start "no disk named and nobody to ask"
STUB_DETECT='os ubuntu 24.04 x86_64' run_install fresh --nodes n1=10.0.1.11
expect_rc 1
expect_out "--devices is required"

case_start "Kubernetes: chart values written for the operator"
STUB_DETECT='kubernetes\ndocker\nos ubuntu 24.04 x86_64' run_install fresh "${NODES3[@]}" --devices /dev/sdb --vip 10.0.1.250/24
expect_rc 0
expect_out "platform: Kubernetes"
expect_out "write $T/out-dir/kubernetes/values.yaml"
expect_out "helm install haify-csi"

case_start "unknown OS stops at step 2 with what to install"
STUB_DETECT='os arch 0 x86_64' run_install fresh --nodes n1=10.0.1.11 --devices /dev/sdb
expect_rc 1
expect_out "has no packages for"
expect_out "--from-step 3"

case_start "unreachable node"
STUB_UNREACHABLE=n2 STUB_DETECT='os ubuntu 24.04 x86_64' run_install fresh --nodes n1=10.0.1.11,n2=10.0.1.12 --devices /dev/sdb --no-self-ha
expect_rc 1
expect_out "root SSH from this machine to n2"

case_start "Proxmox VE: handed over to the PVE bootstrap"
LOCAL_ENV=proxmox run_install fresh --devices /dev/sdb --vip 10.0.1.250/24
expect_out "handing over to deploy/proxmox/bootstrap.sh"

printf '\ninstaller tests: %d passed, %d failed\n' "$PASS" "$FAIL"
[ "$FAIL" -eq 0 ]
