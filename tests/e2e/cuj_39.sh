#!/bin/bash
# CUJ-39: Invalid model (400 error)
source ~/.bash_profile
CLI="bun run src/cli.ts"
mkdir -p tmp

echo "=== Running CUJ-39: Invalid model (400 error) ==="

! $CLI run "Hello" --model nonexistent-model
