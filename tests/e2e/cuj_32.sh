#!/bin/bash
# CUJ-32: Run with custom agent
source ~/.bash_profile
CLI="bun run src/cli.ts"
mkdir -p tmp

echo "=== Running CUJ-32: Run with custom agent ==="

# dry-run
$CLI run "Hello" --agent my-custom-agent --dry-run

# live (requires agent to exist)
$CLI run "Hello" --agent "$AGENT_NAME"
