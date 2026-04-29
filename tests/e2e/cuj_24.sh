#!/bin/bash
# CUJ-24: Agent create (deploy) — live
source ~/.bash_profile
CLI="bun run src/cli.ts"
mkdir -p tmp

echo "=== Running CUJ-24: Agent create (deploy) — live ==="

AGENT_NAME="e2e-create-live-$(date +%s)"
$CLI agents init "$AGENT_NAME"
$CLI agents create --path "./$AGENT_NAME"
# Cleanup
$CLI agents delete "$AGENT_NAME" --force
rm -rf "$AGENT_NAME"
