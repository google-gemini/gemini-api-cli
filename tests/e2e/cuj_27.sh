#!/bin/bash
# CUJ-27: Agent get
source ~/.bash_profile
CLI="bun run src/cli.ts"
mkdir -p tmp

echo "=== Running CUJ-27: Agent get ==="

# dry-run
$CLI agents get my-agent --dry-run

# live
$CLI agents get "$AGENT_NAME"
