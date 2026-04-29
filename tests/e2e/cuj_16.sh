#!/bin/bash
# CUJ-16: Code execution tool
source ~/.bash_profile
CLI="bun run src/cli.ts"
mkdir -p tmp

echo "=== Running CUJ-16: Code execution tool ==="

# dry-run
$CLI run "Calculate 2+2" --tool code_execution --dry-run

# live
$CLI run "Use code execution to calculate 2+2 and return only the number" --tool code_execution
