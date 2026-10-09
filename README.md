# Gemini API CLI (`gemini-api`)

[![Release](https://img.shields.io/github/v/release/google-gemini/gemini-api-cli)](https://github.com/google-gemini/gemini-api-cli/releases)
[![License: Apache 2.0](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](https://opensource.org/licenses/Apache-2.0)

**Gemini models and managed agents for the agent you already use.**

Give Codex, Claude Code, Antigravity, OpenClaw, Hermes, or any other agent with shell access the ability to use the Gemini API. Configure an API key, put `gemini-api` on `PATH`, and let your agent choose a model, generate an asset, analyze a file, or launch a managed agent to handle a task.

Your agent plans the work. Gemini provides models and managed agents. This stateless CLI connects them through standard shell commands:

```bash
gemini-api image "A friendly robot mascot for a developer tool" --out ./assets/
```

A prompt goes in. An image lands in your project. Your agent gets the file path and keeps building. The same binary can return an analysis, a voiceover, a transcript, or the interaction ID of a background managed-agent run.

[Installation](#installation) · [Quick start](#quick-start) · [Command overview](#command-overview) · [Agent discovery](#agent-discovery-and-machine-interface) · [Workflows](#agent-workflows) · [Gemini API docs](https://ai.google.dev/gemini-api/docs)

---

## Why this CLI

A coding agent building an app may need a hero image, a voiceover, a second opinion on a design, or an answer inside a PDF. An assistant may need to turn a recording into captions or delegate multi-step research. Give that agent `gemini-api` and an API key, and it can call those capabilities directly when the task requires them.

- **One binary for models, media, and managed agents:** Access text and reasoning models, multimodal file analysis, image/audio/music/video generation, and managed agents from the command line.
- **Delegate long-running work:** Start a Gemini managed agent in the background, keep the returned interaction ID, and retrieve the result when needed.
- **Self-describing interface:** Commands, flags, request schemas (`--schema`), and machine-readable usage (`--usage`) are built into the binary so an agent can inspect what it needs on the fly.
- **Safe dry-run validation:** `--dry-run` validates inputs and prints the resolved HTTP request with credentials redacted—without making a network call or reading the OS keychain.
- **Script- and agent-friendly output:** Stream text, write media directly to disk, emit structured JSON (`--output-format json`) or compact TOON (`--output-format toon`), or filter fields inline with `--jq`.
- **Stateless execution:** Each invocation runs independently. Your agent supplies the context, chooses the model, and controls the next step.

Built for headless use by coding agents and assistant tools, with the same interface available to shell scripts and CI jobs. A single Go binary runs on macOS, Linux, and Windows.

---

## Installation

### macOS and Linux

```bash
curl -fsSL https://raw.githubusercontent.com/google-gemini/gemini-api-cli/main/scripts/install.sh | bash
```

### Windows (PowerShell)

```powershell
iwr -useb https://raw.githubusercontent.com/google-gemini/gemini-api-cli/main/scripts/install.ps1 | iex
```

### Go install

Install the latest release with Go:

```bash
go install github.com/google-gemini/gemini-api-cli/cmd/gemini-api@latest
```

To build the current development version, use `@main` instead of `@latest`. Current `main` requires **Go 1.26.8** or later (Go's automatic toolchain selection downloads the required version if needed).

### Prebuilt binaries

Download prebuilt archives for macOS, Linux, and Windows from [GitHub Releases](https://github.com/google-gemini/gemini-api-cli/releases) and place `gemini-api` on your `PATH`.

---

## Quick start

### 1. Configure your API key

Get a key from [Google AI Studio](https://aistudio.google.com/apikey) and make it available to your shell or your agent's subprocess environment:

```bash
export GEMINI_API_KEY="YOUR_API_KEY"
```

> **Workstation tip:** To persist your key in your OS keychain (macOS Keychain, Linux Secret Service, or Windows Credential Manager) or `~/.config/gemini-api/config.yaml`, run `gemini-api configure --api-key "YOUR_API_KEY"` or open the interactive form with `gemini-api configure --interactive`. Check your active configuration and credential source at any time with `gemini-api whoami`.

Verify your setup locally with `--dry-run` (no network call), then remove `--dry-run` to call the API:

```bash
gemini-api generate "Hello from my agent" --dry-run
gemini-api generate "Hello from my agent"
```

### 2. Give your agent instructions

Add the following block to your agent's instructions (`AGENTS.md`, `CLAUDE.md`, `.cursorrules`, or tool configuration):

```text
You have access to gemini-api through the shell.
Use it to call Gemini models, generate or edit media, analyze files, or launch managed agents.
1. Inspect --help or --usage to choose a command, and --schema when building a --body JSON payload.
2. Preview API calls with --dry-run (or --dry-run --output-format json).
3. For discrete JSON output or --jq filtering on streaming/background commands (generate, agent run, agent status), pass --stream=false --output-format json.
4. Capture returned file paths (from media commands) and interaction IDs (from agent/model runs) to continue multi-step tasks.
```

### 3. Put your agent to work

Prompt your host agent as usual:

```text
Build a landing page for this app. Use Gemini to create a mascot image and a welcome voiceover, and launch a managed research agent to investigate competing products. Incorporate the assets and findings into the page.
```

Your agent chooses the commands, runs them in its shell, and uses the returned file paths and interaction IDs in its own workflow.

---

## Command overview

Run `gemini-api --help` to see all top-level commands, or `gemini-api <command> --help` for command-specific flags and examples.

| Category | Command | Subcommands / Key Flags | Description & default model |
|---|---|---|---|
| **Create** | `gemini-api generate` | `-m`, `-i/--input`, `--stream=false`, `--body` | Text and multimodal generation (`gemini-3.8-flash`) |
| | `gemini-api image` | `-m`, `-i/--input`, `--out`, `--body` | Generate or edit images (`gemini-nano-banana-2.1`) |
| | `gemini-api tts` | `-f/--file`, `--stdin`, `--voice`, `--multi-speaker`, `--out` | Text-to-speech audio generation (`gemini-3.8-flash-tts`) |
| | `gemini-api music` | `-m`, `--out`, `--body` | Music generation (`lyria-3.5`) |
| | `gemini-api video` | `-m`, `-i/--input`, `--async`, `--out`, `--body` | Conversational video generation and editing (`gemini-omni-1.1-flash`) |
| **Understand** | `gemini-api analyze` | `-i/--input` *(repeatable)*, `-f/--file`, `--stdin`, `-m` | Ask questions about PDFs, images, audio, video, CSV/text files, or YouTube URLs (`gemini-3.8-flash`) |
| | `gemini-api transcribe` | `-i/--input` *(repeatable)*, `--format` (`md`, `text`, `json`, `srt`), `--out` | Transcribe audio or video to Markdown (`md`), plain text (`text`), JSON (`json`), or SRT captions (`srt`) (`gemini-3.8-flash`) |
| **Manage** | `gemini-api agent` | `run`, `status`, `cancel`, `delete-interaction`, `create`, `list`, `get`, `delete` | Run interactions with models or managed agents (`run`, `status`, `cancel`) and manage agent definitions |
| | `gemini-api files` | `upload`, `list`, `get`, `generated-files-list`, `register`, `delete` | Upload, list, inspect, register, and delete files via the Files API (48-hour retention) |
| | `gemini-api models` | *(bare catalog)*, `list` (`--all`), `get` | View the offline model catalog, or query live API models (`list`, `get`) |
| | `gemini-api configure` | `--api-key`, `--interactive`, `--output-format` | Configure authentication, global parameters, and persistent preferences |
| **Advanced** | `gemini-api environments` | `create`, `list`, `get`, `delete`, `files` (`list`, `upload`, `download`) | Manage sandbox environments and upload, list, or download environment files |
| | `gemini-api credentials` | `create`, `list`, `get`, `update`, `delete` | Manage stored server-side credentials for managed agents |
| | `gemini-api webhooks` | `create`, `list`, `get`, `update`, `ping`, `rotate-signing-secret`, `delete` | Manage webhook endpoints and signing secrets for event delivery |
| | `gemini-api triggers` | `list`, `get`, `update`, `run`, `list-executions`, `delete` | Inspect, run, update, and delete cron triggers for managed agents |
| **Utility** | `gemini-api whoami` / `auth` / `explore` / `version` | `auth login`, `auth status`, `auth logout` | Inspect active credentials (`whoami`, `auth`), launch the interactive explorer (`explore`), or print the CLI version (`version`) |

---

## Agent discovery and machine interface

Everything an agent needs to use `gemini-api` is discoverable from the binary itself, without external documentation or credentials:

| When an agent needs to… | It runs… |
|---|---|
| Discover commands and flags | `gemini-api --help` or `gemini-api --usage` |
| Inspect a request body schema | `gemini-api agent run --schema` |
| Validate and preview a call offline | `gemini-api image "A robot mascot" --dry-run` |
| Parse a complete structured response | `gemini-api generate "Hello" --stream=false --output-format json` |
| Extract a single field inline | `gemini-api agent list --jq '.agents[].id'` |

- **`--usage`** outputs the complete command tree, flags, defaults, environment variables, and config keys as machine-readable [KDL](https://kdl.dev).
- **`--schema`** prints the bundled JSON Schema (draft 2020-12) for any command that accepts a request body.
- **`--dry-run`** validates flags and payloads and previews the HTTP request without credentials or network access (use `--dry-run --output-format json` for machine-parseable JSON request previews).
- **`--no-interactive`** disables all interactive prompts and TUI forms.
- **`--agent-mode`** (or `GEMINI_CLI_AGENT_MODE=1`) enables structured JSON error envelopes on `stderr` and defaults `stdout` to compact [TOON](https://github.com/toon-format/spec) output (`--output-format json` explicitly selects JSON).
- **Exit codes** distinguish success (`0`), runtime/API failures (`1`), invalid usage/validation errors (`2`), and authentication/authorization failures (`3`).

Run `gemini-api --help-global` to see all global authentication, network, output, and diagnostic flags.

---

## Agent workflows

### 1. Generate and edit creative assets

Coding agents like Codex, Claude Code, or Antigravity can generate images, speech, music, and video while building an app:

<!-- readme-examples: skip (needs local file from previous command) -->
```bash
# Generate an image and capture the written file path
MASCOT_PATH=$(gemini-api image "A friendly robot mascot, soft studio lighting" --out ./assets/mascot.jpg)

# Edit an existing image with -i / --input
gemini-api image "Add a winter scarf and snowy background" -i "$MASCOT_PATH" --out ./assets/mascot-winter.jpg

# Generate single-speaker or multi-speaker voiceovers
gemini-api tts "Welcome. Let's build something together." --out ./assets/welcome.wav
gemini-api tts "Alice: Welcome to the demo! Bob: Let's dive right in." \
  --multi-speaker "Alice=Kore,Bob=Puck" \
  --out ./assets/dialogue.wav

# Generate background music and video
gemini-api music "Warm ambient music for a product walkthrough" --out ./assets/
gemini-api video "A slow camera move through a sunlit workshop" --out ./assets/
```

Media commands write files locally and print only the written file path to `stdout`. When `--out` points to a directory (or is omitted), the CLI names the file automatically using the file extension that matches the API's returned MIME type (for example, `.jpg` for `image/jpeg`, `.wav` for `audio/wav`, `.mp3` for `audio/mpeg`, and `.mp4` for `video/mp4`).

### 2. Analyze documents and transcribe recordings

Assistants like OpenClaw or Hermes can turn documents, media files, and YouTube URLs into answers and deliverables:

<!-- readme-examples: skip (needs local files) -->
```bash
# Analyze a PDF or compare multiple images
gemini-api analyze --input product-brief.pdf "What should we build first, and why?" > plan.md
gemini-api analyze --input before.png --input after.png "Summarize the visual design changes"

# Transcribe audio or video (valid formats: md, text, json, srt)
gemini-api transcribe --input interview.mp4 --format srt --out interview.srt
```

For larger media files (over ~14 MB raw / 20 MB base64-encoded), upload once with `files upload --wait` and reuse the returned `files/<id>` URI across `analyze`, `transcribe`, `generate`, `image`, `video`, or `agent run`:

<!-- readme-examples: skip (needs local file and uploaded file URI) -->
```bash
FILE_URI=$(gemini-api files upload interview.mp4 --wait --output-format json --jq '.name')
gemini-api analyze --input "$FILE_URI" "List the key action items with timestamps"
```

### 3. Launch and poll a background managed agent

Delegate multi-step research or coding tasks to a Gemini managed agent such as Deep Research. Pass `--background --stream=false` so the CLI returns the interaction ID immediately instead of holding open an SSE stream:

```bash
INTERACTION_ID=$(gemini-api agent run "Research battery recycling approaches and cite sources" \
  --agent deep-research-preview-04-2026 \
  --background \
  --stream=false \
  --jq '.id')
echo "Started interaction: $INTERACTION_ID"
```

Check progress or retrieve the finished output using the returned `INTERACTION_ID`:

```bash
gemini-api agent status "$INTERACTION_ID" --stream=false --output-format json
```

You can also stream live progress events (`gemini-api agent status "$INTERACTION_ID" --stream`) or cancel an active run (`gemini-api agent cancel "$INTERACTION_ID"`).

### 4. Choose the right model

Use `--model` (`-m`) on generation and analysis commands to pick a model for reasoning, fast text, multimodal understanding, or media generation:

```bash
# View the curated offline model catalog grouped by capability
gemini-api models

# Query all live models available to your API key (--all auto-paginates across pages)
gemini-api models list --all
gemini-api models get gemini-3.8-flash

# Run a prompt with a specific model
gemini-api generate "Review this approach: cache immutable responses by content hash" --model gemini-3.8-flash
```

---

## Go deeper: custom request bodies (`--schema` to `--body`)

Short flags cover common tasks; `--body` gives your agent full access to the underlying Interactions API request schema while keeping the CLI's file output and error handling.

First, inspect the schema for `image`:

```bash
gemini-api image --schema > image-schema.json
```

Next, create `hero-image.json` to select `gemini-nano-banana-2.1`, request a `16:9` image at `2K` size (`image_size`: `"512"`, `"1K"`, `"2K"`, or `"4K"`), and disable server-side interaction storage (`"store": false`):

```bash
cat > hero-image.json <<'JSON'
{
  "model": "gemini-nano-banana-2.1",
  "input": "A friendly robot mascot in a sunlit workshop, composed on the right with uncluttered space on the left for a landing page headline. No text or watermark.",
  "response_format": {
    "type": "image",
    "aspect_ratio": "16:9",
    "image_size": "2K"
  },
  "store": false,
  "stream": false
}
JSON
```

Preview the exact HTTP payload with `--dry-run`, then execute the request:

<!-- readme-examples: skip (needs local hero-image.json file) -->
```bash
gemini-api image --body @hero-image.json --out ./assets/hero.jpg --dry-run --output-format json
gemini-api image --body @hero-image.json --out ./assets/hero.jpg --output-format json
```

With `--output-format json`, the CLI writes the image to disk and returns structured artifact metadata on `stdout`:

```json
{
  "kind": "image",
  "mime_type": "image/jpeg",
  "path": "/path/to/project/assets/hero.jpg",
  "size_bytes": 2914159,
  "status": "completed"
}
```

`--out` controls the local file destination; `response_format` controls what the model generates (pass `--raw-response` instead of `--out` if you want the full raw API response envelope). The same `--schema` to `--body` workflow applies to `gemini-api agent run` and other commands when configuring `system_instruction`, `tools`, structured JSON `response_format.schema`, or stateful multi-turn continuations via `previous_interaction_id`:

<!-- readme-examples: skip (uses shell variable from previous turn) -->
```bash
# Turn 1: Run a non-streaming interaction and capture its ID
TURN1_ID=$(gemini-api generate "Propose 3 database indexing strategies for a time-series log table." \
  --stream=false --jq '.id')

# Turn 2: Continue the conversation using previous_interaction_id
gemini-api agent run "Compare strategy #2 and #3 for write-heavy workloads." \
  --body "{\"previous_interaction_id\": \"$TURN1_ID\"}" \
  --stream=false
```

The CLI stores no local conversation state—server-side interactions and uploaded files manage their own retention on the API, and your workflow keeps only the IDs it wants to reuse.

---

## Status, feedback, and contributions

> [!NOTE]
> `gemini-api` is in **beta** and evolving alongside the [Gemini Interactions API](https://ai.google.dev/gemini-api/docs/interactions-overview). Breaking changes may occur between releases; we recommend pinning to a specific [release version](https://github.com/google-gemini/gemini-api-cli/releases) in CI and automated workflows.

- **Feedback & bug reports:** Try `gemini-api` in your agent workflow and let us know where it helps or where it gets in the way by opening an issue in [GitHub Issues](https://github.com/google-gemini/gemini-api-cli/issues).
- **Contributions:** We are **not** accepting open source contributions (pull requests) at this time.
- **Releases:** Browse changelog notes and prebuilt binaries on [GitHub Releases](https://github.com/google-gemini/gemini-api-cli/releases).

---

## Licensing & Disclaimer

Copyright 2026 Google LLC

All software is licensed under the Apache License, Version 2.0 (Apache 2.0); you may not use this file except in compliance with the Apache 2.0 license. You may obtain a copy of the Apache 2.0 license at: https://www.apache.org/licenses/LICENSE-2.0

All other materials are licensed under the Creative Commons Attribution 4.0 International License (CC-BY). You may obtain a copy of the CC-BY license at: https://creativecommons.org/licenses/by/4.0/legalcode

Unless required by applicable law or agreed to in writing, all software and materials distributed here under the Apache 2.0 or CC-BY licenses are distributed on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied. See the licenses for the specific language governing permissions and limitations under those licenses.

This is not an official Google product.
