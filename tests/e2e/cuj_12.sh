#!/bin/bash
# CUJ-12: Image generation with config (aspect ratio, size)
source ~/.bash_profile
CLI="bun run src/cli.ts"
mkdir -p tmp

echo "=== Running CUJ-12: Image generation with config (aspect ratio, size) ==="

# dry-run
$CLI run "A sunset" --model gemini-3-pro-image-preview --aspect-ratio 16:9 --image-size 2k --dry-run

# live
$CLI run "A sunset over mountains" --model gemini-3-pro-image-preview --aspect-ratio 16:9 --image-size 2k --output tmp/test_sunset.png
