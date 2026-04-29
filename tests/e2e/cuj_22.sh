#!/bin/bash
# CUJ-22: Agent init is idempotent
source ~/.bash_profile
CLI="bun run src/cli.ts"
mkdir -p tmp

echo "=== Running CUJ-22: Agent init is idempotent ==="

AGENT_NAME="e2e-idempotent-$(date +%s)"
$CLI agents init "$AGENT_NAME"
$CLI agents init "$AGENT_NAME"
rm -rf "$AGENT_NAME"
