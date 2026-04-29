#!/bin/bash
# CUJ-11: Image generation
source ~/.bash_profile
CLI="bun run src/cli.ts"
mkdir -p tmp

echo "=== Running CUJ-11: Image generation ==="

# dry-run
$CLI run "A blue square" --model gemini-3-pro-image-preview --output tmp/test_gen.png --dry-run

# live
$CLI run "Generate a simple blue square" --model gemini-3-pro-image-preview --output tmp/test_gen.png
