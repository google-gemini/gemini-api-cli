#!/bin/bash
# CUJ-08: Stateful multi-turn with previous-interaction-id
source ~/.bash_profile
CLI="bun run src/cli.ts"
mkdir -p tmp

echo "=== Running CUJ-08: Stateful multi-turn with previous-interaction-id ==="

# dry-run
$CLI run "What was the word?" --previous-interaction-id fake_id_123 --dry-run

# live (two-step)
# Turn 1:
RESULT=$($CLI run "Remember the word: banana" --json 2>&1)
INT_ID=$(echo "$RESULT" | grep -o '"id":"[^"]*"' | head -1 | cut -d'"' -f4)

# Turn 2:
$CLI run "What word did I ask you to remember?" --previous-interaction-id "$INT_ID"
