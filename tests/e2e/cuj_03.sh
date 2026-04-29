#!/bin/bash
# CUJ-03: Specify a model explicitly
source ~/.bash_profile
CLI="bun run src/cli.ts"
mkdir -p tmp

echo "=== Running CUJ-03: Specify a model explicitly ==="

# dry-run
$CLI run "Hello" --model gemini-3.1-pro-preview --dry-run

# live
$CLI run "Hello" --model gemini-3.1-pro-preview
