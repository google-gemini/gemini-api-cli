#!/bin/bash
# CUJ-34: Deep Research (background mode)
source ~/.bash_profile
CLI="bun run src/cli.ts"
mkdir -p tmp

echo "=== Running CUJ-34: Deep Research (background mode) ==="

# dry-run
$CLI run "Research the history of TPUs" --agent deep-research-preview-04-2026 --dry-run

# live (will take minutes)
$CLI run "Research the history of Google TPUs in 2 paragraphs" --agent deep-research-preview-04-2026
