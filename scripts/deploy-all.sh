#!/usr/bin/env bash
# Build and deploy Haify to the given hosts.
# Usage: ./scripts/deploy-all.sh node1,node2,node3 [--ai]
#
# --ai builds the web UI with the AI Copilot and installs haify-ai and
# haify-mcp too; without it neither is built or deployed.
#
# Cross-compiles for each node's own architecture (TARGET_ARCH forces one),
# checks every node gets a binary built for it, and is self-HA aware (see
# deploy.sh).

set -e

HOSTS=${1:?usage: $0 host1,host2,... [--ai]}
shift

echo ">>> Building (cross-compiled) and deploying to: $HOSTS"
./scripts/deploy.sh --hosts "$HOSTS" --build "$@"

echo ">>> Deployment complete."
