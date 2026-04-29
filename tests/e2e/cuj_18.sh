#!/bin/bash
# CUJ-18: URL context tool
source ~/.bash_profile
CLI="bun run src/cli.ts"
mkdir -p tmp

echo "=== Running CUJ-18: URL context tool ==="

# dry-run
$CLI run "Summarize https://www.wikipedia.org/" --tool url_context --dry-run

# live
$CLI run "Summarize the content of https://www.wikipedia.org/" --tool url_context
