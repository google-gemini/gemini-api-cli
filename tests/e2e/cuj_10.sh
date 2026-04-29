#!/bin/bash
# CUJ-10: Missing input file error
source ~/.bash_profile
CLI="bun run src/cli.ts"
mkdir -p tmp

echo "=== Running CUJ-10: Missing input file error ==="

# No API call needed — this is a client-side validation
! $CLI run "Hello" --input image:nonexistent.png
