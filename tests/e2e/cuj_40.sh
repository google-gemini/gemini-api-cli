#!/bin/bash
# CUJ-40: Missing agent.yaml for agents create
source ~/.bash_profile
CLI="bun run src/cli.ts"
mkdir -p tmp

echo "=== Running CUJ-40: Missing agent.yaml for agents create ==="

! $CLI agents create --path tmp/empty-dir-that-does-not-exist
