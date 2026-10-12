#!/usr/bin/env bash
# Install Prometheus node_exporter on Haify nodes, for memory, CPU, load, disk
# I/O and network per node. Haify itself exports cluster and DRBD state; the
# controller's metrics listener hands Prometheus these nodes as targets at
# /discovery/node-exporter (deploy/monitoring/README.md).
#
# Usage: scripts/install-node-exporter.sh node1,node2,... [--port PORT]
#
# Uses the distribution's package (prometheus-node-exporter on Debian/Ubuntu,
# node_exporter from EPEL on EL) and moves it to PORT (default 39117, the
# controller's [metrics] node_exporter_port) with a systemd drop-in.

set -euo pipefail

HOSTS=${1:?usage: $0 host1,host2,... [--port PORT]}
shift
PORT=39117
while [ $# -gt 0 ]; do
    case $1 in
        --port) PORT="$2"; shift 2 ;;
        *) echo "unknown argument: $1" >&2; exit 1 ;;
    esac
done

remote_script=$(cat <<SCRIPT
set -e
if command -v apt-get >/dev/null; then
    NEEDRESTART_SUSPEND=1 DEBIAN_FRONTEND=noninteractive apt-get install -y -qq prometheus-node-exporter >/dev/null 2>&1
    unit=prometheus-node-exporter
elif command -v dnf >/dev/null; then
    dnf install -y -q node_exporter >/dev/null 2>&1 || dnf install -y -q golang-github-prometheus-node-exporter >/dev/null
    unit=node_exporter
else
    echo "no apt-get or dnf on this node" >&2; exit 1
fi
bin=\$(systemctl show -p ExecStart --value \$unit | sed -n 's/.*path=\([^ ;]*\).*/\1/p')
[ -n "\$bin" ] || { echo "cannot find \$unit's binary" >&2; exit 1; }
mkdir -p /etc/systemd/system/\$unit.service.d
printf '[Service]\nExecStart=\nExecStart=%s --web.listen-address=:$PORT \$ARGS\n' "\$bin" > /etc/systemd/system/\$unit.service.d/10-haify-port.conf
systemctl daemon-reload
systemctl enable -q \$unit
systemctl restart \$unit
for i in 1 2 3 4 5 6 7 8 9 10; do
    if curl -fs -o /dev/null http://127.0.0.1:$PORT/metrics; then echo "\$(hostname): node_exporter on :$PORT"; exit 0; fi
    sleep 1
done
echo "\$(hostname): node_exporter did not answer on :$PORT" >&2; exit 1
SCRIPT
)

failed=0
for host in ${HOSTS//,/ }; do
    if ! ssh "$host" "sudo bash -s" <<<"$remote_script"; then
        echo "$host: install failed" >&2
        failed=1
    fi
done
exit $failed
