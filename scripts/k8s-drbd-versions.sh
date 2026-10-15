#!/bin/sh
# Print the DRBD kernel module each Kubernetes node has loaded, as seen by the
# Haify CSI node plugin running there.
#
#   scripts/k8s-drbd-versions.sh [namespace] [min-version]
#
# namespace defaults to kube-system and min-version to 9.2.0. A node whose
# plugin is not running yet (it waits for DRBD when the chart's loader is on)
# is reported with the loader's last log line instead. Exits 1 if any node is
# below min-version or could not be checked.
set -u

NS=${1:-kube-system}
MIN=${2:-9.2.0}
rc=0

pods=$(kubectl -n "$NS" get pods -l app=haify-csi-node \
  -o jsonpath='{range .items[*]}{.metadata.name}{" "}{.spec.nodeName}{" "}{.status.phase}{"\n"}{end}') || exit 1
if [ -z "$pods" ]; then
  echo "no haify-csi-node pods in namespace $NS" >&2
  exit 1
fi

while read -r pod node phase; do
  [ -n "$pod" ] || continue
  if out=$(kubectl -n "$NS" exec "$pod" -c plugin -- \
      /usr/local/bin/haify-csi drbd-module check --min-version="$MIN" </dev/null 2>&1); then
    printf '%-24s ok    %s\n' "$node" "$out"
    continue
  fi
  # The plugin container is not running (still waiting for DRBD), or DRBD is
  # too old. Show what the loader on that node last said.
  loader=$(kubectl -n "$NS" get pods -l app=haify-drbd-loader \
    --field-selector "spec.nodeName=$node" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)
  if [ -n "$loader" ]; then
    last=$(kubectl -n "$NS" logs "$loader" --all-containers --tail=1 2>/dev/null | tail -n 1)
    printf '%-24s FAIL  %s (plugin %s; loader %s: %s)\n' "$node" "$out" "$phase" "$loader" "$last"
  else
    printf '%-24s FAIL  %s (plugin %s; no loader pod on this node)\n' "$node" "$out" "$phase"
  fi
  rc=1
done <<EOF
$pods
EOF
exit $rc
