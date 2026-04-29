#!/bin/bash
# CUJ-04: JSON output mode
source ~/.bash_profile
CLI="bun run src/cli.ts"
mkdir -p tmp

echo "=== Running CUJ-04: JSON output mode ==="

# dry-run
$CLI run "Hello" --json --dry-run

# live
$CLI run "Say hi" --json
