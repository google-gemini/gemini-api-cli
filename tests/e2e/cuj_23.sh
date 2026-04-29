#!/bin/bash
# CUJ-23: Agent create (deploy) — dry-run
source ~/.bash_profile
CLI="bun run src/cli.ts"
mkdir -p tmp

echo "=== Running CUJ-23: Agent create (deploy) — dry-run ==="

AGENT_NAME="e2e-create-dry-$(date +%s)"
$CLI agents init "$AGENT_NAME"
$CLI agents create --path "./$AGENT_NAME" --dry-run
rm -rf "$AGENT_NAME"
