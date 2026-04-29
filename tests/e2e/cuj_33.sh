#!/bin/bash
# CUJ-33: Agent with environment persistence (multi-turn)
source ~/.bash_profile
CLI="bun run src/cli.ts"
mkdir -p tmp

echo "=== Running CUJ-33: Agent with environment persistence (multi-turn) ==="

# dry-run
$CLI run "Continue" --agent waverunner --previous-interaction-id fake_int --dry-run

# live (two-step)
RESULT=$($CLI run "Write 'hello' to tmp/test.txt" --agent waverunner --json 2>&1)
INT_ID=$(echo "$RESULT" | grep -o '"id":"[^"]*"' | head -1 | cut -d'"' -f4)

$CLI run "Read tmp/test.txt and tell me what it says" --agent waverunner --previous-interaction-id "$INT_ID"
