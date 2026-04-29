#!/bin/bash
# CUJ-31: Run with deployed agent (waverunner)
source ~/.bash_profile
CLI="bun run src/cli.ts"
mkdir -p tmp

echo "=== Running CUJ-31: Run with deployed agent (waverunner) ==="

# dry-run
$CLI run "What is 2+2?" --agent waverunner --dry-run

# live
$CLI run "What is 2+2?" --agent waverunner
