#!/bin/bash
# CUJ-37: Missing prompt
source ~/.bash_profile
CLI="bun run src/cli.ts"
mkdir -p tmp

echo "=== Running CUJ-37: Missing prompt ==="

! $CLI run
