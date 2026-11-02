#!/bin/sh
# Download the Haify installer and run it. Published with each release as
# install.sh:
#
#   curl -fsSL https://github.com/haify-project/haify/releases/latest/download/install.sh | sudo bash
#   curl -fsSL .../install.sh | sudo bash -s -- --env libvirt --nodes kvm1=10.0.0.11,kvm2=10.0.0.12 --dry-run
#
# Without arguments it starts the web wizard and prints the address to open.
# With arguments (or --cli) it asks in the terminal; the arguments go to
# haify-install.sh (--help lists them).
# HAIFY_VERSION=v1.18.0 picks a release instead of the latest;
# HAIFY_DOWNLOAD_BASE=<url> downloads the bundle from a mirror instead.
set -eu

version=${HAIFY_VERSION:-latest}
repo=https://github.com/haify-project/haify/releases
case "$version" in
	latest) base="$repo/latest/download" ;;
	*) base="$repo/download/$version" ;;
esac
base=${HAIFY_DOWNLOAD_BASE:-$base}
case "$(uname -m)" in
	x86_64 | amd64) ;;
	*) echo "the installer bundle is built for x86_64; this machine is $(uname -m)" >&2; exit 1 ;;
esac
[ "$(id -u)" = 0 ] || { echo "run as root (sudo bash)" >&2; exit 1; }

dir=/opt/haify/installer
mkdir -p "$dir"
cd "$dir"
fetch() {
	if command -v curl >/dev/null; then curl -fsSL -o "$2" "$1"; else wget -qO "$2" "$1"; fi
}
echo "Downloading the Haify installer ($version)..."
fetch "$base/haify-bundle-linux-amd64.tar.gz" haify-bundle-linux-amd64.tar.gz
fetch "$base/haify-bundle-linux-amd64.tar.gz.sha256" haify-bundle-linux-amd64.tar.gz.sha256
sha256sum -c --quiet haify-bundle-linux-amd64.tar.gz.sha256
rm -rf haify-bundle
tar -xzf haify-bundle-linux-amd64.tar.gz

# With no options it opens the web wizard and prints its address; options,
# or --cli, run the installer in this terminal instead.
if [ $# -eq 0 ]; then
	exec "$dir/haify-bundle/bin/haify-installer"
fi
[ "${1:-}" = --cli ] && shift

# Piped into bash, this script's stdin is the pipe; the installer's
# questions are read from the terminal instead.
if [ -t 1 ] && [ -r /dev/tty ]; then
	exec bash "$dir/haify-bundle/deploy/install/haify-install.sh" "$@" < /dev/tty
fi
exec bash "$dir/haify-bundle/deploy/install/haify-install.sh" "$@"
