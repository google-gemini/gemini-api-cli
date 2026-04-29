#!/bin/bash
# CUJ-43: Interaction logging
source ~/.bash_profile
CLI="bun run src/cli.ts"
mkdir -p tmp

echo "=== Running CUJ-43: Interaction logging ==="

rm -rf .gemini/logs
$CLI run "Hello"
ls .gemini/logs/
