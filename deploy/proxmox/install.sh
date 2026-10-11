#!/bin/bash
# Install the Haify storage plugin on this Proxmox VE node.
#
# The plugin is a Perl module plus its helpers under Haify/, with no dependencies
# beyond what PVE already ships, so installing is a copy plus a daemon reload.
#
#   ./install.sh            install/upgrade, then restart the PVE daemons
#   ./install.sh --uninstall remove the plugin
#
# Run it on every PVE node in the cluster: pvedaemon loads storage plugins
# in-process, so a node without the module cannot use the storage.

set -euo pipefail

PLUGIN_DIR=/usr/share/perl5/PVE/Storage/Custom
PLUGIN_NAME=HaifyPlugin.pm
SRC_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# Helper modules live in a subdirectory: PVE loads every *.pm directly under
# Custom/ as a storage plugin, so they must not sit next to HaifyPlugin.pm.
HELPER_DIR="$PLUGIN_DIR/Haify"
HELPER_SRC_DIR="$SRC_DIR/PVE/Storage/Custom/Haify"
HELPER_NAMES=(Activation.pm Api.pm Capacity.pm Client.pm Inventory.pm Migration.pm Naming.pm Snapshots.pm Templates.pm Token.pm)

# The web interface's dialog for the haify storage type (gui/), and what keeps
# it loaded across pve-manager upgrades.
GUI_JS=/usr/share/pve-manager/js/haify-storage.js
GUI_PATCH=/usr/share/haify-pve-plugin/gui-patch.sh
APT_HOOK=/etc/apt/apt.conf.d/90haify-pve-gui

if [ "$(id -u)" -ne 0 ]; then
    echo "install.sh must run as root" >&2
    exit 1
fi

# Every PVE daemon that loads storage plugins in-process. Each must restart to
# see a new or changed module: one that does not answers "unsupported type
# 'haify'". The HA local resource manager is the one that matters most — before
# it was on this list, the first node failure after an install left HA unable
# to start the guest anywhere. Restarting them does not disturb running guests;
# PVE's own upgrades restart the same set.
PVE_DAEMONS="pvedaemon pveproxy pvestatd pvescheduler pve-ha-crm pve-ha-lrm"

# drbd-utils' drbd-graceful-disconnect.service disconnects every resource when
# the network goes down, which on shutdown is before PVE has stopped its guests.
# A VM still running on the node then keeps writing to a resource whose peers
# were just told they are Outdated, and when HA starts it elsewhere the two
# copies split. Ordering the disconnect after the guests stop closes that.
DRBD_ORDER=/etc/systemd/system/drbd-graceful-disconnect.service.d/haify-pve.conf

order_drbd_shutdown() {
    mkdir -p "$(dirname "$DRBD_ORDER")"
    cat >"$DRBD_ORDER" <<'UNIT'
# Installed by the Haify PVE plugin: disconnect DRBD only after the guests stop.
[Unit]
Before=pve-guests.service pve-ha-lrm.service
UNIT
    systemctl daemon-reload
}

reload_pve() {
    # try-restart: a daemon that is not running (HA on a node outside any HA
    # group never starts its LRM) is left alone.
    # shellcheck disable=SC2086 # the list is meant to split
    systemctl try-restart $PVE_DAEMONS
}

if [ "${1:-}" = "--uninstall" ]; then
    rm -f "$PLUGIN_DIR/$PLUGIN_NAME"
    echo "Removed $PLUGIN_DIR/$PLUGIN_NAME"
    for name in "${HELPER_NAMES[@]}"; do
        rm -f "$HELPER_DIR/$name"
        echo "Removed $HELPER_DIR/$name"
    done
    rmdir "$HELPER_DIR" 2>/dev/null || true
    [ -x "$GUI_PATCH" ] && "$GUI_PATCH" --remove
    rm -f "$GUI_JS" "$GUI_PATCH" "$APT_HOOK" "$DRBD_ORDER"
    systemctl daemon-reload
    echo "Removed the Haify storage dialog from the web interface"
    echo "Remove any 'haify:' entries from /etc/pve/storage.cfg before reloading."
    reload_pve
    exit 0
fi

if [ ! -f "$SRC_DIR/$PLUGIN_NAME" ]; then
    echo "$PLUGIN_NAME not found next to install.sh" >&2
    exit 1
fi
for name in "${HELPER_NAMES[@]}"; do
    if [ ! -f "$HELPER_SRC_DIR/$name" ]; then
        echo "$HELPER_SRC_DIR/$name not found" >&2
        exit 1
    fi
done

# Fail before touching the system if the module does not even compile. Without
# this a syntax error takes pvedaemon down with it on restart. -I makes the
# check use the helpers being installed, not ones already on the node.
if ! perl -I "$SRC_DIR" -c "$SRC_DIR/$PLUGIN_NAME" >/dev/null 2>&1; then
    echo "Refusing to install: $PLUGIN_NAME does not compile on this node:" >&2
    perl -I "$SRC_DIR" -c "$SRC_DIR/$PLUGIN_NAME" >&2 || true
    exit 1
fi

mkdir -p "$PLUGIN_DIR" "$HELPER_DIR"
for name in "${HELPER_NAMES[@]}"; do
    install -m 0644 "$HELPER_SRC_DIR/$name" "$HELPER_DIR/$name"
    echo "Installed $HELPER_DIR/$name"
done
install -m 0644 "$SRC_DIR/$PLUGIN_NAME" "$PLUGIN_DIR/$PLUGIN_NAME"
echo "Installed $PLUGIN_DIR/$PLUGIN_NAME"

install -D -m 0644 "$SRC_DIR/gui/haify-storage.js" "$GUI_JS"
install -D -m 0755 "$SRC_DIR/gui/gui-patch.sh" "$GUI_PATCH"
install -m 0644 "$SRC_DIR/gui/90haify-pve-gui" "$APT_HOOK"
"$GUI_PATCH"
echo "Added Haify to the web interface's storage dialogs"

# Keep the host's LVM off the DRBD devices that carry guest disks (see
# lvm-filter.sh). HAIFY_SKIP_LVM_FILTER=1 leaves lvm.conf to you.
if [ "${HAIFY_SKIP_LVM_FILTER:-0}" != "1" ]; then
    if ! "$SRC_DIR/lvm-filter.sh"; then
        echo "WARNING: the LVM filter was not added; a guest's LVM inside its disk can be activated on this host" >&2
    fi
fi

order_drbd_shutdown
echo "DRBD disconnects only after the guests have stopped on shutdown"

reload_pve
echo "Restarted $PVE_DAEMONS"

cat <<'EOF'

Next: add the storage once for the cluster, in the web interface under
Datacenter -> Storage -> Add -> Haify, or in /etc/pve/storage.cfg, e.g.

  haify: haify0
        controller 192.168.1.10
        haifypool vg0
        replicas 2
        content images,rootdir
        shared 1

See storage.cfg.example for every option.
EOF
