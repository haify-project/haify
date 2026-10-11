#!/bin/sh
# Install the Haify libvirt hook on this KVM host, as root.
#
#   install.sh --controller <addr>[,<addr>...] [--node <name>] [--token-file <path>]
#   install.sh --uninstall
#
# <addr> is haify-controller's REST address, host[:port] (port 3375) or a full
# http(s):// URL; list every controller host when the controller runs Self-HA.
# --node is this host's Haify node name, when it differs from the short host
# name. The hook goes to /etc/libvirt/hooks/qemu.d/, beside any other qemu hook
# the host has, and libvirt's daemon is restarted to load it; running guests are
# not affected by that restart.
set -eu

HOOK_DIR=/etc/libvirt/hooks/qemu.d
HOOK=$HOOK_DIR/haify
CONF=/etc/haify/libvirt.conf
HERE=$(cd "$(dirname "$0")" && pwd)

controller="" node="" token="" uninstall=""
while [ $# -gt 0 ]; do
	case "$1" in
	--controller) controller=$2; shift 2 ;;
	--node) node=$2; shift 2 ;;
	--token-file) token=$2; shift 2 ;;
	--uninstall) uninstall=1; shift ;;
	*) echo "unknown argument: $1" >&2; exit 2 ;;
	esac
done

# The daemon that runs qemu guests: virtqemud with libvirt's modular daemons,
# libvirtd otherwise.
restart_libvirt() {
	for d in virtqemud libvirtd; do
		if systemctl is-active -q "$d" 2>/dev/null; then
			systemctl restart "$d"
			echo "restarted $d"
			return
		fi
	done
	echo "no running virtqemud or libvirtd; the hook loads when it starts"
}

# drbd-utils' drbd-graceful-disconnect.service disconnects every resource when
# the network goes down, which on shutdown can come before libvirt-guests has
# stopped the guests: a guest still writing then splits from the copy it
# restarts on elsewhere. Order the disconnect after the guests.
DRBD_ORDER=/etc/systemd/system/drbd-graceful-disconnect.service.d/haify-libvirt.conf

if [ -n "$uninstall" ]; then
	rm -f "$HOOK" "$DRBD_ORDER"
	systemctl daemon-reload
	restart_libvirt
	echo "removed $HOOK ($CONF kept)"
	exit 0
fi

[ "$(id -u)" = 0 ] || { echo "run as root" >&2; exit 1; }
command -v python3 >/dev/null || { echo "python3 is required" >&2; exit 1; }
command -v drbdadm >/dev/null || { echo "drbd-utils is required (drbdadm)" >&2; exit 1; }

if [ -n "$controller" ]; then
	install -d -m 0755 /etc/haify
	umask 077
	{
		echo "# Written by deploy/libvirt/install.sh; read by $HOOK."
		echo "CONTROLLER=$controller"
		[ -n "$node" ] && echo "NODE=$node"
		[ -n "$token" ] && echo "TOKEN_FILE=$token"
	} >"$CONF.new"
	mv "$CONF.new" "$CONF"
	echo "wrote $CONF"
elif [ ! -f "$CONF" ]; then
	echo "--controller is required on the first install" >&2
	exit 2
fi

install -d -m 0755 "$HOOK_DIR"
install -m 0755 "$HERE/haify-hook.py" "$HOOK"
echo "installed $HOOK"
install -d -m 0755 "$(dirname "$DRBD_ORDER")"
printf '%s\n' '# Installed by the Haify libvirt hook: disconnect DRBD only after the guests stop.' \
	'[Unit]' 'Before=libvirt-guests.service' >"$DRBD_ORDER"
systemctl daemon-reload
echo "DRBD disconnects only after the guests have stopped on shutdown"
restart_libvirt
