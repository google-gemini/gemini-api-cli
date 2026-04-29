#!/bin/bash
# CUJ-17: Google Search tool
source ~/.bash_profile
CLI="bun run src/cli.ts"
mkdir -p tmp

echo "=== Running CUJ-17: Google Search tool ==="

# dry-run
$CLI run "What happened today?" --tool google_search --dry-run

# live
$CLI run "What is the current population of Tokyo? Use search." --tool google_search
