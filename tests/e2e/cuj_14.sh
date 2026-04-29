#!/bin/bash
# CUJ-14: Image editing (input image + output image)
source ~/.bash_profile
CLI="bun run src/cli.ts"
mkdir -p tmp

echo "=== Running CUJ-14: Image editing (input image + output image) ==="

# Create test image
echo "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==" | base64 -d > tmp/test_edit.png

# dry-run
$CLI run "Make it green" --input image:tmp/test_edit.png --response-modality image --model gemini-3-pro-image-preview --output tmp/test_edited.png --dry-run

# live
$CLI run "Make this image green" --input image:tmp/test_edit.png --response-modality image --model gemini-3-pro-image-preview --output tmp/test_edited.png
