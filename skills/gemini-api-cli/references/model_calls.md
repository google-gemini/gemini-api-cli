# Normal Model Calls

This reference covers standard interactions with Gemini models using the `gemini-api` CLI.

## Basic Prompting

Run a simple prompt against the default model (`gemini-3-flash-preview`):

```bash
gemini-api run "What is the capital of France?"
```

## Specifying a Model

Use the `--model` or `-m` flag to specify a different model:

```bash
gemini-api run "Explain quantum computing" --model gemini-3.1-pro-preview
```

Common models:
- `gemini-3-flash-preview` (default)
- `gemini-3.1-pro-preview`
- `gemini-2.5-flash`
- `gemini-2.5-pro`

## Multi-Turn Conversations

To continue a conversation, use the `--previous-interaction-id` or `-p` flag with the ID returned from the previous command.

```bash
# First turn
gemini-api run "Remember the word: banana"
# Output will include: interaction_id: v1_...

# Second turn
gemini-api run "What word did I tell you to remember?" --previous-interaction-id v1_...
```

## Using Tools

You can enable tools like Google Search or Code Execution:

```bash
gemini-api run "What is the current weather in Tokyo?" --tool google_search
gemini-api run "Calculate the 100th Fibonacci number" --tool code_execution
```

You can repeat the `--tool` flag to enable multiple tools:

```bash
gemini-api run "Research and analyze the latest AI trends" --tool google_search --tool code_execution
```

## Multimodal Input

Pass files as input using the `--input` or `-i` flag:

```bash
gemini-api run "Describe this image" --input image:path/to/photo.jpg
gemini-api run "Summarize this document" --input document:path/to/report.pdf
```
