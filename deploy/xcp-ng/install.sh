#!/bin/sh
# Install the Haify SR driver on this XCP-ng host, as root. Run it on every
# host of the pool.
#
#   ./install.sh               install or upgrade, then restart the toolstack
#   ./install.sh --uninstall   remove the driver (destroy the Haify SRs first)
#
# The toolstack restart does not touch running VMs.
set -eu

SM=/opt/xensource/sm
CONF=/etc/xapi.conf
HERE=$(cd "$(dirname "$0")" && pwd)

[ "$(id -u)" = 0 ] || { echo "run as root" >&2; exit 1; }

plugins() { sed -n 's/^sm-plugins=//p' "$CONF"; }

if [ "${1:-}" = "--uninstall" ]; then
	rm -f "$SM/HaifySR" "$SM/HaifySR.py" "$SM/haifyclient.py"
	sed -i 's/^\(sm-plugins=.*\) haify\b/\1/' "$CONF"
	xe-toolstack-restart
	echo "removed the Haify SR driver"
	exit 0
fi

command -v drbdadm >/dev/null || {
	echo "DRBD 9 is required: yum install --enablerepo=xcp-ng-linstor kmod-drbd drbd-utils" >&2
	exit 1
}
install -m 0755 "$HERE/HaifySR.py" "$SM/HaifySR.py"
install -m 0644 "$HERE/haifyclient.py" "$SM/haifyclient.py"
ln -sf HaifySR.py "$SM/HaifySR"
python3 -m py_compile "$SM/HaifySR.py" "$SM/haifyclient.py"

# xapi only offers the SR types listed here.
if ! plugins | tr ' ' '\n' | grep -qx haify; then
	sed -i 's/^\(sm-plugins=.*\)$/\1 haify/' "$CONF"
fi
echo drbd > /etc/modules-load.d/drbd.conf
modprobe drbd

xe-toolstack-restart
echo "installed the Haify SR driver; xe sm-list type=haify shows it"
