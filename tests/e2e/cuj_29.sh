#!/bin/bash
# CUJ-29: Agent full lifecycle (create → list → get → delete)
source ~/.bash_profile
CLI="bun run src/cli.ts"
mkdir -p tmp

echo "=== Running CUJ-29: Agent full lifecycle (create → list → get → delete) ==="

AGENT_NAME="e2e-lifecycle-$(date +%s)"

# 1. Init
$CLI agents init "$AGENT_NAME"

# 2. Create
$CLI agents create --path "./$AGENT_NAME"

# 3. Test Interaction
$CLI run "What is 2+2?" --agent "$AGENT_NAME"

# 4. List — should include the agent
$CLI agents list

# 5. Get — should return details
$CLI agents get "$AGENT_NAME"

# 6. Delete
$CLI agents delete "$AGENT_NAME" --force

# 7. Cleanup
rm -rf "$AGENT_NAME"
