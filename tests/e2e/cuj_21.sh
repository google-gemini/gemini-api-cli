#!/bin/bash
# CUJ-21: Agent init (scaffold)
source ~/.bash_profile
CLI="bun run src/cli.ts"
mkdir -p tmp

echo "=== Running CUJ-21: Agent init (scaffold) ==="

AGENT_NAME="e2e-test-agent-$(date +%s)"

$CLI agents init "$AGENT_NAME"
