#!/bin/bash
# CUJ-25: Agent list
source ~/.bash_profile
CLI="bun run src/cli.ts"
mkdir -p tmp

echo "=== Running CUJ-25: Agent list ==="

# dry-run
$CLI agents list --dry-run

# live
$CLI agents list
