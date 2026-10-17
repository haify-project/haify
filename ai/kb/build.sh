#!/usr/bin/env bash
# Builds the Haify shared knowledge base: the knowledge that is the same on every
# cluster Haify runs on. One file, versioned with the code, installed on every
# node and attached to haify-ai read-only (HAIFY_AI_SHARED_KNOWLEDGE_DB). What a
# cluster learns about itself — incident notes, what was tried on which machine
# — goes into that cluster's own knowledge base, never into this one.
#
# Contents:
#   - Haify documentation (docs/, README) and the operations runbooks
#   - the Haify code graph (.understand-anything/knowledge-graph.json, not in the
#     repository: generate it with Understand-Anything first, or the step is
#     skipped) and source
#   - the haify reference, generated from the binary this commit builds, so
#     it can never describe flags the installed CLI does not have
#   - the service-ip OCF agent's code graph, when its checkout is present
#   - DRBD 9 documentation: the LINBIT knowledge base, the DRBD 9 user's guide
#     (English) and the AI-assistant notes
#
# Writes <out-dir>/haify-kb.{db,json}, and the public edition, which leaves out
# what may not be redistributed, in <out-dir>/public/ with its NOTICE.
#
# Every cluster must query it with the embedder it was built with; the model
# and dimension are recorded in the manifest next to the database.
#
# Usage: ai/kb/build.sh [out-dir]          (default: dist/kb)
#
# Every document and step is retried, and recorded in <out-dir>/progress when
# it succeeds: a build takes hours, and the embedder behind it answers 503 now
# and then. HAIFY_KB_RESUME=1 keeps the
# database and the record and carries on where the last run stopped; without
# it the build starts from nothing.
#
# HAIFY_KB_FROM=<previous haify-kb.db> starts from a previous build instead:
# everything Haify's own (its docs, runbooks, code graph, source, CLI and API)
# is dropped and rebuilt, and the DRBD documentation, which does not change
# with Haify, is kept, so a release does not spend hours re-extracting the
# LINBIT knowledge base. The previous build must have been made with the same
# embedder; the manifest beside it says which.
#
# Environment:
#   STEWARD_EMB_BASE_URL / _API_KEY / _MODEL, HAIFY_KB_EMB_DIM   embedder
#   STEWARD_LLM_BASE_URL / _API_KEY / _MODEL                   entity extraction
#   HAIFY_KB_CORPUS     dir holding linbit-blog-kb/, linbit-documentation/, ai-assistants/
#   SERVICE_IP_REPO   service-ip checkout (optional)
#   HAIFY_KB_FROM     a previous haify-kb.db to start from (optional, see above)
#   STEWARD           steward binary (default: installed at the pinned version)
set -euo pipefail

root=$(cd "$(dirname "$0")/../.." && pwd)
out=${1:-$root/dist/kb}
corpus=${HAIFY_KB_CORPUS:?set HAIFY_KB_CORPUS to the dir with linbit-blog-kb/, linbit-documentation/, ai-assistants/}
: "${STEWARD_EMB_API_KEY:?}" "${STEWARD_EMB_MODEL:?}" "${STEWARD_EMB_BASE_URL:?}"
: "${STEWARD_LLM_API_KEY:?}" "${STEWARD_LLM_MODEL:?}" "${STEWARD_LLM_BASE_URL:?}"
export STEWARD_EMB_DIM=${HAIFY_KB_EMB_DIM:-768}
steward_version=v0.54.0 # keep in step with cmd/haify-ai/go.mod
od=${STEWARD:-}
if [ -z "$od" ]; then
	od=$(mktemp -d)/steward
	GOBIN=$(dirname "$od") GOPROXY=https://proxy.golang.org,direct GOSUMDB=sum.golang.org \
		go install "github.com/liliang-cn/steward/cmd/steward@$steward_version"
fi

version=$(git -C "$root" describe --tags --always --dirty)
db=$out/haify-kb.db
stage=$(mktemp -d)
trap 'rm -rf "$stage"' EXIT
mkdir -p "$out"
progress=$out/progress
if [ "${HAIFY_KB_RESUME:-0}" != 1 ]; then
	rm -f "$db" "$db"-wal "$db"-shm "$progress"
fi
touch "$progress"
export STEWARD_KNOWLEDGE_DB_PATH=$db
export STEWARD_DOMAIN_FILE=$root/ai/domain.toml

if [ -n "${HAIFY_KB_FROM:-}" ] && [ ! -s "$db" ]; then
	printf '\n==> starting from %s\n' "$HAIFY_KB_FROM"
	sqlite3 "$HAIFY_KB_FROM" ".backup '$db'"
	# Every source Haify itself produced, under its current and its former
	# (sds-) names; what remains is the DRBD documentation.
	for p in haify-docs__ haify-runbooks__ sds-docs__ sds-runbooks__ ua: haify- sds- api: corpus/ sdssrc/; do
		"$od" purge --prefix "$p"
	done
fi

# flatten <src-dir> <prefix> <find-args...>: copies matching files into one
# staging dir as *.md, naming each after its path so document ids stay unique
# and say where the text came from.
flatten() {
	local src=$1 prefix=$2 dir
	shift 2
	dir=$stage/$prefix
	mkdir -p "$dir"
	(cd "$src" && find . -type f "$@") | while read -r f; do
		f=${f#./}
		cp "$src/$f" "$dir/${prefix}__$(echo "${f%.*}" | tr '/' '_').md"
	done
	echo "$dir"
}

step() { printf '\n==> %s\n' "$*"; }

# once <key> <cmd...>: runs cmd unless key is recorded as done, retrying a
# failure with backoff, and records key when it succeeds.
once() {
	local key=$1 try
	shift
	grep -qxF "$key" "$progress" && return 0
	for try in 1 2 3 4 5; do
		if "$@"; then
			echo "$key" >>"$progress"
			return 0
		fi
		echo "   $key failed (attempt $try); retrying in $((try * 20))s" >&2
		sleep $((try * 20))
	done
	echo "giving up on $key; rerun with HAIFY_KB_RESUME=1" >&2
	return 1
}

# ingest_one <file>: ingests one document on its own, so a failure costs that
# document rather than the directory it came from. A document a previous build
# (HAIFY_KB_FROM) already holds is kept as it is.
ingest_one() {
	local f=$1 one id held
	id=$(basename "$f" .md)
	# Waits out the other jobs' writes: a locked database answered nothing,
	# nothing was not 0, and two documents were taken as held and never built.
	held=$(sqlite3 -cmd '.timeout 60000' "$db" "SELECT count(*) FROM embeddings WHERE json_extract(metadata,'\$.document_id') = '$id' LIMIT 1" 2>/dev/null || true)
	if [ -n "${HAIFY_KB_FROM:-}" ] && [ "${held:-0}" -gt 0 ] 2>/dev/null; then
		grep -qxF "doc:$(basename "$f")" "$progress" || echo "doc:$(basename "$f")" >>"$progress"
		return 0
	fi
	one=$stage/one/$id
	mkdir -p "$one"
	cp "$f" "$one/"
	once "doc:$(basename "$f")" "$od" ingest "$one"
	rm -rf "$one"
}

# ingest_each <dir>: ingests a flattened dir, HAIFY_KB_JOBS documents at a time
# (default 4). Almost all of a document's time is spent waiting on the
# extraction model, so documents in parallel is what makes the build take
# minutes per hundred documents rather than hours; SQLite serialises the
# brief writes between them. Waits on the oldest job when all slots are busy,
# which bash 3.2 (the macOS /bin/bash) can do and `wait -n` cannot.
ingest_each() {
	local f jobs=${HAIFY_KB_JOBS:-4} failed=0
	local pids=()
	for f in "$1"/*; do
		ingest_one "$f" &
		pids+=($!)
		if [ ${#pids[@]} -ge "$jobs" ]; then
			wait "${pids[0]}" || failed=1
			pids=(${pids[@]+"${pids[@]:1}"})
		fi
	done
	for f in ${pids[@]+"${pids[@]}"}; do
		wait "$f" || failed=1
	done
	return $failed
}

step "Haify documentation"
docs=$(flatten "$root/docs" haify-docs -name '*.md')
cp "$root/README.md" "$docs/haify-docs__README.md"
ingest_each "$docs"

# The operations runbooks the MCP server serves to agents: the same text, so
# the Copilot answers a procedure question with the procedure it would be given.
step "Haify runbooks"
ingest_each "$(flatten "$root/pkg/mcpserver/runbooks" haify-runbooks -name '*.md')"

code_graph=0
if [ -f "$root/.understand-anything/knowledge-graph.json" ]; then
	step "Haify code graph"
	once step:haify-code-graph "$od" import-graph --repo haify "$root/.understand-anything/knowledge-graph.json"
	code_graph=1
fi

# The strings an operator sees in a log, mined from the Go source so an error
# line leads back to the code that printed it. Only the source is staged: given
# the checkout, ingest-repo found the code graph, imported it a second time and
# stopped there, and no string was ever mined. The strings are found by their
# vectors and words; no entities are extracted from them (the code graph holds
# the code's), which would cost one model call per file, one after another.
step "Haify source"
src=$stage/src/haify
mkdir -p "$src"
(cd "$root" && git ls-files '*.go' | grep -vE '_test\.go$|\.pb(\.gw)?\.go$' | while read -r f; do
	mkdir -p "$src/$(dirname "$f")"
	cp "$f" "$src/$f"
done)
once step:haify-source env STEWARD_LLM_API_KEY= "$od" ingest-repo "$src"

# The command line as a reference and a graph: each command linked to the
# function in cmd/cli that handles it, when the code graph is there.
step "haify reference (built from this commit)"
cli=$stage/haify
(cd "$root" && go build -o "$cli" ./cmd/cli)
link_cli=()
[ $code_graph = 1 ] && link_cli=(--repo haify --code-path cmd/cli/)
once step:haify "$od" ingest-cli ${link_cli[@]+"${link_cli[@]}"} "$cli"

# The REST API: every operation, what it takes and returns, the controller
# method serving it and the command that calls it. The gRPC methods are in
# pkg/controller/server*.go, and the names recur in the package's other files,
# so that prefix is tried before the package.
step "Haify REST API"
link_api=(--cli haify --name haify)
[ $code_graph = 1 ] && link_api+=(--repo haify --code-path pkg/controller/server,pkg/controller/)
once step:haify-api "$od" ingest-api "${link_api[@]}" "$root/haify.swagger.json"

if [ -n "${SERVICE_IP_REPO:-}" ] && [ -f "$SERVICE_IP_REPO/.understand-anything/knowledge-graph.json" ]; then
	step "service-ip code graph"
	once step:service-ip-graph "$od" import-graph "$SERVICE_IP_REPO/.understand-anything/knowledge-graph.json"
fi

step "DRBD 9 documentation"
ingest_each "$(flatten "$corpus/linbit-blog-kb" linbit-kb -name '*.md')"
# DRBD 9 only: the 8.4 guide describes behaviour Haify does not have.
ingest_each "$(flatten "$corpus/linbit-documentation/UG9/en" drbd9-guide -name '*.adoc')"
ingest_each "$(flatten "$corpus/ai-assistants" drbd-ai-notes -name '*.md')"

# manifest <db> <edition>: the manifest that goes next to db.
manifest() {
	cat <<EOF
{
  "version": "$version",
  "edition": "$2",
  "built": "$(date -u +%Y-%m-%dT%H:%M:%SZ)",
  "sha256": "$(shasum -a 256 "$1" | cut -d' ' -f1)",
  "embedder": {"model": "$STEWARD_EMB_MODEL", "dim": $STEWARD_EMB_DIM},
  "extractor": {"model": "$STEWARD_LLM_MODEL"},
  "steward": "$steward_version"
}
EOF
}

step "manifest"
sqlite3 "$db" 'PRAGMA wal_checkpoint(TRUNCATE); VACUUM;'
manifest "$db" full >"$out/haify-kb.json"
cat "$out/haify-kb.json"

# The public edition, published with every GitHub release: the same base
# without what may not be passed on, the LINBIT blog and knowledge-base
# articles and the notes from LINBIT's internal ai-assistants repository. The
# DRBD 9 user's guide stays; it is CC BY-SA 3.0, credited in NOTICE.
step "public edition"
pub=$out/public
rm -rf "$pub"
mkdir -p "$pub"
sqlite3 "$db" ".backup '$pub/haify-kb.db'"
for p in linbit-kb__ drbd-ai-notes__; do
	STEWARD_KNOWLEDGE_DB_PATH=$pub/haify-kb.db "$od" purge --prefix "$p"
done
left=$(sqlite3 "$pub/haify-kb.db" "SELECT count(*) FROM embeddings WHERE json_extract(metadata,'\$.document_id') LIKE 'linbit-kb\_\_%' ESCAPE '\\' OR json_extract(metadata,'\$.document_id') LIKE 'drbd-ai-notes\_\_%' ESCAPE '\\'")
if [ "$left" != 0 ]; then
	echo "public edition still holds $left passages it may not" >&2
	exit 1
fi
# The change history still holds every node and edge the purge removed, and
# nothing that reads the base uses it. The vector index snapshot is dropped
# with it; it is rebuilt from the vectors when it is missing.
sqlite3 "$pub/haify-kb.db" "DELETE FROM change_log; DELETE FROM graph_node_history; DELETE FROM graph_edge_history;
	DELETE FROM index_snapshots;
	INSERT INTO chunks_fts(chunks_fts) VALUES('optimize'); INSERT INTO chunks_fts_cjk(chunks_fts_cjk) VALUES('optimize');
	PRAGMA wal_checkpoint(TRUNCATE); VACUUM;"
manifest "$pub/haify-kb.db" public >"$pub/haify-kb.json"
cp "$root/ai/kb/NOTICE" "$pub/"
cat "$pub/haify-kb.json"
