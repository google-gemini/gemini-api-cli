# Genmedia (Image, Audio, TTS)

This reference covers generating and editing media using the Gemini API CLI.

## Image Generation

To generate an image, use a model that supports image generation and specify an output file with `--output` or `-o`.

```bash
gemini-api run "A cat in space, oil painting" --model gemini-3.1-flash-image --output cat.png
```

Supported image models include:
- `gemini-3-pro-image` (Nano Banana Pro)
- `gemini-3.1-flash-image` (Nano Banana 2)
- `gemini-2.5-flash-image`

## Image Editing

You can edit an existing image by providing it as input and describing the changes:

```bash
gemini-api run "Add a red hat to the person" \
  --input image:person.jpg \
  --response-modality image \
  --output with_hat.jpg
```

## Text-to-Speech (TTS)

To generate audio from text, use a TTS model and specify an audio output file:

```bash
gemini-api run "Hello, I can help you with a wide range of tasks." \
  --model gemini-3.1-flash-tts-preview \
  --voice Kore \
  --output hello.wav
```

Supported TTS models:
- `gemini-3.1-flash-tts-preview`
- `gemini-2.5-flash-preview-tts`
- `gemini-2.5-pro-preview-tts`

## Music Generation (Lyria)

The CLI supports Lyria models for music generation, although specific command examples are minimal. You would typically use the `run` command with a Lyria model and an appropriate output extension (e.g., `.wav` or `.mp3`).

Supported Lyria models:
- `lyria-3-clip-preview` (Music clips)
- `lyria-3-pro-preview` (Full-song generation)

Example (inferred usage):
```bash
gemini-api run "A happy upbeat electronic track" --model lyria-3-pro-preview --output song.wav
```
