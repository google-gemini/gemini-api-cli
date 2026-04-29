#!/bin/bash
# CUJ-09: Image understanding
source ~/.bash_profile
CLI="bun run src/cli.ts"
mkdir -p tmp

echo "=== Running CUJ-09: Image understanding ==="

# Create a test image (1x1 red PNG)
echo "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==" | base64 -d > tmp/test.png

# dry-run
$CLI run "What color is this?" --input image:tmp/test.png --dry-run

# live
$CLI run "What color is this?" --input image:tmp/test.png
