#!/bin/bash
# CUJ-42: Completion summary metadata
source ~/.bash_profile
CLI="bun run src/cli.ts"
mkdir -p tmp

echo "=== Running CUJ-42: Completion summary metadata ==="

$CLI run "Say hello"
