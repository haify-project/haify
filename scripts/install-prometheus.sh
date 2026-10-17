#!/usr/bin/env bash
# Install the cluster's Prometheus on every node that can run the controller,
# to run with it under Self-HA: drbd-reactor starts it on the active node, its
# data is on the haify-meta volume, and it moves with the controller.
#
# Usage: scripts/install-prometheus.sh node1,node2,... [--retention-size SIZE]
#
# On each node: the distribution's prometheus package (its own service is
# masked), haify-prometheus.service, /etc/haify/prometheus.yml, and in
# /etc/haify/controller.toml haify-prometheus.service in [self_ha]
# extra_services and [metrics] prometheus_url. The running promoter is not
# touched; deploy/monitoring/README.md says how to add the service to it.

set -euo pipefail

HOSTS=${1:?usage: $0 host1,host2,... [--retention-size SIZE]}
shift
SIZE=300MB
while [ $# -gt 0 ]; do
    case $1 in
        --retention-size) SIZE="$2"; shift 2 ;;
        *) echo "unknown argument: $1" >&2; exit 1 ;;
    esac
done
here=$(cd "$(dirname "$0")/.." && pwd)
unit=$(sed "s/retention.size=300MB/retention.size=$SIZE/" "$here/configs/haify-prometheus.service" | base64 | tr -d '\n')
conf=$(base64 < "$here/configs/haify-prometheus.yml" | tr -d '\n')

remote_script=$(cat <<SCRIPT
set -e
# Judged by the binary, not the package manager's exit status: a node with
# some other package half-configured fails every install, this one included.
if command -v apt-get >/dev/null; then
    out=\$(NEEDRESTART_SUSPEND=1 DEBIAN_FRONTEND=noninteractive apt-get install -y -qq prometheus 2>&1) || true
elif command -v dnf >/dev/null; then
    out=\$(dnf install -y -q golang-github-prometheus 2>&1) || true
else
    echo "no apt-get or dnf on this node" >&2; exit 1
fi
command -v prometheus >/dev/null || { echo "\$out" | tail -5 >&2; echo "prometheus did not install" >&2; exit 1; }
# The package's own server would start at boot on every node with its own
# data; this cluster's runs only where the controller does.
systemctl disable -q --now prometheus 2>/dev/null || true
systemctl mask -q prometheus
bin=\$(command -v prometheus)
mkdir -p /etc/haify
echo $conf | base64 -d > /etc/haify/prometheus.yml
echo $unit | base64 -d | sed "s#/usr/bin/prometheus#\$bin#" > /etc/systemd/system/haify-prometheus.service
systemctl daemon-reload
python3 - /etc/haify/controller.toml <<'PY'
import re, sys
path = sys.argv[1]
lines = open(path).read().split("\n")

def section(name):
    """The header line of [name] and the line after the section, or None."""
    start = None
    for i, line in enumerate(lines):
        if start is None and line.strip() == "[" + name + "]":
            start = i
        elif start is not None and re.match(r"\s*\[", line):
            return start, i
    return (start, len(lines)) if start is not None else (None, None)

def ensure(name, key, want, edit):
    start, end = section(name)
    if start is None:
        lines.extend(["", "[" + name + "]", want])
        return
    for i in range(start + 1, end):
        if re.match(r"\s*" + key + r"\s*=", lines[i]):
            lines[i] = edit(lines[i])
            return
    lines.insert(start + 1, want)

svc = '"haify-prometheus.service"'

def add_service(line):
    m = re.match(r'\s*extra_services\s*=\s*\[(.*)\]\s*$', line)
    if m is None:
        sys.exit("extra_services in " + path + " is not a one-line list; add " + svc + " by hand")
    items = [x.strip() for x in m.group(1).split(",") if x.strip()]
    return line if svc in items else "extra_services = [" + ", ".join(items + [svc]) + "]"

url = 'prometheus_url = "http://127.0.0.1:39417"'
ensure("self_ha", "extra_services", "extra_services = [" + svc + "]", add_service)
ensure("metrics", "prometheus_url", url, lambda line: url if re.match(r'\s*prometheus_url\s*=\s*""', line) else line)
open(path, "w").write("\n".join(lines))
PY
# The controller's metrics port, which the scrape config reaches it on.
port=\$(python3 -c 'import re,sys; s=open(sys.argv[1]).read(); m=re.search(r"^\[metrics\][^\[]*?^port\s*=\s*(\d+)", s, re.M|re.S); print(m.group(1) if m else 9433)' /etc/haify/controller.toml)
sed -i "s/127.0.0.1:9433/127.0.0.1:\$port/g" /etc/haify/prometheus.yml
echo "\$(hostname): haify-prometheus.service installed, controller metrics on :\$port (\$(\$bin --version 2>&1 | head -1))"
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
