#!/bin/bash
# CUJ-01: Simple text prompt (non-streaming)
source ~/.bash_profile
CLI="bun run src/cli.ts"
mkdir -p tmp

echo "=== Running CUJ-01: Simple text prompt (non-streaming) ==="

# dry-run
$CLI run "What is 2+2?" --dry-run

# live
$CLI run "What is 2+2?"
