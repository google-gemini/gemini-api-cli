#!/bin/bash
# CUJ-20: Invalid tool error
source ~/.bash_profile
CLI="bun run src/cli.ts"
mkdir -p tmp

echo "=== Running CUJ-20: Invalid tool error ==="

! $CLI run "Hello" --tool invalid_tool
