#!/bin/bash
# CUJ-02: Simple text prompt (streaming)
source ~/.bash_profile
CLI="bun run src/cli.ts"
mkdir -p tmp

echo "=== Running CUJ-02: Simple text prompt (streaming) ==="

# dry-run
$CLI run "Count to 5" --dry-run

# live
$CLI run "Count to 5"
