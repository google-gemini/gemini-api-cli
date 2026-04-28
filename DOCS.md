# gemini-api CLI — Documentation

> Develop, test, and deploy Gemini Agents. Run interactions across every model and modality.

---

## Installation

### Standalone Binary (Recommended)

Download the pre-compiled binary for your platform:

```bash
# macOS (Apple Silicon)
curl -fsSL https://github.com/google-gemini/gemini-api-cli/releases/latest/download/gemini-api-darwin-arm64 -o /usr/local/bin/gemini-api
chmod +x /usr/local/bin/gemini-api

# macOS (Intel)
curl -fsSL https://github.com/google-gemini/gemini-api-cli/releases/latest/download/gemini-api-darwin-x64 -o /usr/local/bin/gemini-api
chmod +x /usr/local/bin/gemini-api

# Linux (x64)
curl -fsSL https://github.com/google-gemini/gemini-api-cli/releases/latest/download/gemini-api-linux-x64 -o /usr/local/bin/gemini-api
chmod +x /usr/local/bin/gemini-api
```

### npm

```bash
npm install -g @google/gemini-api-cli
```

### GCS (Internal/EAP)

```bash
gcloud storage cp gs://gemini-api-eap/agents-api/install.sh ./install.sh
bash ./install.sh
```

### From Source

```bash
git clone https://github.com/google-gemini/Gemini-API-Agent-Templates.git
cd Gemini-API-Agent-Templates/gemini-agents-cli
bun install && bun run compile
# Binary at dist/gemini-api
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
gemini-api run "Explain quantum computing" --model gemini-3-pro-preview

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
gemini-api run "Explain this code" --model gemini-3-pro-preview
gemini-api run "Analyze my data" --agent my-data-analyst
echo "Translate to French" | gemini-api run -
```

| Flag | Short | Type | Default | Description |
|---|---|---|---|---|
| `<prompt>` | | positional | — | Input prompt. Use `-` for stdin. |
| `--model` | `-m` | string | `gemini-3-flash-preview` | Model to use |
| `--agent` | `-a` | string | — | Agent to use (overrides `--model`) |
| `--input` | `-i` | string[] | — | Multimodal input: `image:path`, `audio:path`, `video:path`, `document:path` |
| `--output` | `-o` | string | — | Save generated media to file |
| `--stream` | | boolean | `true` | Stream response via SSE |
| `--no-stream` | | boolean | | Disable streaming |
| `--store` | | boolean | `false` | Store interaction for later retrieval |
| `--previous-interaction-id` | `-p` | string | — | Continue from previous interaction |
| `--system-instruction` | `-s` | string | — | System instruction |
| `--response-modality` | | enum[] | — | `text`, `image`, `audio`, `video`, `document` |
| `--response-format` | | JSON | — | JSON schema for structured output |
| `--response-mime-type` | | string | — | MIME type for response |
| `--tool` | | string[] | — | Tool declarations (see Tools section) |
| `--tool-choice` | | enum | — | `auto`, `any`, `none`, `validated` |
| `--voice` | | string | — | TTS voice name |
| `--language` | | string | — | TTS language code |
| `--aspect-ratio` | | enum | — | Image aspect ratio (e.g., `16:9`) |
| `--image-size` | | enum | — | `512`, `1K`, `2K`, `4K` |
| `--edit-strength` | | float | — | How much to change the original image (0.0 to 1.0) |
| `--mask` | | string | — | Path to a mask image for localized editing |
| `--service-tier` | | enum | — | `flex`, `standard`, `priority` |
| `--json` | `-j` | boolean | `false` | Output raw SSE events as JSONL |
| `--dry-run` | | boolean | `false` | Print curl command and exit |
| `--verbose` | `-v` | boolean | `false` | Debug output |
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
gemini-api run "A cat in space" --model gemini-3-pro-image-preview --output cat.png

# Text-to-speech
gemini-api run "Hello world" --model gemini-3.1-flash-tts-preview --voice Kore --output hello.wav

# With tools
gemini-api run "What is the weather?" --tool google_search --tool code_execution

# Structured output
gemini-api run "List 5 cities" --response-format '{"type":"array","items":{"type":"string"}}'

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
gemini-api agents init my-agent --base-agent gemini-3-flash-preview
gemini-api agents init my-agent --from-template https://github.com/google-gemini/Gemini-API-Agent-Templates/tree/main/customer-data-analysis-agent
```

| Flag | Type | Default | Description |
|---|---|---|---|
| `<name>` | positional | — | Agent directory name |
| `--base-agent` | string | `gemini-3-flash-preview` | Base model |
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

#### `gemini-api agents update <id>`

```bash
gemini-api agents update my-agent
gemini-api agents update my-agent --path ./my-agent
gemini-api agents update my-agent --dry-run
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
| `--stream` | boolean | `true` | Stream SSE |
| `--no-stream` | boolean | | Disable streaming |
| `--previous-interaction-id` | string | — | Multi-turn |
| `--environment` | string | — | Use existing environment |
| `--json` | boolean | `false` | JSON output |
| `--dry-run` | boolean | `false` | Print curl |

---

### `gemini-api files`

Manage environment files.

#### `gemini-api files list <env-id>`

```bash
gemini-api files list env_xyz789
gemini-api files list env_xyz789 --json
gemini-api files list env_xyz789 --dry-run
```

#### `gemini-api files download <env-id>`

```bash
gemini-api files download env_xyz789
gemini-api files download env_xyz789 --output ./results
gemini-api files download env_xyz789 --dry-run
```

| Flag | Type | Default | Description |
|---|---|---|---|
| `--output` | string | `./` | Output directory |
| `--file-id` | string | — | Download specific file |

---

## Agent Configuration

### Directory Structure

```
my-agent/
├── agent.yaml       # Configuration
├── AGENTS.md        # System instructions (uploaded to environment)
├── .env             # Credentials (inlined to /credentials/.env)
├── skills/          # Custom skills
└── workspace/       # Files seeded into remote environment
```

### `agent.yaml`

```yaml
# Required
id: my-agent
base_agent: gemini-3-flash-preview

# Optional
description: "A data analyst agent"
system_instruction: "You are a helpful assistant."

# Tools
tools:
  - type: code_execution
  - type: google_search

# Environment
environment:
  enabled: true

# OR derive from existing environment
# base_environment: env_abc123

# Subagents
subagents:
  - researcher

# Metadata
metadata:
  team: platform
```

### `.env`

Credentials file. If it contains non-empty values, it's inlined during `agents test` and `agents create` to `/credentials/.env` in the agent environment.

### `AGENTS.md`

Agent instructions in markdown. Uploaded to the remote environment and loaded before running. Use for long instructions — easier to read, diff, and version than `system_instruction`.

---

## Tools

### `--tool` Flag Syntax

```bash
# Simple tools (no config)
--tool code_execution
--tool google_search
--tool url_context
--tool computer_use
--tool file_search
--tool google_maps
--tool retrieval

# MCP server — mcp_server:<name>:<url>
--tool 'mcp_server:weather:https://example.com/mcp'

# Custom function — function:<name>:<json-schema>
--tool 'function:get_weather:{"type":"object","properties":{"location":{"type":"string"}}}'

# Multiple tools
--tool code_execution --tool google_search
```

### `agent.yaml` Tools

```yaml
tools:
  - type: code_execution
  - type: google_search
    search_types: [web_search, image_search]
  - type: url_context
  - type: computer_use
    environment: browser
  - type: mcp_server
    name: weather_service
    url: https://example.com/mcp
  - type: file_search
    file_search_store_names: ["fileSearchStores/abc123"]
    top_k: 10
  - type: google_maps
    latitude: 37.7749
    longitude: -122.4194
  - type: retrieval
    retrieval_types: [vertex_ai_search]
  - type: function
    name: get_weather
    description: "Get weather for a location"
    parameters:
      type: object
      properties:
        location: { type: string }
      required: [location]
```

---

## Output Modes

### Human (Default)

Streaming text with typed block labels:

```
[thought]  Analyzing...
[code]     print("Hello")
[result]   Hello
[text]     Based on my analysis...

✓ completed
  interaction_id: int_abc123
  tokens: in:1,234 out:567
  latency: 4.2s
```

### JSON (`--json`)

Raw SSE events as JSONL (one event per line):

```jsonl
{"event_type":"interaction.start","interaction":{...}}
{"event_type":"content.delta","index":0,"delta":{"type":"text","text":"Hello"}}
{"event_type":"interaction.complete","interaction":{...}}
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
| `gemini-3-pro-preview` | SOTA reasoning + multimodal |
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
| `deep-research-pro-preview-12-2025` | Deep Research |

---

## License

Apache-2.0
