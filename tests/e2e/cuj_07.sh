#!/bin/bash
# CUJ-07: Service tier (flex)
source ~/.bash_profile
CLI="bun run src/cli.ts"
mkdir -p tmp

echo "=== Running CUJ-07: Service tier (flex) ==="

# dry-run
$CLI run "Hello" --service-tier flex --dry-run

# live
$CLI run "Hello" --service-tier flex
