#!/bin/bash
# CUJ-38: Missing API key
source ~/.bash_profile
CLI="bun run src/cli.ts"
mkdir -p tmp

echo "=== Running CUJ-38: Missing API key ==="

! GEMINI_API_KEY="" GEMINI_AUTOPUSH_API_KEY="" $CLI run "Hello"
