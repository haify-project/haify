#!/usr/bin/env bash
# Build the bundle the installer runs from:
#   <out>/haify-bundle-linux-amd64.tar.gz  (and .sha256)
# holding haify-bundle/{bin,configs,deploy,docs}. deploy/install/get.sh
# downloads it and runs haify-bundle/deploy/install/haify-install.sh.
#
# Usage: scripts/build-bundle.sh <out dir> <version>
# The web UI must already be in ui/dist (make ui-sync, or the release job).
set -euo pipefail

out=${1:?usage: $0 <out dir> <version>}
version=${2:?usage: $0 <out dir> <version>}
root=$(cd "$(dirname "$0")/.." && pwd)
stage=$(mktemp -d)
trap 'rm -rf "$stage"' EXIT
b="$stage/haify-bundle"
mkdir -p "$b/bin" "$b/docs" "$out"

export GOOS=linux GOARCH=amd64 CGO_ENABLED=0
cd "$root"
go build -ldflags "-s -w -X main.version=$version" -o "$b/bin/haify-controller" ./cmd/controller
go build -ldflags "-s -w -X main.version=$version" -o "$b/bin/haify-mcp" ./cmd/mcp
for c in cli:haify haify-installer:haify-installer service-ip:service-ip haify-csi:haify-csi haify-docker:haify-docker haify-opennebula:haify-opennebula; do
	go build -ldflags "-s -w" -o "$b/bin/${c#*:}" "./cmd/${c%%:*}"
done

cp -R configs "$b/configs"
# The integrations the installer applies, without their tests and caches.
mkdir -p "$b/deploy"
for d in install proxmox xcp-ng libvirt opennebula docker helm k8s nomad openstack; do
	cp -R "deploy/$d" "$b/deploy/$d"
done
find "$b/deploy" \( -name __pycache__ -o -name tests -o -name 't' -o -name 'test*.sh' -o -name 'test_*.py' -o -name 'stub-ssh.sh' \) -prune -exec rm -rf {} +
cp docs/harvester.md docs/node-prerequisites.md "$b/docs/"
printf '%s\n' "$version" > "$b/VERSION"

# No macOS extended attributes in the archive: Linux tar warns on each.
COPYFILE_DISABLE=1 tar --no-xattrs -C "$stage" -czf "$out/haify-bundle-linux-amd64.tar.gz" haify-bundle
(cd "$out" && sha256sum haify-bundle-linux-amd64.tar.gz > haify-bundle-linux-amd64.tar.gz.sha256)
cp deploy/install/get.sh "$out/install.sh"
echo "$out/haify-bundle-linux-amd64.tar.gz"
