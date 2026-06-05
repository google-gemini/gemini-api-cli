# Experimental Gemini API CLI — Documentation

> [!CAUTION]
> **Disclaimer**: This is not a supported Google product.

> Develop, test, and deploy Gemini Agents. Run interactions across every model and modality.

---

## Installation

### Via Install Script (Recommended)

You can install the pre-compiled binary directly (no cloning required) using this single-line command:

```bash
curl -fsSL https://raw.githubusercontent.com/google-gemini/gemini-api-cli/main/scripts/install.sh | bash
```

### From Source (via npm)

```bash
git clone https://github.com/google-gemini/gemini-api-cli.git
cd gemini-api-cli
npm install -g .
```

**Requirements:** Bun ≥ 1.1 or Node.js ≥ 22

---

## Authentication

```bash
# Option 1: Environment variable (recommended)
export GEMINI_API_KEY="your-api-key"

# Option 2: Flag (works with any command)
gemini-api run "Hello" --api-key "your-api-key"
```

Get your API key at [aistudio.google.com](https://aistudio.google.com/).

---

## Quick Start

```bash
# Run a prompt against a model
gemini-api run "What is the capital of France?"

# Use a specific model
gemini-api run "Explain quantum computing" --model gemini-3.1-pro-preview

# Scaffold an agent
gemini-api agents init my-agent
cd my-agent

# Edit agent.yaml and AGENTS.md, then test locally
gemini-api agents test --prompt "Hello, what can you do?"

# Deploy to the platform
gemini-api agents create

# Test the deployed agent
gemini-api run "Hello" --agent my-agent

# List and manage agents
gemini-api agents list
gemini-api agents delete my-agent
```

---

## Commands

### `gemini-api run <prompt>`

Create an interaction against a model or agent.

```bash
gemini-api run "What is the capital of France?"
gemini-api run "Explain this code" --model gemini-3.1-pro-preview
gemini-api run "Analyze my data" --agent my-data-analyst
```

| Flag | Short | Type | Default | Description |
|---|---|---|---|---|
| `<prompt>` | | positional | — | Input prompt. |
| `--model` | `-m` | string | `gemini-3-flash-preview` | Model to use |
| `--agent` | `-a` | string | — | Agent to use (overrides `--model`) |
| `--input` | `-i` | string[] | — | Multimodal input: `image:path`, `audio:path`, `video:path`, `document:path` |
| `--output` | `-o` | string | — | Save generated media to file |

| `--previous-interaction-id` | `-p` | string | — | Continue from previous interaction |
| `--system-instruction` | `-s` | string | — | System instruction |
| `--response-modality` | | enum[] | — | `text`, `image`, `audio`, `video`, `document` |
| `--response-mime-type` | | string | — | MIME type for response |
| `--tool` | | string[] | — | Tool declaration (can be repeated): `code_execution`, `google_search`, `mcp_server:name:url` |
| `--source` | | string[] | — | Environment source (can be repeated): `inline:target:content`, `github:url:target`, `gcs:source:target` |

| `--voice` | | string | — | TTS voice name |
| `--language` | | string | — | TTS language code |
| `--aspect-ratio` | | enum | — | Image aspect ratio (e.g., `16:9`) |
| `--image-size` | | enum | — | `512`, `1K`, `2K`, `4K` |
| `--edit-strength` | | float | — | How much to change the original image (0.0 to 1.0) |
| `--mask` | | string | — | Path to a mask image for localized editing |
| `--service-tier` | | enum | — | `flex`, `standard`, `priority` |
| `--json` | `-j` | boolean | `false` | Output raw SSE events as JSONL |
| `--dry-run` | | boolean | `false` | Print curl command and exit |

| `--api-key` | | string | `$GEMINI_API_KEY` | API key |
| `--base-url` | | string | `$GEMINI_API_BASE_URL` | Override API base URL |


**Examples:**

```bash
# Model interaction
gemini-api run "Write a haiku about code"

# Image understanding
gemini-api run "What's in this image?" --input image:photo.jpg

# Image editing
gemini-api run "Add a red hat" --input image:person.jpg --response-modality image --output with_hat.jpg

# Image generation
gemini-api run "A cat in space" --model gemini-3.1-flash-image-preview --output cat.png

# Text-to-speech
gemini-api run "Hello my name is gemini, i am a large language model from google. I can help you with a wide range of tasks." --model gemini-3.1-flash-tts-preview --voice Kore --output hello.wav

# With tools
gemini-api run "What is the weather?" --tool google_search --tool code_execution

# With sources (e.g. antigravity-preview-05-2026)
gemini-api run "Generate test" --agent antigravity-preview-05-2026 --source "inline:/.agents/README.md:# Instructions" --source "github:https://github.com/user/repo:/.agents"

# Multi-turn
gemini-api run "Remember the word: banana"
# → interaction_id: int_abc123
gemini-api run "What word?" --previous-interaction-id int_abc123

# Dry run
gemini-api run "Hello" --dry-run
```

---

### `gemini-api agents`

Manage the full agent lifecycle.

#### `gemini-api agents init <name>`

Scaffold a new agent project.

```bash
gemini-api agents init my-agent
gemini-api agents init my-agent --base-agent antigravity-preview-05-2026
gemini-api agents init my-agent --from-template https://github.com/google-gemini/Gemini-API-Agent-Templates/tree/main/customer-data-analysis-agent
```

| Flag | Type | Default | Description |
|---|---|---|---|
| `<name>` | positional | — | Agent directory name |
| `--base-agent` | string | `antigravity-preview-05-2026` | Base model (only 'antigravity-preview-05-2026' is supported) |
| `--from-template` | string | — | Git or GCS URL to scaffold from |

#### `gemini-api agents create`

Deploy agent from current directory.

```bash
gemini-api agents create
gemini-api agents create --path ./my-agent
gemini-api agents create --dry-run
```

| Flag | Type | Default | Description |
|---|---|---|---|
| `--path` | string | `.` | Agent directory |
| `--base-env` | string | — | Override base environment |
| `--dry-run` | boolean | `false` | Print curl |
| `--json` | boolean | `false` | JSON output |

#### `gemini-api agents list`

```bash
gemini-api agents list
gemini-api agents list --json
gemini-api agents list --dry-run
```

#### `gemini-api agents get <id>`

```bash
gemini-api agents get my-agent
gemini-api agents get my-agent --json
gemini-api agents get my-agent --dry-run
```

#### `gemini-api agents delete <id>`

```bash
gemini-api agents delete my-agent
gemini-api agents delete my-agent --force
gemini-api agents delete my-agent --dry-run
```

| Flag | Type | Default | Description |
|---|---|---|---|
| `--force` | boolean | `false` | Skip confirmation |

#### `gemini-api agents test`

Run an interaction using local agent config.

```bash
gemini-api agents test --prompt "Hello"
gemini-api agents test --prompt "Hello" --path ./my-agent
gemini-api agents test --prompt "Continue" --previous-interaction-id int_abc --environment env_xyz
gemini-api agents test --prompt "Hello" --dry-run
```

| Flag | Type | Default | Description |
|---|---|---|---|
| `--prompt` | string | — | Input prompt (required) |
| `--path` | string | `.` | Agent directory |

| `--previous-interaction-id` | string | — | Multi-turn |
| `--environment` | string | — | Use existing environment |
| `--json` | boolean | `false` | JSON output |
| `--dry-run` | boolean | `false` | Print curl |


---

### `gemini-api files`

Manage environment files.

#### `gemini-api files download <env-id>`

Download all files from the environment as a snapshot and extract them into a folder named `snapshot_<env-id>` in the output directory.

```bash
gemini-api files download env_xyz789
gemini-api files download env_xyz789 --output ./results
gemini-api files download env_xyz789 --dry-run
```

| Flag | Type | Default | Description |
|---|---|---|---|
| `--output` | string | `./` | Output directory |

---

## Agent Configuration

### Directory Structure

```
my-agent/
├── agent.yaml       # Configuration (not inlined)
├── AGENTS.md        # System instructions (inlined to /.agents/AGENTS.md)
├── .env             # Credentials (inlined to /credentials/.env)
├── skills/          # Custom skills (all files inlined recursively)
└── workspace/       # Files seeded into remote environment (all files inlined recursively)
```

### `agent.yaml`

```yaml
# Required
id: my-agent
base_agent: antigravity-preview-05-2026

# Optional
description: "A data analyst agent"
instructions: "You are a helpful assistant."

# Tools
tools:
  - type: code_execution
  - type: google_search

# Environment
environment: remote

# OR derive from existing environment
# base_environment: env_abc123
```

### `.env`

Credentials file. If it contains non-empty values, it's inlined during `agents test` and `agents create` to `/credentials/.env` in the agent environment. The agent can then `source /credentials/.env` to load the values.

### `AGENTS.md`

Agent instructions in markdown. Uploaded to the remote environment and loaded before running. Use for long instructions — easier to read, diff, and version than `instructions` in `agent.yaml`.

### `workspace/`

Files seeded into the remote environment at `/.agents/workspace/`. All files in this directory are inlined into the API request when running `agents test` or `agents create`.

**File handling:**

| File type | How it's sent | Example extensions |
|---|---|---|
| Text files | Inlined as UTF-8 strings | `.md`, `.py`, `.csv`, `.json`, `.yaml` |
| Binary files | Base64-encoded with `"encoding": "base64"` | `.pdf`, `.png`, `.jpg`, `.mp3`, `.wav`, `.zip` |
| Files > 1 MB | Skipped | — |

> **Note:** Only `AGENTS.md`, `.env`, `workspace/`, and `skills/` are inlined from the agent directory. All other root-level files and directories are ignored.

Binary files are automatically detected by extension. The following are treated as binary:
- Images: `.png`, `.jpg`, `.jpeg`, `.webp`, `.gif`, `.bmp`, `.tiff`, `.heic`, `.heif`
- Audio: `.wav`, `.mp3`, `.aac`, `.ogg`, `.flac`, `.opus`, `.m4a`
- Video: `.mp4`, `.mov`, `.avi`, `.webm`, `.wmv`
- Documents: `.pdf`
- Archives: `.zip`, `.tar`, `.gz`, `.bz2`, `.xz`, `.7z`

### `environment` (in `agent.yaml`)

Controls the sandbox environment for the agent:

```yaml
# Enable a managed sandbox environment
environment: remote

# OR reuse an existing environment by ID
# base_environment: env_abc123
```

When `environment` is `"remote"`, the API provisions a sandbox with code execution capabilities. Workspace files, skills, and credentials are seeded into it before the agent runs.

You can also specify a structured config object to configure GCS/GitHub `sources`, establish `network` allowlists, and inject secret credentials securely via header `transform` rules:

```yaml
environment:
  type: "remote"
  # Sources to copy or clone into the environment on startup
  sources:
    - type: "gcs"
      source: "gs://my-bucket-name/folder/"
      target: ".agents/workspace"
    - type: "github"
      source: "https://github.com/my-username/my-repo"
      target: ".agents/workspace/repo"

  # Outbound network security policies and headers injection (secrets)
  network:
    allowlist:
      - domain: "api.github.com"
        transform:
          # Injects Authorization header dynamically at egress proxy level
          Authorization: "Bearer your-github-token"
      - domain: "storage.googleapis.com"
        transform:
          Authorization: "Bearer your-gcloud-oauth-token"
      - domain: "*.wikipedia.org"
      # Catch-all rule (optional) to allow other traffic without header injection
      - domain: "*"
```

---

## Tools



### `agent.yaml` Tools

```yaml
tools:
  - type: code_execution
  - type: google_search
    search_types: [web_search, image_search]
  - type: url_context
```

---

## Output Modes

### Normal (Default)

Optimized for clean, readable output and valid Markdown parsing. Thoughts are concise, tool calls are consolidated into single lines, and the final response text is printed without leading indentation:

```
[thought]
[tool] write_file(path="hello.py") -> {"success":true}
[code] python3 hello.py -> "Hello, World!"
[text]
I have created a Python script named `hello.py` and successfully executed it.

Here is the content of `hello.py`:
```python
print("Hello, World!")
```
```

### Verbose (`--verbose` / `-v`)

Optimized for automated parsing by agents. Steps are output as completed single-line JSON objects, followed by the final `{interaction}` metadata as a JSON line:

```json
{"index":0,"type":"thought","status":"completed","thought":{"signature":"EvQBCvEBAQw5..."}}
{"index":1,"type":"function_call","status":"completed","function_call":{"name":"write_file","arguments":{"path":"hello.py","content":"print(\"Hello, World!\")"}}}
{"index":2,"type":"function_result","status":"completed","function_result":{"name":"write_file","result":{"success":true}}}
{"interaction":{"id":"v1_ChdIcjRp...","status":"completed","usage":{"total_tokens":9131,"total_input_tokens":8970,"total_output_tokens":161},"object":"interaction"}}
```

### JSON (`--json`)

Raw streamed SSE events as JSONL (one raw event per line):

```jsonl
{"event_type":"interaction.created","interaction":{...}}
{"index":0,"step":{"type":"thought"},"event_type":"step.start"}
{"index":0,"delta":{"signature":"EvQBC...","type":"thought_signature"},"event_type":"step.delta"}
{"event_type":"interaction.completed","interaction":{...}}
```

### Dry Run (`--dry-run`)

Prints the equivalent `curl` command and exits without making an API call.

---

## Multimodal I/O

### Input

```bash
gemini-api run "Describe this" --input image:photo.jpg
gemini-api run "Transcribe" --input audio:meeting.wav
gemini-api run "Summarize" --input document:report.pdf
```

### Output

```bash
gemini-api run "Draw a cat" --model gemini-3-pro-image-preview --output cat.png
gemini-api run "Read aloud" --model gemini-3.1-flash-tts-preview --voice Kore --output speech.wav
```

---

## Multi-Turn Conversations

```bash
# First turn
gemini-api run "Analyze the dataset"
# → interaction_id: int_abc123

# Second turn — continues the conversation
gemini-api run "Summarize in 3 bullets" --previous-interaction-id int_abc123
```

For agent tests with environments:

```bash
gemini-api agents test --prompt "Analyze data"
# → interaction_id: int_abc123
# → environment: env_xyz789

gemini-api agents test --prompt "Now chart it" \
  --previous-interaction-id int_abc123 \
  --environment env_xyz789
```

---

## Interaction Logging

Every interaction is automatically logged to `.gemini/logs/<interaction-id>.jsonl` in the current directory. Logs contain the request and reassembled response (SSE events combined into final content blocks).

```
.gemini/
└── logs/
    └── int_abc123.jsonl
```

Each file has 2 lines:
- **Line 1:** Request (model, input, tools, system instruction)
- **Line 2:** Response (outputs, usage, status)

Binary data (images, audio) is excluded from logs.

---

## Troubleshooting

### No API key

```
✗ No API key found.

  Try:
    export GEMINI_API_KEY="your-api-key"
    gemini-api run "Hello" --api-key "your-api-key"
```

### No agent.yaml

```
✗ No agent.yaml found in /home/user/project.

  Try:
    gemini-api agents init my-agent
    cd my-agent && gemini-api agents create
```

### Model not found

```
✗ API error (400): Model 'nonexistent' not found.

  Try:
    gemini-api run "Hello" --model gemini-3-flash-preview
```

### Debug output

Use `--verbose` to see request/response details:

```bash
gemini-api run "Hello" --verbose
```

### Preview requests

Use `--dry-run` to see the curl equivalent without making an API call:

```bash
gemini-api agents create --dry-run
```

---

## Environment Variables

| Variable | Description |
|---|---|
| `GEMINI_API_KEY` | API key for authentication |
| `GEMINI_API_BASE_URL` | Override API base URL |
| `AGENTS_WORKSPACE_PATH` | Target path prefix for inline files (default: `/.agents/`) |

---

## Models

| Model | Description |
|---|---|
| `gemini-3-flash-preview` | Frontier + search (default) |
| `gemini-3.1-pro-preview` | SOTA reasoning + multimodal |
| `gemini-3.1-pro-preview` | SOTA reasoning |
| `gemini-2.5-flash` | Hybrid reasoning, 1M context |
| `gemini-2.5-pro` | SOTA coding + reasoning |
| `gemini-3-pro-image-preview` | Image generation |
| `gemini-3.1-flash-image-preview` | Flash image generation |
| `gemini-2.5-flash-image` | Native image generation |
| `gemini-3.1-flash-tts-preview` | Text-to-speech |
| `gemini-2.5-flash-preview-tts` | TTS |
| `gemini-2.5-pro-preview-tts` | TTS (pro) |
| `gemini-2.5-computer-use-preview-10-2025` | Computer use |
| `lyria-3-clip-preview` | Music: clip generation |
| `lyria-3-pro-preview` | Music: full-song |

## Agents

| Agent | Description |
|---|---|
| `deep-research-preview-04-2026` | Deep Research (latest) |
| `deep-research-max-preview-04-2026` | Deep Research Max |

---

## License

Apache-2.0