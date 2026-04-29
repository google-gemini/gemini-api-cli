#!/bin/bash
# CUJ-13: Text-to-speech (TTS)
source ~/.bash_profile
CLI="bun run src/cli.ts"
mkdir -p tmp

echo "=== Running CUJ-13: Text-to-speech (TTS) ==="

# dry-run
$CLI run "Hello world" --model gemini-3.1-flash-tts-preview --voice Kore --language en-US --output tmp/test_tts.wav --dry-run

# live
$CLI run "Hello world" --model gemini-3.1-flash-tts-preview --voice Kore --output tmp/test_tts.wav
