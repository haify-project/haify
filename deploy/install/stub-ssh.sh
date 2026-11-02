#!/bin/bash
# Test double for ssh, used by test.sh (installed on PATH as `ssh`).
#
# Nodes are told apart by address: 10.0.1.1N is node nN. Each answers the
# installer's read-only probes as a fresh node (STUB_MODE=fresh) or an
# installed one (STUB_MODE=done) would; what it runs is in STUB_DETECT (or
# STUB_DETECT_N for node N). Every call is logged, and a command nobody
# modelled lands in $STUB_DIR/unhandled so the tests fail on it.

set -uo pipefail

target=""
args=()
while [ $# -gt 0 ]; do
	case "$1" in
		-o) shift 2 ;;
		-*) shift ;;
		*) args+=("$1"); shift ;;
	esac
done
target="${args[0]#root@}"
cmd="${args[*]:1}"
n="${target##*.1}"
node="n$n"
printf '%s\t%s\n' "$node" "${cmd//$'\n'/ }" >> "$STUB_DIR/ssh.log"

if [ -n "${STUB_UNREACHABLE:-}" ] && [ "$node" = "$STUB_UNREACHABLE" ]; then
	echo "ssh: connect to host $target: Connection refused" >&2
	exit 255
fi

fresh() { [ "$STUB_MODE" = fresh ]; }
answer() { if fresh; then exit "$1"; else exit "$2"; fi; }
detect_var="STUB_DETECT_$n"

# shellcheck disable=SC2016 # patterns match the probes' text literally
case "$cmd" in
	"") exit 1 ;; # a master connection (-fN): the tests run without one
	true) exit 0 ;;
	"hostname -s") echo "$node" ;;
	*os-release*) printf '%b\n' "${!detect_var:-${STUB_DETECT:-os ubuntu 24.04 x86_64}}" ;;
	*linbit-drbd9-stack*) answer 1 0 ;;
	"uname -r") echo 6.8.0-45-generic ;;
	"dpkg --print-architecture") echo amd64 ;;
	"apt-cache show resource-agents-extra"*) exit 0 ;;
	"rpm -q xcp-ng-release-linstor"* | "rpm -q elrepo-release"*) answer 1 0 ;;
	"rpm -E %rhel") echo 9 ;;
	*linbit.asc*grep\ -qxF*) answer 1 0 ;;
	*"|| echo"*)
		# pkg_missing: fresh nodes miss every package asked about.
		if fresh; then printf '%s\n' "$cmd" | grep -o 'echo [a-z0-9.+-]*' | cut -d' ' -f2; fi
		;;
	*"dport 7000:8999"*) answer 1 0 ;;
	*/sys/module/drbd/refcnt*) echo "0 0" ;;
	*/sys/module/drbd/version*) fresh && exit 1; echo 9.2.12 ;;
	"test -f /etc/drbd-reactor.toml") answer 1 0 ;;
	*"snippets"*"/etc/drbd-reactor.toml"*) answer 1 0 ;;
	*"systemctl is-enabled --quiet drbd-reactor"*) answer 1 0 ;;
	"test -f /root/.ssh/id_ed25519 || test -f /root/.ssh/id_rsa") answer 1 0 ;;
	"cat /root/.ssh/id_ed25519.pub"*) echo "ssh-ed25519 AAAAstub root@$node" ;;
	*"/root/.ssh/authorized_keys"*) answer 1 0 ;;
	"test -r /root/.ssh/id_ed25519") answer 1 0 ;;
	"test -r "*) exit 1 ;;
	sha256sum\ *)
		fresh && exit 0
		f="${cmd#sha256sum }"
		f="${f%% 2>*}"
		# An installed node has exactly what the installer would copy there.
		case "$f" in
			/opt/haify/bin/haify-controller) src="$HAIFY_BIN_DIR/haify-controller" ;;
			*/service-ip) src="$HAIFY_BIN_DIR/service-ip" ;;
			/usr/local/bin/haify) src="$HAIFY_BIN_DIR/haify" ;;
			/etc/systemd/system/*) src="$HAIFY_CONFIG_DIR/${f##*/}" ;;
			/root/.dispatch/config.toml) for src in "$TMPDIR"/haify-install.*/dispatch-config.toml; do break; done ;;
			*) src="" ;;
		esac
		[ -n "$src" ] && [ -r "$src" ] && sha256sum "$src" | cut -d' ' -f1
		;;
	"test -d /etc/haify"*) answer 1 0 ;;
	"test -L /usr/local/bin/haify-cli") answer 1 0 ;;
	"test -f /etc/haify/controller.toml") answer 1 0 ;;
	"systemctl is-active --quiet haify-controller")
		fresh && exit 3
		[ "$node" = n1 ] && exit 0
		exit 3
		;;
	"drbdadm role haify-meta") fresh && exit 10; echo Secondary ;;
	*"/v1/nodes") fresh && exit 22; printf '{"nodes":[%s]}' "$STUB_NODES_JSON" ;;
	*"/v1/pools") fresh && exit 22; printf '{"pools":[%s]}' "$STUB_POOLS_JSON" ;;
	"haify ha self status") fresh && exit 1; printf 'Controller self-HA: enabled\n  Active node: n1\n' ;;
	"vgs haify_vg0") answer 5 0 ;;
	d=*readlink*) echo "${STUB_DEVICE_VERDICT:-clean}" ;;
	"test -x /etc/libvirt/hooks/qemu.d/haify") answer 1 0 ;;
	"systemctl is-active --quiet haify-docker") answer 1 0 ;;
	"test -f /opt/xensource/sm/HaifySR.py") answer 1 0 ;;
	"xe sr-list type=haify"*) answer 1 0 ;;
	*)
		printf '%s\t%s\n' "$node" "$cmd" >> "$STUB_DIR/unhandled"
		exit 1
		;;
esac
exit 0
