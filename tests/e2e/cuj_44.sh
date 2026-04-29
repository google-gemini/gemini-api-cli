#!/bin/bash
# CUJ-44: Invalid agent error
source ~/.bash_profile
CLI="bun run src/cli.ts"
mkdir -p tmp

echo "=== Running CUJ-44: Invalid agent error ==="

! $CLI run "Hello" --agent invalid_agent
