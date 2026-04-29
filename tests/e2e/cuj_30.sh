#!/bin/bash
# CUJ-30: Agent test (interaction via local config)
source ~/.bash_profile
CLI="bun run src/cli.ts"
mkdir -p tmp

echo "=== Running CUJ-30: Agent test (interaction via local config) ==="

AGENT_NAME="e2e-test-$(date +%s)"
$CLI agents init "$AGENT_NAME"

# dry-run
$CLI agents test --prompt "Hello" --path "./$AGENT_NAME" --dry-run

# live
$CLI agents test --prompt "Hello" --path "./$AGENT_NAME"

rm -rf "$AGENT_NAME"
