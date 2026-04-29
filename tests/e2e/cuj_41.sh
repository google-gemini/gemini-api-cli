#!/bin/bash
# CUJ-41: Dry-run on all commands
source ~/.bash_profile
CLI="bun run src/cli.ts"
mkdir -p tmp

echo "=== Running CUJ-41: Dry-run on all commands ==="

$CLI run "Hello" --dry-run
$CLI agents create --dry-run
$CLI agents list --dry-run
$CLI agents get my-agent --dry-run
$CLI agents delete my-agent --force --dry-run
$CLI files list env_123 --dry-run
$CLI files download env_123 --dry-run
