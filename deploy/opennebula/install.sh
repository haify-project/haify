#!/bin/sh
# Install the Haify storage driver on an OpenNebula frontend, as root.
#
#   install.sh [--binary <path to haify-opennebula>] [--no-restart]
#
# Installs haify-opennebula to /usr/local/bin, the datastore and transfer
# action scripts to /var/lib/one/remotes, registers the haify drivers in
# /etc/one/oned.conf (a copy is kept as oned.conf.haify-bak), gives oneadmin
# the API token, and restarts OpenNebula. Afterwards run `onehost sync -f`
# so the hosts get the new transfer scripts.
#
# KVM hosts need the libvirt hook (deploy/libvirt/install.sh), which promotes
# and demotes a guest's disks as it starts, stops and migrates.
set -eu

HERE=$(cd "$(dirname "$0")" && pwd)
REMOTES=/var/lib/one/remotes
ONED=/etc/one/oned.conf
binary="" restart=1
while [ $# -gt 0 ]; do
	case "$1" in
	--binary) binary=$2; shift 2 ;;
	--no-restart) restart=""; shift ;;
	*) echo "unknown argument: $1" >&2; exit 2 ;;
	esac
done

[ "$(id -u)" = 0 ] || { echo "run as root" >&2; exit 1; }
[ -f "$ONED" ] || { echo "$ONED not found: run this on the OpenNebula frontend" >&2; exit 1; }

if [ -z "$binary" ]; then
	for b in "$HERE/haify-opennebula" "$HERE/../../bin/haify-opennebula"; do
		[ -x "$b" ] && binary=$b && break
	done
fi
[ -n "$binary" ] && [ -x "$binary" ] || { echo "haify-opennebula not found; pass --binary" >&2; exit 1; }
if [ "$(readlink -f "$binary")" != /usr/local/bin/haify-opennebula ]; then
	install -m 0755 "$binary" /usr/local/bin/haify-opennebula
	echo "installed /usr/local/bin/haify-opennebula"
fi

# cp -P keeps the links to the fs and ssh drivers' scripts as links.
for kind in datastore tm; do
	rm -rf "$REMOTES/$kind/haify"
	cp -RP "$HERE/$kind/haify" "$REMOTES/$kind/haify"
	chown -hR oneadmin:oneadmin "$REMOTES/$kind/haify"
done
echo "installed $REMOTES/datastore/haify and $REMOTES/tm/haify"

# The driver reads the token as oneadmin; /etc/haify/token is root's.
if [ -f /etc/haify/token ]; then
	install -d -o oneadmin -g oneadmin -m 0700 /var/lib/one/.haify
	install -o oneadmin -g oneadmin -m 0600 /etc/haify/token /var/lib/one/.haify/token
	echo "copied /etc/haify/token to /var/lib/one/.haify/token"
fi

if grep -q 'NAME *= *"haify"' "$ONED"; then
	echo "$ONED already knows the haify drivers"
else
	cp -p "$ONED" "$ONED.haify-bak"
	# Add haify to the transfer drivers (TM_MAD -d), and to the datastore
	# drivers (DATASTORE_MAD -d) and system datastore drivers (-s).
	awk '
		/^TM_MAD *= *\[/ { block = "tm" }
		/^DATASTORE_MAD *= *\[/ { block = "ds" }
		/^\]/ { block = "" }
		block == "tm" && /ARGUMENTS/ { sub(/-d [^ "]*/, "&,haify") }
		block == "ds" && /ARGUMENTS/ { sub(/-d [^ "]*/, "&,haify"); sub(/-s [^ "]*/, "&,haify") }
		{ print }
	' "$ONED.haify-bak" >"$ONED"
	cat >>"$ONED" <<'EOF'

# Haify (deploy/opennebula): every image is a replicated Haify resource.
# Disks are block devices linked into the VM directory, which is kept on each
# host like the ssh driver keeps it.
TM_MAD_CONF = [
    NAME = "haify", LN_TARGET = "NONE", CLONE_TARGET = "SELF", SHARED = "NO",
    DRIVER = "raw", DISK_TYPE = "BLOCK", ALLOW_ORPHANS = "YES", DS_MIGRATE = "NO",
    DS_LIVE_MIGRATE = "NO", DS_MIGRATE_SNAP = "NO"
]

DS_MAD_CONF = [
    NAME = "haify", REQUIRED_ATTRS = "HAIFY_POOL", PERSISTENT_ONLY = "NO"
]
EOF
	echo "registered the haify drivers in $ONED (previous copy: $ONED.haify-bak)"
fi

# A running guest's disks are snapshotted by the driver (snap_create_live):
# a Haify snapshot is taken under a running guest like an LVM one.
EXECRC=/etc/one/vmm_exec/vmm_execrc
if [ -f "$EXECRC" ] && ! grep -q '^LIVE_DISK_SNAPSHOTS=.*kvm-haify' "$EXECRC"; then
	sed -i 's/^LIVE_DISK_SNAPSHOTS="\(.*\)"/LIVE_DISK_SNAPSHOTS="\1 kvm-haify qemu-haify"/' "$EXECRC"
	echo "allowed live disk snapshots on haify disks in $EXECRC"
fi

if [ -n "$restart" ]; then
	systemctl restart opennebula
	echo "restarted opennebula"
fi
echo "now run, as oneadmin: onehost sync -f"
