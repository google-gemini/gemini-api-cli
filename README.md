# gemini-api

Command-line interface for the *Gemini* API.

[![Built by Speakeasy](https://img.shields.io/badge/Built_by-SPEAKEASY-374151?style=for-the-badge&labelColor=f3f4f6)](https://www.speakeasy.com/?utm_source=google3-/third-party/gemini-api-cli&utm_campaign=cli)
[![License: Apache 2.0](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](https://opensource.org/licenses/Apache-2.0)


<br /><br />
> [!IMPORTANT]
> This CLI is not yet ready for production use. Delete this notice before publishing to a package manager.

<!-- Start Summary [summary] -->
## Summary

Gemini API: Use the Gemini Interactions API and managed-agent platform from the command line.

Get started:
  Set GEMINI_API_KEY, or run: gemini-api configure
  Then run a model or managed agent: gemini-api agent --help
  Add --dry-run to preview any API call without sending it.
<!-- End Summary [summary] -->

<!-- Start Table of Contents [toc] -->
## Table of Contents
<!-- $toc-max-depth=2 -->
* [gemini-api](#gemini-api)
  * [CLI Installation](#cli-installation)
  * [Shell Completion](#shell-completion)
  * [CLI Example Usage](#cli-example-usage)
  * [For AI agents](#for-ai-agents)
  * [Authentication](#authentication)
  * [Configuration](#configuration)
  * [Commands](#commands)
  * [Request Body Input](#request-body-input)
  * [Server Selection](#server-selection)
  * [Output Formats](#output-formats)
  * [Server-Sent Event Streaming](#server-sent-event-streaming)
  * [Pagination](#pagination)
  * [Retries](#retries)
  * [Error Handling](#error-handling)
  * [Diagnostics](#diagnostics)
* [Development](#development)
  * [Maturity](#maturity)
  * [Contributions](#contributions)

<!-- End Table of Contents [toc] -->

<!-- Start CLI Installation [installation] -->
## CLI Installation

### Quick Install (Linux/macOS)

```bash
curl -fsSL https://raw.githubusercontent.com/google-gemini/gemini-api-cli/main/scripts/install.sh | bash
```

### Quick Install (Windows PowerShell)

```powershell
iwr -useb https://raw.githubusercontent.com/google-gemini/gemini-api-cli/main/scripts/install.ps1 | iex
```

### Go Install

Alternatively, install directly via Go:

```bash
go install google3/third_party/gemini_api_cli/cmd/gemini-api@latest
```

### Manual Download

Download pre-built binaries for your platform from the [releases page](https://github.com/google-gemini/gemini-api-cli/releases).
<!-- End CLI Installation [installation] -->

<!-- Start Shell Completion [completion] -->
## Shell Completion

Shell completions are available for Bash, Zsh, Fish, and PowerShell.

### Bash

```bash
# Add to ~/.bashrc:
source <(gemini-api completion bash)

# Or install permanently:
gemini-api completion bash > /etc/bash_completion.d/gemini-api
```

### Zsh

```zsh
# Add to ~/.zshrc:
source <(gemini-api completion zsh)

# Or install permanently:
gemini-api completion zsh > "${fpath[1]}/_gemini-api"
```

### Fish

```fish
gemini-api completion fish | source

# Or install permanently:
gemini-api completion fish > ~/.config/fish/completions/gemini-api.fish
```

### PowerShell

```powershell
gemini-api completion powershell | Out-String | Invoke-Expression
```
<!-- End Shell Completion [completion] -->

<!-- Start CLI Example Usage [usage] -->
## CLI Example Usage

### Quick start

```bash
# Run a managed agent by ID
gemini-api agent run "Analyze market trends for Q3" --agent deep-research-preview-04-2026

# Choose a different model
gemini-api generate "Write a haiku about APIs" --model gemini-2.5-pro

# Generate an image (prints the written file path)
gemini-api image "a lighthouse at sunset"
```

### Example

```bash
gemini-api agent list --api-key test_api_key --api-version v1beta

```
<!-- End CLI Example Usage [usage] -->

<!-- Start For AI agents [agents] -->
## For AI agents

This CLI is built to be driven by AI coding agents as well as people: everything an agent needs is discoverable from the binary itself, and every command can be validated without credentials. Work down this ladder:

| Run | You get |
|-----|---------|
| `gemini-api --help`, `gemini-api agent list --help` | Commands by category, runnable examples, flags |
| `gemini-api --usage`, `gemini-api agent list --usage` | The command surface as machine-readable [KDL](https://kdl.dev): commands, aliases, flags, defaults, env vars, config keys |
| `gemini-api agent run --schema` | The exact JSON Schema of the command's request body (all `$ref`s bundled) — build a valid `--body` from it |
| `gemini-api agent list --dry-run` | The exact HTTP request (method, URL, headers, body), with no credentials or network call |
| `gemini-api agent list --output-format json` (or `--jq`) | Machine-readable output |

### Discover the command surface

```bash
# Every command, flag, default, env var and config key, as KDL
gemini-api --usage

# One command's subtree only
gemini-api agent list --usage
```

### Read the exact request schema

`--schema` is available on every command that accepts a request body (`--body`, stdin, or a whole-body flag where the command has one), including intent commands. It prints the JSON Schema the request is validated against and exits without calling the API.

```bash
# JSON Schema (draft 2020-12) of the request body, with every $ref bundled under $defs
gemini-api agent run --schema
```

### Probe before you spend

Start quota-spending commands with `--dry-run`. It validates inputs, resolves the request, redacts secrets and binary payloads, makes no network call, and exits 0. It never reads the OS keychain; credentials supplied by flag, environment, or config file are included only as `[REDACTED]`.

```bash
# Human preview: the [DRY-RUN] block is on stderr and stdout is empty
gemini-api agent list --dry-run
gemini-api agent run "Analyze market trends for Q3" --agent deep-research-preview-04-2026 --dry-run

# Machine preview: compact JSON on stdout and silent stderr
gemini-api agent list --dry-run --output-format json
```

The machine form writes one object per would-be request, one per line (NDJSON for multi-request commands), with exactly this shape:

```json
{"dry_run":true,"request":{"method":"POST","url":"https://…","headers":{"Accept":["application/json"],…},"body":<JSON value | string | null>}}
```

`body` is a parsed JSON value when the body is JSON, a string for text, `"<bytes:N>"` for binary data, and `null` when absent. An explicit caller `--jq` also selects this JSON preview protocol, but the filter is not applied to preview objects. Command-declared jq presets do not select or filter the preview.

Local mutation commands make no request under `--dry-run`: instead of a preview they emit one `{"dry_run":true,"local":true,"command":"…","message":"…"}` object. `select(.request)` keeps only would-be requests; `select(.local)` keeps the local no-ops.

### Machine-readable output

```bash
# JSON on stdout
gemini-api agent list --output-format json

# Filter or reshape with a jq expression (always emits JSON, overrides --output-format)
gemini-api agent list --jq '.'

# Print jq string results as plain text instead of JSON strings (like jq -r)
gemini-api agent list --jq '.' --raw-output
```

`--output-format toon` emits [TOON](https://github.com/toon-format/spec), a compact line-oriented format that uses fewer tokens than JSON; it is the default in agent mode.

### Interactive mode
This CLI is non-interactive by default. Pass `--interactive` to prompt for missing inputs or open guided `configure` / `auth login` forms. Required-input prompts require an interactive terminal; off-TTY forms read line input from stdin.

```bash
# Prompt for missing command inputs
gemini-api agent run --interactive

# Open the guided configuration form
gemini-api configure --interactive

# Explicitly launch the terminal command explorer
gemini-api explore
```

### Agent mode and structured errors
Agent mode turns on only when explicitly requested with `--agent-mode`; environment variables do not identify the caller.
In agent mode interactive prompts never launch, output defaults to TOON, and every failure — API errors and CLI usage errors alike — is one JSON envelope on stderr:
`--output-format json` and `--jq` use the same error envelope without requiring agent mode.

```json
{
  "error": "...",
  "error_type": "validation_error",
  "error_reason": "CLI_VALIDATION",
  "exit_code": 2,
  "message": "human-readable message",
  "hints": ["what to try next"]
}
```

`error_type` is one of `authentication_error`, `authorization_error`, `not_found`, `validation_error`, `rate_limit_error`, `server_error`, `api_error`, `connection_error`, `protocol_error`, `service_disabled`, `billing_disabled`, `runtime_error`, `unsupported_error`, `async_failed`, `async_timeout`, `async_unknown_state`. Classification reads the structured reason code at `$.details[*].reason`, then `$.status` in the error body (resolved against the nested `error` object when the body has one) before HTTP status, so a declared credential reason sent with HTTP 400 is not mistaken for request validation. `error_reason` carries the reason code found there, verbatim from a declared carrier when no declared rule matches it; it is absent for status-only API errors. Status-less local failures may use `CLI_VALIDATION`, `CLI_CONNECTION`, `CLI_PROTOCOL`, `CLI_RUNTIME`, `CLI_UNAVAILABLE`, `CLI_AUTHENTICATION`, or the async polling reasons `CLI_ASYNC_FAILED`, `CLI_ASYNC_TIMEOUT`, and `CLI_ASYNC_UNKNOWN_STATE`. `hints` preserves server guidance first, adds the most specific local taxonomy guidance, then typed CLI and command-specific guidance, removing exact duplicates. `exit_code` is always the code for the final `error_type` shown in the envelope: 1 runtime, 2 usage, or 3 authentication/authorization.

### Lists, streams, and files

List commands accept `--all` to fetch every page and stream results as they arrive (one JSON value per line with `--output-format json`; `--max-pages N` bounds the walk).

Structured output and agent mode never write pagination hints to stderr; if a later page fails or the server repeats a cursor, the command exits non-zero after the pages already written.

```bash
gemini-api agent list --all --output-format json
```

Streaming commands write each event as it arrives (one JSON object per line with `--output-format json`; a declared streamed projection prints just the selected text, e.g. `/data/delta/text`):

```bash
gemini-api agent run "Analyze market trends for Q3" --agent deep-research-preview-04-2026 --stream --output-format json
```

Commands that produce media write the file and print only its path on stdout (`--out <path-or-dir>` chooses the location, default `./gemini-image-{timestamp}-{rand}.{ext}`; `--raw-response` prints the API response instead):

```bash
gemini-api image "a lighthouse at sunset" --out ./output/
```

Long-running commands poll to a terminal response; human progress goes to stderr and machine-mode success keeps stderr silent. Add `--async` to `gemini-api video "a timelapse of a city at night" --async` to return its handle immediately, or tune foreground polling with `--poll-interval <duration>` and `--poll-timeout <duration>`. Resume an escaped or timed-out operation with `gemini-api agent status --id <id>`.
<!-- End For AI agents [agents] -->

<!-- Start Authentication [security] -->
## Authentication

Authentication credentials can be configured in four ways (in order of priority):

### 1. Command-line flags

Pass credentials directly as flags to any command:

```bash
gemini-api --api-key "$GEMINI_API_KEY" --access-token "$GEMINI_ACCESS_TOKEN" agent list
```

### 2. Environment variables

Set credentials via environment variables:

| Variable | Description |
|----------|-------------|
| `GEMINI_API_KEY` | Gemini API key sent as x-goog-api-key. |
| `GEMINI_ACCESS_TOKEN` | OAuth access token sent as a bearer Authorization header. |

### 3. OS Keychain (recommended for workstations)

Credentials are stored securely in your operating system's keychain when you run:

```bash
gemini-api configure
```

Secret credentials (tokens, API keys, passwords) are automatically stored in:
- **macOS**: Keychain
- **Linux**: GNOME Keyring / KWallet (via D-Bus Secret Service)
- **Windows**: Windows Credential Locker

If no keychain is available (e.g., in CI environments), credentials fall back to the config file.

### 4. Configuration file

Run the interactive `configure` command to store non-secret settings:

```bash
gemini-api configure
```

Configuration is stored in `~/.config/gemini-api/config.yaml`.
<!-- End Authentication [security] -->

<!-- Start Configuration [global-parameters] -->
## Configuration

`gemini-api configure` stores your settings in `~/.config/gemini-api/config.yaml`. You can run it interactively to set credentials and persistent preferences, or edit the config file directly.

For authentication credentials specifically, see [Authentication](#authentication).

### Global Parameters

Certain parameters are configured globally and applied to all commands that use them. These parameters can be set via CLI flags, environment variables, or the config file. Individual commands can override global values with their own flags when needed.

Priority: CLI flags > environment variables > config file

| Source | Example |
|--------|---------|
| CLI flag | `gemini-api --api-version v1beta agent list` |
| Environment variable | `GEMINI_API_VERSION=v1beta gemini-api agent list` |
| Config file | `gemini-api configure` |

#### Available Global Parameters

| Flag             | Type   | Description                                                                                                                        | Environment         |
| ---------------- | ------ | ---------------------------------------------------------------------------------------------------------------------------------- | ------------------- |
| `--api-version`  | string | Which version of the API to use. Defaults to v1beta (the only version covering the full interactions surface).                     | GEMINI_API_VERSION  |
| `--api-revision` | string | Interactions API revision to request. Omitted by default (matching the official SDKs), so the service serves its current revision. | GEMINI_API_REVISION |
| `--user-project` | string | Quota project header to send with Google GenAI API requests.                                                                       | GEMINI_USER_PROJECT |

### Example

```bash
# Set a global parameter via flag
gemini-api --api-version v1beta agent list

# Or set via environment variable
GEMINI_API_VERSION=v1beta gemini-api agent list

# Or configure globally (persisted to config file)
gemini-api configure
```
<!-- End Configuration [global-parameters] -->

<!-- Start Commands [operations] -->
## Commands

Commands are grouped the way `gemini-api --help` shows them. Every command accepts `--help`; body-bearing commands also accept `--schema` (exact request JSON Schema) and `--dry-run` (preview the request without sending it) — see [For AI agents](#for-ai-agents).

### Create

* [`generate`](docs/gemini-api_generate.md) - Text & multimodal generation (gemini-3.6-flash)

  ```bash
  # Choose a different model
  gemini-api generate "Write a haiku about APIs" --model gemini-2.5-pro
  # Generate with the default model (streams the reply)
  gemini-api generate "Explain concurrency in one sentence"
  ```

* [`image`](docs/gemini-api_image.md) - Generate or edit images (gemini-3.1-flash-image)

  ```bash
  # Generate an image (prints the written file path)
  gemini-api image "a lighthouse at sunset"
  # Write to a chosen path
  gemini-api image "product shot, white bg" --out shots/hero.png
  ```

* [`music`](docs/gemini-api_music.md) - Music generation (lyria-3-pro-preview)

  ```bash
  # Generate a clip
  gemini-api music "upbeat synthwave with a driving bassline"
  ```

* [`tts`](docs/gemini-api_tts.md) - Text to speech (gemini-3.1-flash-tts-preview) — _not in this build_: "tts" is not yet callable through the Interactions API — the TTS models (gemini-3.1-flash-tts-preview) reject interaction requests and audio-modality speech needs the classic generateContent speech config. Verified live 2026-08-13
* [`video`](docs/gemini-api_video.md) - Generate & edit video conversationally (gemini-omni-flash-preview)

  ```bash
  # Return the interaction ID immediately; poll it yourself
  gemini-api video "a timelapse of a city at night" --async
  # Generate a video (polls until done, prints the written file path)
  gemini-api video "a timelapse of a city at night"
  ```

### Understand

* [`analyze`](docs/gemini-api_analyze.md) - Ask questions about video, audio, PDF, or image files — _not in this build_: "analyze" is supported by the Interactions API (image, audio, document, and video content inputs) but needs the CLI's file-input adapter, which is not in this build yet. Meanwhile pass file content parts via "gemini-api agent run --body"
* [`embed`](docs/gemini-api_embed.md) - Vector embeddings (gemini-embedding-2) — _not in this build_: "embed" needs the classic GenAI API surface, which is not part of this interactions-only build
* [`tokens`](docs/gemini-api_tokens.md) - Count tokens without generating — _not in this build_: "tokens" needs the classic GenAI API surface, which is not part of this interactions-only build
* [`transcribe`](docs/gemini-api_transcribe.md) - Audio/video → text (timestamps, captions) — _not in this build_: "transcribe" is supported by the Interactions API (audio and video content inputs) but needs the CLI's file-input adapter, which is not in this build yet. Meanwhile pass file content parts via "gemini-api agent run --body"

### Manage

* [`agent`](docs/gemini-api_agent.md) - Run interactions with Gemini models or managed agents, and manage agent definitions
  * [`run`](docs/gemini-api_agent_run.md) - Run an interaction with a Gemini model or a managed agent

    ```bash
    # Run a managed agent by ID
    gemini-api agent run "Analyze market trends for Q3" --agent deep-research-preview-04-2026
    # Start a background run, then poll with "agent status"
    gemini-api agent run "Write a detailed research report on solar batteries" --background
    # Run a model interaction (streams the reply)
    gemini-api agent run "Explain the difference between concurrency and parallelism" --model gemini-3.6-flash
    ```

  * [`list`](docs/gemini-api_agent_list.md) - List managed agent definitions
  * [`create`](docs/gemini-api_agent_create.md) - Create a managed agent definition
  * [`delete`](docs/gemini-api_agent_delete.md) - Delete a managed agent definition by ID
  * [`get`](docs/gemini-api_agent_get.md) - Get a managed agent definition by ID
  * [`delete-interaction`](docs/gemini-api_agent_delete-interaction.md) - Delete an interaction by interaction ID
  * [`status`](docs/gemini-api_agent_status.md) - Get status and output of an interaction by interaction ID
  * [`cancel`](docs/gemini-api_agent_cancel.md) - Cancel an in-progress interaction by interaction ID
* [`batch`](docs/gemini-api_batch.md) - Async batch jobs at reduced cost — _not in this build_: "batch" needs the classic GenAI Batches API, which is not part of this interactions-only build
* [`configure`](docs/gemini-api_configure.md) - Configure authentication, global parameters, and preferences
* [`files`](docs/gemini-api_files.md) - Upload / list / download / delete media (48h TTL)
  * [`list`](docs/gemini-api_files_list.md) - Lists the metadata for `File`s owned by the requesting project.
  * [`delete`](docs/gemini-api_files_delete.md) - Deletes the `File`.
  * [`get`](docs/gemini-api_files_get.md) - Gets the metadata for the given `File`.
  * [`register`](docs/gemini-api_files_register.md) - Registers a Google Cloud Storage files with FileService. The user is expected to provide Google Cloud Storage URIs and will receive a File resource for each URI in return. Note that the files are not copied, just registered with File API. If one file fails to register, the whole request fails.
* [`models`](docs/gemini-api_models.md) - Full model operations — list and get model metadata, embed, count tokens, and generate with complete request control
  * [`list`](docs/gemini-api_models_list.md) - Lists the [`Model`s](https://ai.google.dev/gemini-api/docs/models/gemini) available through the Gemini API.
  * [`get`](docs/gemini-api_models_get.md) - Gets information about a specific `Model` such as its version number, token limits, [parameters](https://ai.google.dev/gemini-api/docs/models/generative-models#model-parameters) and other metadata. Refer to the [Gemini models guide](https://ai.google.dev/gemini-api/docs/models/gemini) for detailed model information.

### Advanced

* [`docs`](docs/gemini-api_docs.md) - Gemini API documentation & guides — _not in this build_: "docs" curated guides are not part of this build yet. Meanwhile browse https://ai.google.dev/gemini-api/docs
* [`triggers`](docs/gemini-api_triggers.md) - Schedule and manage cron triggers that run managed agents
  * [`list`](docs/gemini-api_triggers_list.md) - List triggers for a project
  * [`delete`](docs/gemini-api_triggers_delete.md) - Delete a trigger by ID
  * [`get`](docs/gemini-api_triggers_get.md) - Get a trigger by ID
  * [`update`](docs/gemini-api_triggers_update.md) - Update a trigger by ID
  * [`list-executions`](docs/gemini-api_triggers_list-executions.md) - List executions for a trigger
  * [`run`](docs/gemini-api_triggers_run.md) - Run a trigger immediately
* [`webhooks`](docs/gemini-api_webhooks.md) - Manage webhook endpoints and signing secrets for event delivery
  * [`list`](docs/gemini-api_webhooks_list.md) - List webhook endpoints
  * [`create`](docs/gemini-api_webhooks_create.md) - Create a webhook endpoint
  * [`delete`](docs/gemini-api_webhooks_delete.md) - Delete a webhook by ID
  * [`get`](docs/gemini-api_webhooks_get.md) - Get a webhook by ID
  * [`update`](docs/gemini-api_webhooks_update.md) - Update a webhook by ID
  * [`ping`](docs/gemini-api_webhooks_ping.md) - Send a ping event to a webhook
  * [`rotate-signing-secret`](docs/gemini-api_webhooks_rotate-signing-secret.md) - Rotate the signing secret for a webhook

### Additional commands

* [`credentials`](docs/gemini-api_credentials.md) - Operations for credentials
  * [`list`](docs/gemini-api_credentials_list.md) - Lists credentials for a project.
  * [`create`](docs/gemini-api_credentials_create.md) - Creates a credential.
  * [`delete`](docs/gemini-api_credentials_delete.md) - Deletes a credential. Fails if referenced by active triggers.
  * [`get`](docs/gemini-api_credentials_get.md) - Gets metadata of a single credential (no secret fields).
  * [`update`](docs/gemini-api_credentials_update.md) - Updates a credential.
* [`environments`](docs/gemini-api_environments.md) - Operations for environments
  * [`list`](docs/gemini-api_environments_list.md) - Lists environments.
  * [`create`](docs/gemini-api_environments_create.md) - Creates an environment.
  * [`delete`](docs/gemini-api_environments_delete.md) - Deletes an environment.
  * [`get`](docs/gemini-api_environments_get.md) - Gets an environment.
  * [`files`](docs/gemini-api_environments_files.md) - Operations for files
    * [`list`](docs/gemini-api_environments_files_list.md) - Retrieves file metadata or directory contents from an environment's snapshot. To download file contents directly, pass ?alt=media or use the files.download helper.
<!-- End Commands [operations] -->

<!-- Start Request Body Input [stdinpiping] -->
## Request Body Input

Commands that accept a request body take it three ways, with a clear priority chain. The examples use `gemini-api environments create`; every body-bearing command works the same way and prints its exact request schema with `--schema`.

### `--body` flag

Provide the entire request body as a JSON string:

```bash
gemini-api environments create --body '{"network":{"allowlist":[{"domain":"github.com","transform":[{"Authorization":"Bearer your-token"}]},{"domain":"*.googleapis.com"}]}}'
```

### Stdin piping (lowest priority)

Pipe JSON into any command that accepts a request body:

```bash
echo '{"network":{"allowlist":[{"domain":"github.com","transform":[{"Authorization":"Bearer your-token"}]},{"domain":"*.googleapis.com"}]}}' | gemini-api environments create
```

This is useful for chaining commands, reading from files, or scripting:

```bash
# Read body from a file
gemini-api environments create < request.json

# Pipe from another command
curl -s https://example.com/request.json | gemini-api environments create
```

### Priority

When multiple input methods are used, the priority is:

| Priority | Source | Description |
|----------|--------|-------------|
| 1 (highest) | Individual flags | A field flag always wins |
| 2 | `--body` flag | Whole-body JSON via flag |
| 3 (lowest) | Stdin | Piped JSON input |
<!-- End Request Body Input [stdinpiping] -->

<!-- Start Server Selection [server] -->
## Server Selection

### Override Server URL

Use `--server-url` to override the server URL entirely:

```bash
gemini-api --server-url https://custom-api.example.com agent list
```

**Precedence**: `--server-url` > default
<!-- End Server Selection [server] -->

<!-- Start Output Formats [output-formats] -->
## Output Formats

Every command supports a `--output-format` flag that controls how the response is rendered to stdout.

### Available formats

| Format | Flag | Description |
|--------|------|-------------|
| Pretty | `--output-format pretty` (default) | Aligned key-value pairs with color, nested indentation. Human-readable at a glance. |
| JSON | `--output-format json` | JSON output. Passthrough when the response is already JSON (preserves original field order and numeric precision). Falls back to typed marshaling otherwise. |
| YAML | `--output-format yaml` | YAML output via standard marshaling. |
| Table | `--output-format table` | Tabular output for array responses. |
| TOON | `--output-format toon` | [Token-Oriented Object Notation](https://github.com/toon-format/spec) — a compact, line-oriented format that typically uses 30–60% fewer tokens than JSON. Well-suited for piping responses into LLM prompts. |

```bash
# Default pretty output
gemini-api agent list

# Machine-readable JSON
gemini-api agent list --output-format json

# TOON for LLM-friendly compact output
gemini-api agent list --output-format toon

# Pipe JSON to jq without using --output-format
gemini-api agent list --output-format json | jq '.'
```

### jq filtering

Use `--jq` to filter or transform the response inline using a [jq](https://jqlang.org) expression. This always outputs JSON and overrides `--output-format`:

```bash
# Extract a single field
gemini-api agent list --jq '.'

# Reshape with any jq program; --raw-output prints string results as plain text (like jq -r)
gemini-api agent list --jq '.' --raw-output
```

### Color control

Use `--color` to control terminal colors:

| Value | Behavior |
|-------|----------|
| `auto` (default) | Color when stdout is a TTY, plain text otherwise |
| `always` | Always colorize |
| `never` | Never colorize |

The `NO_COLOR` and `FORCE_COLOR` environment variables are also respected.

### Streaming and pagination

When using `--all` (pagination) or streaming operations, output is written incrementally as items arrive:

| Format | Streaming behavior |
|--------|-------------------|
| `json` | One compact JSON object per line ([NDJSON](https://github.com/ndjson/ndjson-spec)) |
| `yaml` | YAML documents separated by `---` |
| `toon` | One TOON-encoded object per block, separated by blank lines |
| `pretty` (default) | Pretty-printed items separated by blank lines |
<!-- End Output Formats [output-formats] -->

<!-- Start Server-Sent Event Streaming [eventstreaming] -->
## Server-Sent Event Streaming

Some operations return server-sent events (SSE). These are streamed to the terminal in real-time, with each event output as a separate JSON object (one per line).

```bash
# Stream events in JSON format
gemini-api agent run "Analyze market trends for Q3" --agent deep-research-preview-04-2026 --stream --output-format json

# Filter streaming events with jq
gemini-api agent run "Analyze market trends for Q3" --agent deep-research-preview-04-2026 --stream --output-format json --jq '.'
```

Events are output as they arrive. Use `Ctrl+C` to stop streaming.

For operation commands with a declared streamed projection, the selected string is written raw as it arrives. When the command exposes a stream toggle flag, its default decides the response shape — the command's help says whether to pass `--stream=false` for one complete JSON response (streaming on by default) or `--stream` to request a streamed response (off by default). Use `-o json` to keep each full streamed event.
<!-- End Server-Sent Event Streaming [eventstreaming] -->

<!-- Start Pagination [pagination] -->
## Pagination

Some operations in this CLI support automatic pagination. These operations accept `--all` to automatically fetch all pages and stream results incrementally.

### Basic usage

```bash
# Fetch a single page (default behavior)
gemini-api agent list

# Automatically fetch all pages
gemini-api agent list --all
```

### Limiting pages

Use `--max-pages` with `--all` to cap the number of pages fetched. A negative value is invalid; `0` means unlimited. Passing `--max-pages` without `--all` is an error.

```bash
# Fetch at most 5 pages
gemini-api agent list --all --max-pages 5
```

### Output formats

When using `--all`, output is streamed as each page is fetched. Operations whose pagination declaration names an `outputs.results` array emit one item at a time. Other operations emit one complete page object at a time, preserving the single-page response shape and any continuation cursor.

| Format | Behavior |
|--------|----------|
| `--output-format json` | One JSON object per line ([NDJSON](https://github.com/ndjson/ndjson-spec)) |
| `--output-format yaml` | YAML documents separated by `---` |
| `--output-format toon` | One TOON-encoded block per item, separated by blank lines |
| Default (pretty) | Pretty-printed items separated by blank lines |

```bash
# Stream all results as NDJSON
gemini-api agent list --all --output-format json

# Pipe to jq for further processing
gemini-api agent list --all --output-format json | jq '.'

# Use the built-in --jq flag
gemini-api agent list --all --jq '.'
```

### How it works

Under the hood, `--all` calls the operation once, then follows the underlying `Next()` pagination closure to fetch subsequent pages. Results are written to stdout as they arrive rather than buffered in memory, so this works well even with large result sets.

Without `--all`, paginated operations behave like any other command — pass cursor, page, offset, or limit flags manually and get a single page of results. In pretty or table output, a cursor response that proves another page exists prints a hint on stderr. JSON, YAML, TOON, `--jq`, and agent mode keep stderr silent on success. Offset/limit responses do not guess from a full result page. Cursor operations that declare both a results array and a mutable limit also suppress the hint because the client cannot safely reproduce the SDK's runtime limit check.

Pagination can fail after earlier pages have already been written. A later-page API failure or a repeated/cyclic continuation cursor stops with a non-zero exit status; callers should treat stdout as partial whenever the command exits non-zero. `--all` tracks cursor values and stops before issuing another request when the server repeats one; the same applies to next URLs when the target generator supports them.
<!-- End Pagination [pagination] -->

<!-- Start Retries [retries] -->
## Retries

Some operations in this CLI support automatic retries with exponential backoff.

### Configure retries

Retry flags are supported but intentionally omitted from `--help`. For persistent agent configuration, use `~/.config/gemini-api/config.yaml`:

```yaml
timeout: 30s
no_retries: false
retry_connection_errors: true
retry_max_elapsed_time: 1m
# retry_config replaces the whole policy (overrides retry_max_elapsed_time):
# retry_config: '{"strategy":"backoff","backoff":{"initialInterval":500,"maxInterval":60000,"exponent":1.5,"maxElapsedTime":300000}}'
```

The equivalent hidden flags are `--no-retries`, `--retry-config`, `--retry-connection-errors`, and `--retry-max-elapsed-time`.

### Retry-After

`Retry-After` (integer seconds or an RFC1123 date) and `retry-after-ms` override the next computed interval. With the `backoff` strategy, a server-directed wait that exceeds the remaining `maxElapsedTime` budget is not slept; the last response is returned. With `attempt-count-backoff`, `maxRetries` bounds attempts, while `timeout` bounds wall-clock time; `maxElapsedTime` does not apply.

### Timeout

`timeout` and `--timeout` bound the whole operation, including retry sleeps:

```bash
gemini-api agent list --timeout 30s
```

**Precedence**: `--no-retries` > `--retry-config` > individual flags > config file > API specification defaults.
<!-- End Retries [retries] -->

<!-- Start Error Handling [errors] -->
## Error Handling

The CLI uses standard exit codes to indicate success or failure:

| Exit Code | Meaning |
|-----------|---------|
| `0` | Success |
| `1` | Runtime/API failure |
| `2` | Usage or input failure |
| `3` | Authentication or authorization failure |

On success, the response data is printed to **stdout** as JSON. On failure, error details are printed to **stderr**.

```bash
# Capture output and handle errors
gemini-api agent list --output-format json > output.json 2> error.log
if [ $? -ne 0 ]; then
  echo "Error occurred, see error.log"
fi
```
In pretty mode, each error is printed once as `Error (<type>): <message>`, followed by its reason/HTTP status, actionable `Fix:` bullets, and only non-duplicative residual details.

In agent mode, or with explicit `--output-format json`, `--output-format toon`, or `--jq` machine output, stderr is one classified JSON envelope with `exit_code`, `error_type`, optional `error_reason`, `message`, `hints`, and optional `status_code` — see [For AI agents](#for-ai-agents).

`error_reason` is the structured reason code read from the error body at `$.details[*].reason`, then `$.status` (resolved against the nested `error` object when the body has one).
<!-- End Error Handling [errors] -->

<!-- Start Diagnostics [diagnostics] -->
## Diagnostics

The CLI includes two diagnostic flags available on all commands:

### Dry Run

Preview what would be sent without making any network calls:

```bash
gemini-api agent list --dry-run
```

In human output modes, stdout is empty and the `[DRY-RUN]` block goes to stderr. It includes:
- HTTP method and URL
- Request headers (sensitive values redacted)
- Request body preview (sensitive fields redacted)

With `--output-format json`, or with a caller-explicit `--jq`, stderr is silent and stdout is NDJSON: one compact preview object per would-be request. The jq filter is not applied, and command-declared jq presets do not select the JSON protocol.

```json
{"dry_run":true,"request":{"method":"POST","url":"https://…","headers":{"Accept":["application/json"],…},"body":<JSON value | string | null>}}
```

JSON bodies remain structured; text bodies are strings; binary bodies are `"<bytes:N>"`; absent bodies are `null`. Headers retain all values as arrays, with credentials replaced by `[REDACTED]`. Dry-run never reads the OS keychain, but credentials supplied by flag, environment, or config file still appear redacted. The command exits successfully without contacting the API.

Local mutation commands emit one `{"dry_run":true,"local":true,"command":"…","message":"…"}` object in place of a preview; filter with `select(.request)` or `select(.local)`.

### Debug

Log request and response diagnostics while running normally:

```bash
gemini-api agent list --debug
```

Debug output goes to stderr and includes:
- Request method, URL, headers, and body preview
- Response status, headers, and body preview
- Transport errors (if any)

The command still executes normally and produces its regular output on stdout.

### Flag Precedence

If both `--dry-run` and `--debug` are set, `--dry-run` takes precedence and no network calls are made.

### Security

Sensitive information is automatically redacted in diagnostic output:
- **Headers**: `Authorization`, `Cookie`, `Set-Cookie`, `X-API-Key`, and other security headers show `[REDACTED]`
- **Body**: JSON fields named `password`, `secret`, `token`, `api_key`, `client_secret`, etc. show `[REDACTED]`
- **Binary data**: binary media and canonical base64 strings are replaced with `<bytes:N>`
- **URL query**: credential-like query parameters are replaced with `[REDACTED]`

Diagnostic output should still be treated as potentially sensitive operational data.
<!-- End Diagnostics [diagnostics] -->

<!-- Placeholder for Future Speakeasy SDK Sections -->

# Development

## Maturity

This CLI is in beta, and there may be breaking changes between versions without a major version update. Therefore, we recommend pinning usage
to a specific package version. This way, you can install the same version each time without breaking changes unless you are intentionally
looking for the latest version.

## Contributions

This CLI is generated programmatically. Edits to generated files are overwritten on regeneration. To customize it:

- **Configuration and behavior:** Use [OpenAPI overlays](https://www.speakeasy.com/docs/prep-openapi/overlays/create-overlays) in the Speakeasy workflow with `x-speakeasy-*` extensions (for example, `x-speakeasy-cli-commands`) to define commands, flags, help text, examples, authentication, and grouping.
- **Persistent code changes:** Store unified diffs as [patch files](https://www.speakeasy.com/docs/sdks/customize/code/patch-files/patch-files) at `.speakeasy/patches/<path-of-generated-file>.patch`; they are re-applied on every generation.
- **Hand-written commands:** Add them under `internal/cli/custom/`; the scaffold is generated once and never overwritten.

### CLI Created by [Speakeasy](https://www.speakeasy.com/?utm_source=google3-/third-party/gemini-api-cli&utm_campaign=cli)
