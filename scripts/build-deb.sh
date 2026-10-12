#!/usr/bin/env bash
# Build the Haify Debian packages into dist/ with plain dpkg-deb:
#
#   haify-controller_<version>_<arch>.deb   per architecture (amd64, arm64)
#   haify-pve-plugin_<version>_all.deb      the Proxmox VE storage plugin
#
# No debhelper, nfpm or fpm: a package here is a staged file tree, a generated
# control file and the maintainer scripts in deploy/deb/<package>/, so the only
# build tool beyond Go (and Node.js for the web UI) is dpkg-deb, which every
# Debian/Ubuntu host has.
#
#   scripts/build-deb.sh               both packages
#   scripts/build-deb.sh controller    haify-controller only
#   scripts/build-deb.sh pve-plugin    haify-pve-plugin only (needs no Go or Node)
#
# Environment:
#   VERSION          version to stamp (default: git describe --tags --always --dirty)
#   ARCHES           Debian architectures for haify-controller (default "amd64 arm64")
#   SKIP_UI_BUILD=1  embed the web UI already in ui/dist instead of rebuilding it
#   MAINTAINER       "Name <email>" for the control files (default: DEBFULLNAME and
#                    DEBEMAIL, then git config user.name/user.email)
#   OUT_DIR          output directory, relative to the repository (default dist)

set -euo pipefail
umask 022

cd "$(dirname "$0")/.."
OUT_DIR="${OUT_DIR:-dist}"
ARCHES="${ARCHES:-amd64 arm64}"
HOMEPAGE="https://github.com/haify-project/haify"
PVE_SRC="deploy/proxmox"

die() { echo "build-deb: $*" >&2; exit 1; }
log() { echo ">>> $*"; }

what="${1:-all}"
case "$what" in
    all|controller|pve-plugin) ;;
    *) die "usage: $0 [all|controller|pve-plugin]" ;;
esac

command -v dpkg-deb >/dev/null 2>&1 || die "dpkg-deb not found (Debian/Ubuntu: apt install dpkg)"

# The version the binaries report (main.version) is git's own string, as in
# the release workflow; the package version is that string made valid for
# dpkg: no leading "v", and "-" turned into "+" (a hyphen would make dpkg read
# a Debian revision). A tree with no tag behind it gets "0.0~git<commits>.<sha>",
# which sorts below any release and in commit order among such builds; testing
# for a tag rather than for a leading digit matters, as a bare abbreviated
# hash often starts with one.
tagged=1
if [ -n "${VERSION:-}" ]; then
    RAW_VERSION="$VERSION"
else
    RAW_VERSION=$(git describe --tags --always --dirty 2>/dev/null || true)
    git describe --tags --abbrev=0 >/dev/null 2>&1 || tagged=0
fi
[ -n "$RAW_VERSION" ] || die "no version: set VERSION or build from a git checkout"
DEB_VERSION="${RAW_VERSION#v}"
DEB_VERSION="${DEB_VERSION//-/+}"
case "$tagged:$DEB_VERSION" in
    1:[0-9]*) ;;
    0:*) DEB_VERSION="0.0~git$(git rev-list --count HEAD).${DEB_VERSION}" ;;
    *) DEB_VERSION="0.0~git${DEB_VERSION}" ;;
esac
[[ "$DEB_VERSION" =~ ^[0-9][A-Za-z0-9.+~]*$ ]] || die "version '$RAW_VERSION' gives an invalid package version '$DEB_VERSION'"

if [ -z "${MAINTAINER:-}" ]; then
    if [ -n "${DEBFULLNAME:-}" ] && [ -n "${DEBEMAIL:-}" ]; then
        MAINTAINER="$DEBFULLNAME <$DEBEMAIL>"
    else
        name=$(git config user.name 2>/dev/null || true)
        email=$(git config user.email 2>/dev/null || true)
        MAINTAINER="${name:-Haify local build} <${email:-root@localhost}>"
    fi
fi

# Reproducible archives: dpkg-deb clamps member timestamps to this.
if [ -z "${SOURCE_DATE_EPOCH:-}" ]; then
    SOURCE_DATE_EPOCH=$(git log -1 --format=%ct 2>/dev/null || date +%s)
fi
export SOURCE_DATE_EPOCH

STAGE=$(mktemp -d "${TMPDIR:-/tmp}/haify-deb.XXXXXX")
trap 'rm -rf "$STAGE"' EXIT
mkdir -p "$OUT_DIR"
OUT_DIR=$(cd "$OUT_DIR" && pwd)
BUILT=()

# installed_size is the control file's Installed-Size: KiB of the payload.
installed_size() {
    du -sk --exclude=DEBIAN "$1" | cut -f1
}

# copyright writes the machine-readable copyright file every package carries.
copyright() {
    cat > "$1" <<EOF
Format: https://www.debian.org/doc/packaging-manuals/copyright-format/1.0/
Upstream-Name: haify
Source: $HOMEPAGE

Files: *
License: Apache-2.0
 On Debian systems the full text of the license is in
 /usr/share/common-licenses/Apache-2.0.
EOF
}

# finish_package installs the maintainer scripts, writes the control file
# (its stanza on stdin) and builds root as out. xz, not dpkg-deb's default:
# Ubuntu's dpkg compresses with zstd, which the dpkg of older Debian and
# Proxmox VE releases cannot unpack.
finish_package() {
    local pkg="$1" root="$2" out="$3" script
    for script in postinst prerm postrm; do
        install -m 0755 "deploy/deb/$pkg/$script" "$root/DEBIAN/$script"
    done
    {
        cat
        echo "Installed-Size: $(installed_size "$root")"
    } > "$root/DEBIAN/control"
    dpkg-deb --root-owner-group -Zxz --build "$root" "$out" >/dev/null
    BUILT+=("$out")
}

# ---------------------------------------------------------------- pve-plugin

# check_helper_set fails when install.sh and the helper directory disagree:
# the package ships the directory, install.sh its HELPER_NAMES, and the two
# must install the same modules.
check_helper_set() {
    local listed present
    listed=$(sed -n 's/^HELPER_NAMES=(\(.*\))$/\1/p' "$PVE_SRC/install.sh" | tr ' ' '\n' | sort | tr '\n' ' ')
    present=$(cd "$PVE_SRC/PVE/Storage/Custom/Haify" && ls -1 -- *.pm | sort | tr '\n' ' ')
    [ -n "$listed" ] || die "HELPER_NAMES not found in $PVE_SRC/install.sh"
    [ "$listed" = "$present" ] || die "$PVE_SRC/install.sh HELPER_NAMES (${listed% }) differs from PVE/Storage/Custom/Haify (${present% })"
}

build_pve_plugin() {
    local root="$STAGE/haify-pve-plugin" custom doc share
    custom="$root/usr/share/perl5/PVE/Storage/Custom"
    doc="$root/usr/share/doc/haify-pve-plugin"
    share="$root/usr/share/haify-pve-plugin"

    check_helper_set
    # The suite stubs the PVE modules, so it runs here; install.sh's perl -c
    # needs a PVE node, and postinst repeats that check there.
    if command -v prove >/dev/null 2>&1; then
        log "testing the plugin (prove $PVE_SRC/t)"
        (cd "$PVE_SRC" && prove -Q t/) || die "plugin tests failed"
    else
        echo "build-deb: prove not found, plugin tests skipped" >&2
    fi

    install -d "$root/DEBIAN" "$custom/Haify" "$doc" "$share"
    install -m 0644 "$PVE_SRC/HaifyPlugin.pm" "$custom/HaifyPlugin.pm"
    install -m 0644 "$PVE_SRC"/PVE/Storage/Custom/Haify/*.pm "$custom/Haify/"
    install -m 0755 "$PVE_SRC/lvm-filter.sh" "$PVE_SRC/preflight.sh" "$PVE_SRC/gui/gui-patch.sh" "$share/"
    install -m 0644 "$PVE_SRC/storage.cfg.example" "$doc/"
    # The web interface's dialog for the haify type, and the apt hook that puts
    # it back after a pve-manager upgrade replaces the page template.
    install -D -m 0644 "$PVE_SRC/gui/haify-storage.js" "$root/usr/share/pve-manager/js/haify-storage.js"
    install -D -m 0644 "$PVE_SRC/gui/90haify-pve-gui" "$root/etc/apt/apt.conf.d/90haify-pve-gui"
    echo /etc/apt/apt.conf.d/90haify-pve-gui > "$root/DEBIAN/conffiles"
    # DRBD disconnects only after the guests stop, so a reboot cannot split a
    # running guest's disk (install.sh explains).
    install -D -m 0644 "$PVE_SRC/drbd-shutdown-order.conf" \
        "$root/usr/lib/systemd/system/drbd-graceful-disconnect.service.d/haify-pve.conf"
    copyright "$doc/copyright"

    finish_package haify-pve-plugin "$root" "$OUT_DIR/haify-pve-plugin_${DEB_VERSION}_all.deb" <<EOF
Package: haify-pve-plugin
Version: $DEB_VERSION
Architecture: all
Maintainer: $MAINTAINER
Depends: libpve-storage-perl, drbd-utils, lvm2
Section: admin
Priority: optional
Homepage: $HOMEPAGE
Description: Haify storage plugin for Proxmox VE
 Storage type "haify": backs Proxmox VE guest disks with DRBD resources managed
 by haify-controller, giving synchronous replication, HA restart on a surviving
 node and live migration that copies only RAM.
 .
 Installs what deploy/proxmox/install.sh installs, adds the LVM filter that
 keeps the host's LVM off DRBD devices (HAIFY_SKIP_LVM_FILTER=1 skips it),
 orders DRBD's shutdown disconnect after the guests stop, and restarts the PVE
 daemons that load storage plugins, which does not disturb running guests.
 The node also needs the DRBD 9 kernel module.
EOF
}

# ---------------------------------------------------------------- controller

# prepare_ui puts the web UI the controller embeds (go:embed, ui/ui.go) into
# ui/dist, as make build does.
prepare_ui() {
    if [ "${SKIP_UI_BUILD:-0}" = "1" ]; then
        [ -f ui/dist/index.html ] || die "SKIP_UI_BUILD=1 but ui/dist/index.html is missing"
        if grep -q 'Haify UI placeholder' ui/dist/index.html; then
            echo "build-deb: WARNING: ui/dist holds the placeholder page; the packaged controller serves no web UI" >&2
        fi
        return
    fi
    [ -d web-ui/node_modules ] || die "web-ui/node_modules is missing: run (cd web-ui && npm ci), or set SKIP_UI_BUILD=1"
    log "building the web UI"
    make --no-print-directory ui-sync >/dev/null
}

# build_binaries cross-compiles the four packaged programs the way deploy.sh
# and the release workflow do: static (CGO_ENABLED=0) linux binaries.
build_binaries() {
    local arch="$1" out="$2" spec
    mkdir -p "$out"
    for spec in controller:haify-controller cli:haify mcp:haify-mcp service-ip:service-ip; do
        GOOS=linux GOARCH="$arch" CGO_ENABLED=0 go build -trimpath \
            -ldflags "-s -w -X main.version=${RAW_VERSION}" \
            -o "$out/${spec#*:}" "./cmd/${spec%%:*}"
    done
}

build_controller() {
    local arch="$1" root="$STAGE/haify-controller-$arch" bin="$STAGE/bin-$arch" units doc
    units="$root/lib/systemd/system"
    doc="$root/usr/share/doc/haify-controller"

    log "building haify-controller binaries for linux/$arch"
    build_binaries "$arch" "$bin"

    install -d "$root/DEBIAN" "$root/opt/haify/bin" "$root/usr/bin" "$units" "$doc" \
        "$root/usr/share/haify-controller"
    # /opt/haify/bin is where configs/haify-controller.service runs the controller
    # from, and where the controller looks for the service-ip it installs on HA
    # nodes (next to its own executable).
    install -m 0755 "$bin/haify-controller" "$bin/service-ip" "$root/opt/haify/bin/"
    # The CLI and MCP server go on the default PATH. A manual install uses
    # /usr/local/bin, which belongs to the administrator, not to packages.
    install -m 0755 "$bin/haify" "$bin/haify-mcp" "$root/usr/bin/"
    ln -s haify "$root/usr/bin/haify-cli"
    # haify-mcp-http.service and the AI Copilot run /opt/haify/bin/haify-mcp.
    ln -s /usr/bin/haify-mcp "$root/opt/haify/bin/haify-mcp"

    install -m 0644 configs/haify-controller.service configs/haify-mcp-http.service "$units/"
    # The repository's service-ip@ unit runs /usr/local/bin/service-ip, where
    # the controller installs it on nodes that lack it, together with its own
    # copy of the unit in /etc/systemd/system (which then takes precedence).
    # The packaged unit runs the packaged binary instead.
    sed 's|/usr/local/bin/service-ip|/opt/haify/bin/service-ip|g' \
        "configs/service-ip@.service" > "$units/service-ip@.service"
    chmod 0644 "$units/service-ip@.service"
    if grep -q /usr/local/bin "$units/service-ip@.service"; then
        die "service-ip@.service still names /usr/local/bin after rewriting"
    fi
    grep -q '^ExecStart=/opt/haify/bin/service-ip ' "$units/service-ip@.service" \
        || die "service-ip@.service: ExecStart not rewritten to /opt/haify/bin/service-ip"

    # postinst copies the template to /etc/haify/controller.toml when that is
    # unused; it reads this copy because /usr/share/doc may be path-excluded.
    install -m 0644 configs/controller.toml.example "$root/usr/share/haify-controller/"
    install -m 0644 configs/controller.toml.example "$doc/"
    copyright "$doc/copyright"

    finish_package haify-controller "$root" "$OUT_DIR/haify-controller_${DEB_VERSION}_${arch}.deb" <<EOF
Package: haify-controller
Version: $DEB_VERSION
Architecture: $arch
Maintainer: $MAINTAINER
Recommends: drbd-utils, drbd-reactor
Section: admin
Priority: optional
Homepage: $HOMEPAGE
Description: DRBD-based software-defined storage controller
 haify-controller manages storage pools, replicated DRBD resources, snapshots,
 backups, NFS/iSCSI/NVMe-oF gateways and HA on a set of Linux storage nodes,
 driving them over SSH. This package carries the controller (with its web UI)
 and the service-ip helper in /opt/haify/bin, the haify command-line client, the
 haify-mcp MCP server and their systemd units.
 .
 The controller is installed but never enabled or started: one host per
 cluster runs it, or drbd-reactor does under Self-HA.
EOF
}

# ---------------------------------------------------------------- main

log "version $DEB_VERSION (binaries report $RAW_VERSION)"
if [ "$what" != "controller" ]; then
    build_pve_plugin
fi
if [ "$what" != "pve-plugin" ]; then
    command -v go >/dev/null 2>&1 || die "go not found"
    prepare_ui
    for arch in $ARCHES; do
        case "$arch" in
            amd64|arm64) ;;
            *) die "unsupported architecture '$arch' (amd64, arm64)" ;;
        esac
        build_controller "$arch"
    done
fi
log "built:"
printf '    %s\n' "${BUILT[@]}"
