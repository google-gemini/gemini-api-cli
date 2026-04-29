#!/bin/bash
# CUJ-26: Agent list (JSON mode)
source ~/.bash_profile
CLI="bun run src/cli.ts"
mkdir -p tmp

echo "=== Running CUJ-26: Agent list (JSON mode) ==="

# dry-run
$CLI agents list --json --dry-run

# live
$CLI agents list --json
