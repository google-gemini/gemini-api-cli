#!/bin/bash
# CUJ-15: Image editing with edit_strength and mask
source ~/.bash_profile
CLI="bun run src/cli.ts"
mkdir -p tmp

echo "=== Running CUJ-15: Image editing with edit_strength and mask ==="

# dry-run
echo "dummy" > tmp/tmp_input.png
echo "dummy" > tmp/tmp_mask.png
$CLI run "Edit this" --input image:tmp/tmp_input.png --response-modality image --edit-strength 0.5 --mask tmp/tmp_mask.png --dry-run
