#!/bin/bash
# CUJ-19: Multiple tools together
source ~/.bash_profile
CLI="bun run src/cli.ts"
mkdir -p tmp

echo "=== Running CUJ-19: Multiple tools together ==="

# dry-run
$CLI run "Search and calculate" --tool google_search --tool code_execution --dry-run

# live
$CLI run "Search for the GDP of France then calculate GDP per capita" --tool google_search --tool code_execution
