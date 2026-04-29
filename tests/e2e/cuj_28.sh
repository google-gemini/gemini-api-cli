#!/bin/bash
# CUJ-28: Agent delete
source ~/.bash_profile
CLI="bun run src/cli.ts"
mkdir -p tmp

echo "=== Running CUJ-28: Agent delete ==="

# dry-run
$CLI agents delete my-agent --force --dry-run

# live
AGENT_NAME="e2e-delete-target-$(date +%s)"
$CLI agents init "$AGENT_NAME"
$CLI agents create --path "./$AGENT_NAME"
$CLI agents delete "$AGENT_NAME" --force
rm -rf "$AGENT_NAME"
