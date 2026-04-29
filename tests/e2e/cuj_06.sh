#!/bin/bash
# CUJ-06: System instruction
source ~/.bash_profile
CLI="bun run src/cli.ts"
mkdir -p tmp

echo "=== Running CUJ-06: System instruction ==="

# dry-run
$CLI run "What are you?" --system-instruction "You are a pirate. Always respond in pirate speak." --dry-run

# live
$CLI run "What are you?" --system-instruction "You are a pirate. Always respond in pirate speak."
