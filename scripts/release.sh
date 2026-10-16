#!/usr/bin/env bash
# Releases Haify: tags HEAD, rebuilds the shared knowledge base at that tag,
# pushes the tag, and publishes the knowledge base's public edition and the
# code graph on the GitHub release. release.yml adds the binaries to the same
# release when the tag arrives.
#
# Usage: scripts/release.sh vX.Y.Z
#
# HEAD must be origin/main with a clean tree. The build needs the environment
# ai/kb/build.sh lists; it is read from $HAIFY_KB_ENV (default
# ~/.config/haify/kb.env) when that file exists. The last full build is kept
# in $HAIFY_KB_CACHE (default ~/.cache/haify-kb), and each build starts from
# it, so the DRBD documentation is not extracted again.
#
# Release assets, also at releases/latest/download/<name>:
#   haify-kb.tar.zst          haify-kb.db, haify-kb.json and NOTICE, public edition
#   haify-code-graph.json.gz  the Understand-Anything code graph
# each with a .sha256 next to it.
set -euo pipefail

die() {
	echo "error: $*" >&2
	exit 1
}

tag=${1:-}
[[ $tag =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || die "usage: scripts/release.sh vX.Y.Z"
root=$(cd "$(dirname "$0")/.." && pwd)
cd "$root"

for t in gh zstd sqlite3 jq; do
	command -v "$t" >/dev/null || die "$t is required"
done
[ -z "$(git status --porcelain --untracked-files=no)" ] || die "the tree has uncommitted changes"
git fetch -q origin main --tags
[ "$(git rev-parse HEAD)" = "$(git rev-parse origin/main)" ] || die "HEAD is not origin/main; push main first"
if git rev-parse -q --verify "refs/tags/$tag" >/dev/null; then
	[ "$(git rev-list -n1 "$tag")" = "$(git rev-parse HEAD)" ] || die "$tag exists and is not HEAD"
else
	git tag -a "$tag" -m "$tag"
fi

graph=.understand-anything/knowledge-graph.json
[ -f "$graph" ] || die "no code graph at $graph; generate it with Understand-Anything first"
at=$(jq -r '.project.gitCommitHash // empty' "$graph")
if [ -z "$at" ] || ! git cat-file -e "$at^{commit}" 2>/dev/null; then
	echo "warning: cannot tell which commit the code graph describes" >&2
elif behind=$(git rev-list --count "$at..HEAD" -- '*.go') && [ "$behind" != 0 ]; then
	echo "warning: the code graph describes ${at:0:7}; $behind commits since then change Go code" >&2
fi

env_file=${HAIFY_KB_ENV:-$HOME/.config/haify/kb.env}
if [ -f "$env_file" ]; then
	set -a
	# shellcheck disable=SC1090
	. "$env_file"
	set +a
fi
cache=${HAIFY_KB_CACHE:-$HOME/.cache/haify-kb}
mkdir -p "$cache"
out=$root/dist/kb
if [ -s "$cache/haify-kb.db" ]; then
	HAIFY_KB_FROM=$cache/haify-kb.db ai/kb/build.sh "$out"
else
	ai/kb/build.sh "$out"
fi
[ "$(jq -r .version "$out/public/haify-kb.json")" = "$tag" ] || die "the knowledge base was not built at $tag"
cp "$out/haify-kb.db" "$out/haify-kb.json" "$cache/"

assets=$out/release
rm -rf "$assets"
mkdir -p "$assets"
tar -C "$out/public" -cf - haify-kb.db haify-kb.json NOTICE | zstd -q -19 -T0 -o "$assets/haify-kb.tar.zst"
gzip -9 -c "$graph" >"$assets/haify-code-graph.json.gz"
(cd "$assets" && for f in *; do shasum -a 256 "$f" >"$f.sha256"; done)

git push origin "$tag"
# Created here, before release.yml has built anything, so the release has its
# knowledge base from the moment it appears; the workflow finds the release
# by its tag and adds the binaries.
if ! gh release view "$tag" >/dev/null 2>&1 &&
	gh release create "$tag" --verify-tag --title "$tag" --notes "" "$assets"/*; then
	:
else
	gh release upload "$tag" --clobber "$assets"/*
fi
echo "$tag released with its knowledge base:"
gh release view "$tag" --json url,assets -q '.url, (.assets[] | "  " + .name)'
