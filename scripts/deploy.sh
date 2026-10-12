#!/usr/bin/env bash
# Deploy Haify Controller - first time install or update.
#
# Self-HA aware: when the controller is managed by drbd-reactor (the haify-meta
# resource exists), the binary is pushed to every node but the service is only
# restarted on the active (haify-meta Primary) node -- standby nodes are left for
# reactor to manage and are NOT systemctl-enabled (that would fail on their
# DRBD dependency). Without Self-HA it enables and restarts the controller on
# EVERY given host, each with its own database: list only the controller host
# there (use --cli-only for the others).
#
# Copies configs/controller.toml.example to /etc/haify/controller.toml on hosts
# that have none; edit it afterwards.

set -e

# Associative arrays need bash 4; macOS ships 3.2 as /bin/bash.
if [ "${BASH_VERSINFO[0]}" -lt 4 ]; then
    echo "error: bash 4 or later is required (found $BASH_VERSION); on macOS: brew install bash" >&2
    exit 1
fi

# Configuration
HOSTS=""
CONTROLLER_PORT=3374
SERVICE_FILE="./configs/haify-controller.service"
CONFIG_FILE="./configs/controller.toml.example"
REMOTE_BASE="/opt/haify"
REMOTE_CONTROLLER="${REMOTE_BASE}/bin/haify-controller"
REMOTE_CLI="/usr/local/bin/haify"
REMOTE_SERVICE="/etc/systemd/system/haify-controller.service"
REMOTE_CONFIG="/etc/haify/controller.toml"

# Build target: the build host is often not what the nodes are (e.g.
# macOS/arm64), and the nodes need not all be one architecture. With --build
# each node's architecture is asked (uname -m) and a build is made for each
# one in bin/linux-<arch>/; TARGET_ARCH forces a single one. Before anything
# is copied, every host's controller binary is checked to be built for it.
TARGET_OS="${TARGET_OS:-linux}"
TARGET_ARCH="${TARGET_ARCH:-}"

# Colors
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m'

log_info() { echo -e "${GREEN}[INFO]${NC} $1"; }
log_warn() { echo -e "${YELLOW}[WARN]${NC} $1"; }
log_step() { echo -e "${BLUE}[STEP]${NC} $1"; }

# Parse args
CLI_ONLY=false
AI=0
while [[ $# -gt 0 ]]; do
    case $1 in
        --hosts) HOSTS="$2"; shift 2 ;;
        --build) BUILD=true; shift ;;
        --cli-only) CLI_ONLY=true; shift ;;
        --ai) AI=1; shift ;;
        --target-os) TARGET_OS="$2"; shift 2 ;;
        --target-arch) TARGET_ARCH="$2"; shift 2 ;;
        -h|--help)
            echo "Usage: $0 [--hosts HOST1,HOST2] [--build] [--cli-only] [--ai] [--target-os OS] [--target-arch ARCH]"
            echo ""
            echo "  --hosts HOSTS       Comma-separated hosts (required)"
            echo "  --build             Build before deploying (cross-compiles for ${TARGET_OS}/${TARGET_ARCH})"
            echo "  --cli-only          Only deploy the CLI binary"
            echo "  --ai                Build the web UI with the AI Copilot and install haify-ai + haify-mcp"
            echo "  --target-os OS      Build GOOS (default: $TARGET_OS, or \$TARGET_OS)"
            echo "  --target-arch ARCH  Build only this GOARCH (default: each host's own, or \$TARGET_ARCH)"
            exit 0
            ;;
        *) HOSTS="$1"; shift ;;
    esac
done

if [ -z "$HOSTS" ]; then
    echo "error: no hosts given; pass --hosts host1,host2" >&2
    exit 1
fi

# goarch_of maps `uname -m` to GOARCH.
goarch_of() {
    case "$1" in
        x86_64) echo amd64 ;;
        aarch64|arm64) echo arm64 ;;
        riscv64) echo riscv64 ;;
        ppc64le) echo ppc64le ;;
        s390x) echo s390x ;;
        *) echo "unknown:$1" ;;
    esac
}

# elf_goarch reads a binary's ELF e_machine (bytes 18-19).
elf_goarch() {
    case "$(od -An -tx1 -j18 -N2 "$1" 2>/dev/null | tr -d ' \n')" in
        3e00) echo amd64 ;;
        b700) echo arm64 ;;
        f300) echo riscv64 ;;
        1500) echo ppc64le ;;
        1600) echo s390x ;;
        *) echo unknown ;;
    esac
}

declare -A HOST_ARCH
for host in ${HOSTS//,/ }; do
    HOST_ARCH[$host]=$(goarch_of "$(ssh "$host" uname -m)")
done

# Build (cross-compiled; `make build` force-syncs the web UI into ui/dist via
# ui-sync, so the embedded UI is always fresh), one build per architecture.
if [ "$BUILD" = true ]; then
    archs="$TARGET_ARCH"
    [ -n "$archs" ] || archs=$(printf '%s\n' "${HOST_ARCH[@]}" | sort -u)
    for arch in $archs; do
        log_step "Building binaries for ${TARGET_OS}/${arch}..."
        GOOS="$TARGET_OS" GOARCH="$arch" CGO_ENABLED=0 make build AI="$AI" 2>&1 | tail -3
        mkdir -p "bin/${TARGET_OS}-${arch}"
        cp bin/haify-controller bin/haify "bin/${TARGET_OS}-${arch}/"
        if [ "$AI" = 1 ]; then cp bin/haify-ai bin/haify-mcp "bin/${TARGET_OS}-${arch}/"; fi
    done
fi

# bin_dir is where host's binaries come from: its architecture's build when
# there is one, else ./bin.
bin_dir() {
    if [ -d "bin/${TARGET_OS}-${HOST_ARCH[$1]}" ]; then echo "bin/${TARGET_OS}-${HOST_ARCH[$1]}"; else echo bin; fi
}

# Refuse before copying anything: a binary of the wrong architecture installs
# fine and then fails to start, under Self-HA at the worst moment.
for host in ${HOSTS//,/ }; do
    dir=$(bin_dir "$host")
    for b in haify-controller haify $([ "$AI" = 1 ] && echo haify-ai haify-mcp); do
        [ "$CLI_ONLY" = true ] && [ "$b" != haify ] && continue
        got=$(elf_goarch "$dir/$b")
        if [ "$got" != "${HOST_ARCH[$host]}" ]; then
            echo "error: $dir/$b is built for $got but $host is ${HOST_ARCH[$host]}; run with --build" >&2
            exit 1
        fi
    done
done

log_info "=========================================="
log_info "Deploying to: $HOSTS (CLI_ONLY: $CLI_ONLY)"
log_info "=========================================="

# Phase 1: push binaries / service / config to every host (no service start).
for host in ${HOSTS//,/ }; do
    log_step "Copying to $host..."

    if [ "$CLI_ONLY" = false ]; then
        ssh "$host" "sudo mkdir -p /etc/haify /opt/haify/bin /var/log/haify /var/lib/haify"
        scp -q "$(bin_dir "$host")/haify-controller" "$host:/tmp/haify-controller"
        # A node's unit may run the controller from elsewhere (an older install
        # under /usr/local/bin): install it there too, or the restart below
        # brings the old binary back up and the deploy silently does nothing.
        unit_bin=$(ssh "$host" "systemctl show -p ExecStart --value haify-controller 2>/dev/null" | sed -n 's/.*path=\([^ ;]*\).*/\1/p')
        ssh "$host" "sudo install -m755 /tmp/haify-controller $REMOTE_CONTROLLER"
        if [ -n "$unit_bin" ] && [ "$unit_bin" != "$REMOTE_CONTROLLER" ]; then
            log_info "$host: its unit runs $unit_bin; installing there as well"
            ssh "$host" "sudo install -m755 /tmp/haify-controller $unit_bin"
        fi
        ssh "$host" "rm -f /tmp/haify-controller"
    fi

    if [ "$AI" = 1 ] && [ "$CLI_ONLY" = false ]; then
        # Not restarted here: haify-ai runs in the haify-meta promoter, so a
        # restart is a failover. The new binary runs from the next move.
        for b in haify-ai haify-mcp; do
            scp -q "$(bin_dir "$host")/$b" "$host:/tmp/$b"
            ssh "$host" "[ ! -f /opt/haify/bin/$b ] || sudo cp -p /opt/haify/bin/$b /opt/haify/bin/$b.prev; sudo install -m755 /tmp/$b /opt/haify/bin/$b && rm -f /tmp/$b"
        done
        log_info "$host: haify-ai and haify-mcp installed in /opt/haify/bin; they take effect at the next 'haify ha evict haify-meta'"
    fi

    scp -q "$(bin_dir "$host")/haify" "$host:/tmp/haify"
    ssh "$host" "sudo install -m755 /tmp/haify $REMOTE_CLI && sudo ln -sf haify /usr/local/bin/haify-cli && rm -f /tmp/haify"

    if [ "$CLI_ONLY" = false ]; then
        # Install the unit only if absent: a node's unit may carry local
        # settings, and drbd-reactor manages the service under Self-HA.
        if [ -f "$SERVICE_FILE" ] && ! ssh "$host" "test -f $REMOTE_SERVICE"; then
            scp -q "$SERVICE_FILE" "$host:/tmp/haify-controller.service"
            ssh "$host" "sudo mv /tmp/haify-controller.service $REMOTE_SERVICE && sudo systemctl daemon-reload"
        fi
        # Copy config only if absent (never clobber an existing config).
        if [ -f "$CONFIG_FILE" ] && ! ssh "$host" "test -f $REMOTE_CONFIG"; then
            scp -q "$CONFIG_FILE" "$host:/tmp/controller.toml"
            ssh "$host" "sudo mv /tmp/controller.toml $REMOTE_CONFIG"
        fi
        # drbd-reactor auto-reload (harmless per host).
        if ssh "$host" "command -v drbd-reactor &>/dev/null"; then
            if ssh "$host" "test -f /usr/share/doc/drbd-reactor/examples/drbd-reactor-reload.path"; then
                ssh "$host" "sudo cp /usr/share/doc/drbd-reactor/examples/drbd-reactor-reload.{path,service} /etc/systemd/system/ 2>/dev/null; sudo systemctl daemon-reload; sudo systemctl enable --now drbd-reactor-reload.path" 2>/dev/null || true
            fi
        fi
    fi
done

if [ "$CLI_ONLY" = true ]; then
    log_info "✓ CLI deployed to: $HOSTS"
    exit 0
fi

# Phase 2: detect self-HA and restart appropriately.
# If haify-meta exists, the controller is reactor-managed -> restart only the
# active (Primary) node; standby nodes are driven by reactor on failover.
ACTIVE_NODE=""
SELF_HA=false
for host in ${HOSTS//,/ }; do
    role=$(ssh "$host" "sudo -n drbdadm role haify-meta 2>/dev/null" || true)
    if [ -n "$role" ]; then
        SELF_HA=true
        [ "$role" = "Primary" ] && ACTIVE_NODE="$host"
    fi
done

if [ "$SELF_HA" = true ]; then
    if [ -z "$ACTIVE_NODE" ]; then
        log_warn "Self-HA detected but no haify-meta Primary among the given hosts; not restarting."
        log_warn "Restart the active controller node manually: ssh <active> sudo systemctl restart haify-controller"
    else
        # Restarting a promoter-managed unit in place fails its dependency and
        # makes drbd-reactor fail haify-meta over anyway, unplanned. Every node
        # has the new binary now, so hand the controller over instead: evict
        # haify-meta (detached, as `haify ha evict haify-meta` does) and wait for
        # another node to run it.
        log_step "Self-HA cluster: moving the controller off $ACTIVE_NODE (evict haify-meta)..."
        ssh "$ACTIVE_NODE" "sudo systemd-run --unit=haify-selfha-evict --collect drbd-reactorctl evict haify-ha-haify-meta" >/dev/null
        NEW_ACTIVE=""
        for _ in $(seq 1 60); do
            sleep 2
            for host in ${HOSTS//,/ }; do
                [ "$host" = "$ACTIVE_NODE" ] && continue
                if [ "$(ssh "$host" "sudo -n drbdadm role haify-meta 2>/dev/null")" = "Primary" ] &&
                    ssh "$host" "systemctl is-active -q haify-controller.service"; then
                    NEW_ACTIVE="$host"
                    break 2
                fi
            done
        done
        if [ -n "$NEW_ACTIVE" ]; then
            log_info "Controller now runs on $NEW_ACTIVE with the new binary."
            ACTIVE_NODE="$NEW_ACTIVE"
        else
            log_warn "No other node took over haify-meta within 120s; check: haify ha self status"
        fi
    fi
else
    for host in ${HOSTS//,/ }; do
        log_step "Enabling + restarting controller on $host..."
        ssh "$host" "sudo systemctl enable haify-controller.service && sudo systemctl restart haify-controller.service"
    done
fi

sleep 2

log_info "=========================================="
log_info "Service Status:"
log_info "=========================================="
if [ "$SELF_HA" = true ] && [ -n "$ACTIVE_NODE" ]; then
    echo "[$ACTIVE_NODE (active)]"
    ssh "$ACTIVE_NODE" "sudo systemctl status haify-controller.service --no-pager" 2>/dev/null | head -n 8
else
    for host in ${HOSTS//,/ }; do
        echo "[$host]"
        ssh "$host" "sudo systemctl status haify-controller.service --no-pager" 2>/dev/null | head -n 8
        echo ""
    done
fi

log_info "✓ Deployment completed!"
log_info "Database: /var/lib/haify/haify.db (BoltDB)"
log_info "Logs: journalctl -u haify-controller.service -f"
log_info "Test: haify -c <HOST>:$CONTROLLER_PORT pool list"
