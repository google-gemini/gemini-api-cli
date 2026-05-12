# End-to-End Test Plan — `gemini-api` CLI

> Pre-release validation suite. Every test has a `--dry-run` variant (fast, no API call) and a live variant (real API call).
>
> **Run all dry-run tests:** `bash E2E.sh --dry-run`
> **Run all live tests:** `bash E2E.sh`

---

## Convention

Each test case below uses this format:

```
CUJ-XX: <Title>
API:     <Endpoint or feature being tested>
dry-run: <command with --dry-run>
live:    <command that hits the real API>
assert:  <what to check in stdout/stderr>
```

The CLI entry point is `bun run src/cli.ts` during development or `gemini-api` when installed.
All commands below use the `CLI` placeholder — set it in your test script:

```bash
export GEMINI_API_KEY="your-api-key"

CLI="bun run src/cli.ts"
# or
CLI="gemini-api"
```

---

## 1. Basic Interactions (Model)

### CUJ-01: Simple text prompt (non-streaming)

The most basic interaction — send a text prompt, get a text response.

```bash
# dry-run
$CLI run "What is 2+2?" --dry-run

# live
$CLI run "What is 2+2?"
```

**Assert (dry-run):** Output contains `curl -X POST`, `/interactions`, `"input"`, `"model": "gemini-3-flash-preview"`.
**Assert (live):** Output contains `✓ completed`, `interaction_id:`.

---

### CUJ-02: Simple text prompt (streaming)

Default streaming mode — text arrives incrementally.

```bash
# dry-run
$CLI run "Count to 5" --dry-run

# live
$CLI run "Count to 5"
```

**Assert (dry-run):** Output contains `curl -X POST`, `"stream": true`.
**Assert (live):** Output contains `1`, `5`, `✓ completed`.

---

### CUJ-03: Specify a model explicitly

Override the default model.

```bash
# dry-run
$CLI run "Hello" --model gemini-3.1-pro-preview --dry-run

# live
$CLI run "Hello" --model gemini-3.1-pro-preview
```

**Assert (dry-run):** Output contains `"model": "gemini-3.1-pro-preview"`.
**Assert (live):** Output contains `✓ completed`.

---

### CUJ-04: JSON output mode

Raw SSE events as JSONL for machine consumption.

```bash
# dry-run
$CLI run "Hello" --json --dry-run

# live
$CLI run "Say hi" --json
```

**Assert (dry-run):** Output contains `curl`.
**Assert (live):** Each line is valid JSON. First event has `event_type`. At least one `content.delta` event exists.

---



### CUJ-06: System instruction

Provide a system prompt alongside the user prompt.

```bash
# dry-run
$CLI run "What are you?" --system-instruction "You are a pirate. Always respond in pirate speak." --dry-run

# live
$CLI run "What are you?" --system-instruction "You are a pirate. Always respond in pirate speak."
```

**Assert (dry-run):** Output contains `"system_instruction"`, `pirate`.
**Assert (live):** Output contains pirate-like language (e.g., `arr`, `matey`, `pirate`).

---

### CUJ-07: Service tier (flex)

Use the flex service tier for cost optimization.

```bash
# dry-run
$CLI run "Hello" --service-tier flex --dry-run

# live
$CLI run "Hello" --service-tier flex
```

**Assert (dry-run):** Output contains `"service_tier": "flex"`.
**Assert (live):** Output contains `✓ completed`.

---

## 2. Multi-Turn Conversations

### CUJ-08: Stateful multi-turn with previous-interaction-id

Continue a conversation across two turns using server-side state.

```bash
# dry-run
$CLI run "What was the word?" --previous-interaction-id fake_id_123 --dry-run

# live (two-step)
# Turn 1:
RESULT=$($CLI run "Remember the word: banana" --json 2>&1)
INT_ID=$(echo "$RESULT" | grep -o '"id":"[^"]*"' | head -1 | cut -d'"' -f4)

# Turn 2:
$CLI run "What word did I ask you to remember?" --previous-interaction-id "$INT_ID"
```

**Assert (dry-run):** Output contains `"previous_interaction_id": "fake_id_123"`.
**Assert (live):** Turn 2 output contains `banana`.

---

## 3. Multimodal Input

### CUJ-09: Image understanding

Send an image file alongside a text prompt.

```bash
# Create a test image (1x1 red PNG)
echo "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==" | base64 -d > /tmp/test.png

# dry-run
$CLI run "What color is this?" --input image:/tmp/test.png --dry-run

# live
$CLI run "What color is this?" --input image:/tmp/test.png
```

**Assert (dry-run):** Output contains `"type": "image"`, `"mime_type": "image/png"`, base64 data.
**Assert (live):** Output mentions a color (e.g., `red`, `salmon`).

---

### CUJ-10: Missing input file error

Graceful error when the input file doesn't exist.

```bash
# No API call needed — this is a client-side validation
$CLI run "Hello" --input image:nonexistent.png
```

**Assert:** Output contains `File not found`.

---

## 4. Multimodal Output (Generation)

### CUJ-11: Image generation

Generate an image and save to disk.

```bash
# dry-run
$CLI run "A blue square" --model gemini-3-pro-image-preview --output /tmp/test_gen.png --dry-run

# live
$CLI run "Generate a simple blue square" --model gemini-3-pro-image-preview --output /tmp/test_gen.png
```

**Assert (dry-run):** Output contains `"model": "gemini-3-pro-image-preview"`.
**Assert (live):** File `/tmp/test_gen.png` exists and is > 100 bytes.

---

### CUJ-12: Image generation with config (aspect ratio, size)

Use image_config to control output dimensions.

```bash
# dry-run
$CLI run "A sunset" --model gemini-3-pro-image-preview --aspect-ratio 16:9 --image-size 2k --dry-run

# live
$CLI run "A sunset over mountains" --model gemini-3-pro-image-preview --aspect-ratio 16:9 --image-size 2k --output /tmp/test_sunset.png
```

**Assert (dry-run):** Output contains `"image_config"`, `"aspect_ratio": "16:9"`, `"image_size": "2k"`.
**Assert (live):** File `/tmp/test_sunset.png` exists and is > 100 bytes.

---

### CUJ-13: Text-to-speech (TTS)

Generate speech audio from text.

```bash
# dry-run
$CLI run "Hello world" --model gemini-3.1-flash-tts-preview --voice Kore --language en-US --output /tmp/test_tts.wav --dry-run

# live
$CLI run "Hello world" --model gemini-3.1-flash-tts-preview --voice Kore --output /tmp/test_tts.wav
```

**Assert (dry-run):** Output contains `"speech_config"`, `"voice": "Kore"`.
**Assert (live):** File `/tmp/test_tts.wav` exists and is > 100 bytes.

---

### CUJ-14: Image editing (input image + output image)

Edit an existing image.

```bash
# Create test image
echo "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==" | base64 -d > /tmp/test_edit.png

# dry-run
$CLI run "Make it green" --input image:/tmp/test_edit.png --response-modality image --model gemini-3-pro-image-preview --output /tmp/test_edited.png --dry-run

# live
$CLI run "Make this image green" --input image:/tmp/test_edit.png --response-modality image --model gemini-3-pro-image-preview --output /tmp/test_edited.png
```

**Assert (dry-run):** Output contains `"response_modalities"`, `"image"`.
**Assert (live):** File `/tmp/test_edited.png` exists and is > 100 bytes.

---

### CUJ-15: Image editing with edit_strength and mask

Advanced image editing with strength control and mask.

```bash
# dry-run
echo "dummy" > /tmp/tmp_input.png
echo "dummy" > /tmp/tmp_mask.png
$CLI run "Edit this" --input image:/tmp/tmp_input.png --response-modality image --edit-strength 0.5 --mask /tmp/tmp_mask.png --dry-run
```

**Assert (dry-run):** Output contains `"edit_strength": 0.5`, `"mask":`.

---

## 5. Tools

### CUJ-16: Code execution tool

Use code_execution to calculate something.

```bash
# dry-run
$CLI run "Calculate 2+2" --tool code_execution --dry-run

# live
$CLI run "Use code execution to calculate 2+2 and return only the number" --tool code_execution
```

**Assert (dry-run):** Output contains `"type": "code_execution"`.
**Assert (live):** Output contains `4`, no `API error`.

---

### CUJ-17: Google Search tool

Use Google Search for grounding.

```bash
# dry-run
$CLI run "What happened today?" --tool google_search --dry-run

# live
$CLI run "What is the current population of Tokyo? Use search." --tool google_search
```

**Assert (dry-run):** Output contains `"type": "google_search"`.
**Assert (live):** Output contains `✓ completed`, no `API error`.

---

### CUJ-18: URL context tool

Fetch and summarize a URL.

```bash
# dry-run
$CLI run "Summarize https://www.wikipedia.org/" --tool url_context --dry-run

# live
$CLI run "Summarize the content of https://www.wikipedia.org/" --tool url_context
```

**Assert (dry-run):** Output contains `"type": "url_context"`.
**Assert (live):** Output contains `✓ completed`, mentions Wikipedia.

---

### CUJ-19: Multiple tools together

Combine Google Search and code execution.

```bash
# dry-run
$CLI run "Search and calculate" --tool google_search --tool code_execution --dry-run

# live
$CLI run "Search for the GDP of France then calculate GDP per capita" --tool google_search --tool code_execution
```

**Assert (dry-run):** Output contains both `google_search` and `code_execution`.
**Assert (live):** Output contains `✓ completed`.

---

### CUJ-20: Invalid tool error

Graceful error for unknown tool names.

```bash
$CLI run "Hello" --tool invalid_tool
```

**Assert:** Output contains `Unknown tool`, lists available tools (e.g., `code_execution`).

---

## 6. Agents Lifecycle

### CUJ-21: Agent init (scaffold)

Create a new agent project directory.

```bash
AGENT_NAME="e2e-test-agent-$(date +%s)"

$CLI agents init "$AGENT_NAME"
```

**Assert:** Directory `$AGENT_NAME/` exists with `agent.yaml`, `AGENTS.md`, `skills/`, `.env`.

**Cleanup:** `rm -rf "$AGENT_NAME"`

---

### CUJ-22: Agent init is idempotent

Running init twice on the same name reports "already exists".

```bash
$CLI agents init "$AGENT_NAME"
$CLI agents init "$AGENT_NAME"
```

**Assert:** Second run output contains `already exists`.

---

### CUJ-23: Agent create (deploy) — dry-run

Deploy an agent from a directory.

```bash
$CLI agents init "$AGENT_NAME"
$CLI agents create --path "./$AGENT_NAME" --dry-run
```

**Assert:** Output contains `curl -X POST`, `/agents`, `"name":`, `"base_agent": "antigravity-preview-05-2026"`.

---

### CUJ-24: Agent create (deploy) — live

```bash
$CLI agents init "$AGENT_NAME"
$CLI agents create --path "./$AGENT_NAME"
```

**Assert:** Output contains `✓ Created agent`.

---

### CUJ-25: Agent list

List all deployed agents.

```bash
# dry-run
$CLI agents list --dry-run

# live
$CLI agents list
```

**Assert (dry-run):** Output contains `curl`, `/agents`.
**Assert (live):** Output is a list (JSON or human-readable) containing agent names.

---

### CUJ-26: Agent list (JSON mode)

```bash
# dry-run
$CLI agents list --json --dry-run

# live
$CLI agents list --json
```

**Assert (live):** Output is valid JSON.

---

### CUJ-27: Agent get

Get details of a specific agent.

```bash
# dry-run
$CLI agents get my-agent --dry-run

# live
$CLI agents get "$AGENT_NAME"
```

**Assert (dry-run):** Output contains `curl`, `/agents/my-agent`.
**Assert (live):** Output contains agent name/id and `base_agent`.

---

### CUJ-28: Agent delete

Delete a deployed agent.

```bash
# dry-run
$CLI agents delete my-agent --force --dry-run

# live
$CLI agents delete "$AGENT_NAME" --force
```

**Assert (dry-run):** Output contains `curl -X DELETE`, `/agents/my-agent`.
**Assert (live):** Output contains `Deleted agent`.

---

### CUJ-29: Agent full lifecycle (create → list → get → delete)

End-to-end lifecycle test.

```bash
AGENT_NAME="e2e-lifecycle-$(date +%s)"

# 1. Init
$CLI agents init "$AGENT_NAME"

# 2. Create
$CLI agents create --path "./$AGENT_NAME"

# 3. List — should include the agent
$CLI agents list

# 4. Get — should return details
$CLI agents get "$AGENT_NAME"

# 5. Delete
$CLI agents delete "$AGENT_NAME" --force

# 6. Cleanup
rm -rf "$AGENT_NAME"
```

**Assert:** Each step succeeds. Agent appears in list after create, disappears after delete.

---

### CUJ-30: Agent test (interaction via local config)

Run an interaction using local agent.yaml.

```bash
AGENT_NAME="e2e-test-$(date +%s)"
$CLI agents init "$AGENT_NAME"

# dry-run
$CLI agents test --prompt "Hello" --path "./$AGENT_NAME" --dry-run

# live
$CLI agents test --prompt "Hello" --path "./$AGENT_NAME"

rm -rf "$AGENT_NAME"
```

**Assert (dry-run):** Output contains `curl`, `/interactions`, `"agent": "antigravity-preview-05-2026"`.
**Assert (live):** Output contains `✓ completed`.

---

## 7. Agent Interactions

### CUJ-31: Run with deployed agent (antigravity-preview-05-2026)

Interact with the base antigravity-preview-05-2026 agent.

```bash
# dry-run
$CLI run "What is 2+2?" --agent antigravity-preview-05-2026 --dry-run

# live
$CLI run "What is 2+2?" --agent antigravity-preview-05-2026
```

**Assert (dry-run):** Output contains `"agent": "antigravity-preview-05-2026"`, `"environment": {"enabled": true}`.
**Assert (live):** Output contains `✓ completed`, environment_id.

---

### CUJ-32: Run with custom agent

Interact with a user-created agent.

```bash
# dry-run
$CLI run "Hello" --agent my-custom-agent --dry-run

# live (requires agent to exist)
$CLI run "Hello" --agent "$AGENT_NAME"
```

**Assert (dry-run):** Output contains `"agent": "my-custom-agent"`.
**Assert (live):** Output contains `✓ completed`.

---

### CUJ-33: Agent with environment persistence (multi-turn)

Multi-turn agent interaction that reuses an environment.

```bash
# dry-run
$CLI run "Continue" --agent antigravity-preview-05-2026 --previous-interaction-id fake_int --dry-run

# live (two-step)
RESULT=$($CLI run "Write 'hello' to /tmp/test.txt" --agent antigravity-preview-05-2026 --json 2>&1)
INT_ID=$(echo "$RESULT" | grep -o '"id":"[^"]*"' | head -1 | cut -d'"' -f4)

$CLI run "Read /tmp/test.txt and tell me what it says" --agent antigravity-preview-05-2026 --previous-interaction-id "$INT_ID"
```

**Assert (dry-run):** Output contains `"previous_interaction_id"`.
**Assert (live):** Second turn output mentions `hello`.

---

## 8. Deep Research Agent

### CUJ-34: Deep Research (background mode)

Start a Deep Research agent task.

```bash
# dry-run
$CLI run "Research the history of TPUs" --agent deep-research-preview-04-2026 --dry-run

# live (will take minutes)
$CLI run "Research the history of Google TPUs in 2 paragraphs" --agent deep-research-preview-04-2026
```

**Assert (dry-run):** Output contains `"agent": "deep-research-preview-04-2026"`, `"background": true`, `"agent_config"`.
**Assert (live):** Output contains final research text, `✓ completed`.

---

## 9. Files (Environment)

### CUJ-36: Download environment files (snapshot)

```bash
# dry-run
$CLI files download env_fake123 --dry-run

# live (requires a valid env_id)
# $CLI files download "$ENV_ID" --output ./tmp
```

**Assert (dry-run):** Output contains `curl`, `/files/environment-env_fake123:download?alt=media`.
**Assert (live):** Directory `./tmp/snapshot_$ENV_ID` exists and contains files.

---

## 10. Error Handling

### CUJ-37: Missing prompt

```bash
$CLI run
```

**Assert:** Output contains `Missing prompt`, exit code ≠ 0.

---

### CUJ-38: Missing API key

```bash
GEMINI_API_KEY="" GEMINI_AUTOPUSH_API_KEY="" $CLI run "Hello"
```

**Assert:** Output contains `No API key found`, exit code ≠ 0.

---

### CUJ-39: Invalid model (400 error)

```bash
$CLI run "Hello" --model nonexistent-model
```

**Assert:** Output contains `API error` or `400`.

---

### CUJ-40: Missing agent.yaml for agents create

```bash
$CLI agents create --path /tmp/empty-dir-that-does-not-exist
```

**Assert:** Output contains error about missing `agent.yaml`.

---

### CUJ-44: Invalid agent error

Graceful error for unknown agent names.

```bash
$CLI run "Hello" --agent invalid_agent
```

**Assert:** Output contains `Unknown agent`, lists available agent types (e.g., `antigravity-preview-05-2026`).

---

## 11. Output & UX

### CUJ-41: Dry-run on all commands

Every command that hits the API must support `--dry-run`.

```bash
$CLI run "Hello" --dry-run
$CLI agents create --dry-run
$CLI agents list --dry-run
$CLI agents get my-agent --dry-run
$CLI agents delete my-agent --force --dry-run
$CLI files list env_123 --dry-run
$CLI files download env_123 --dry-run
```

**Assert:** Each outputs a `curl` command and exits with code 0.

---

### CUJ-42: Completion summary metadata

The human-mode completion summary includes machine-useful metadata.

```bash
$CLI run "Say hello"
```

**Assert:** Output contains `✓ completed`, `interaction_id:`, `latency:`.

---

### CUJ-43: Interaction logging

Every interaction creates a JSONL log file.

```bash
rm -rf .gemini/logs
$CLI run "Hello"
ls .gemini/logs/
```

**Assert:** A `.jsonl` file exists in `.gemini/logs/`. File has 2 lines (request + response).

---

## Summary Table

| # | CUJ | Category | 
|---|-----|----------|
| [01](file:///usr/local/google/home/philschmid/gemini-api-cli/tests/e2e/cuj_01.sh) | Simple text (non-streaming) | Interactions |
| [02](file:///usr/local/google/home/philschmid/gemini-api-cli/tests/e2e/cuj_02.sh) | Simple text (streaming) | Interactions |
| [03](file:///usr/local/google/home/philschmid/gemini-api-cli/tests/e2e/cuj_03.sh) | Specify model | Interactions |
| [04](file:///usr/local/google/home/philschmid/gemini-api-cli/tests/e2e/cuj_04.sh) | JSON output | Interactions |
| [06](file:///usr/local/google/home/philschmid/gemini-api-cli/tests/e2e/cuj_06.sh) | System instruction | Interactions |
| [07](file:///usr/local/google/home/philschmid/gemini-api-cli/tests/e2e/cuj_07.sh) | Service tier | Interactions |
| [08](file:///usr/local/google/home/philschmid/gemini-api-cli/tests/e2e/cuj_08.sh) | Multi-turn | Conversations |
| [09](file:///usr/local/google/home/philschmid/gemini-api-cli/tests/e2e/cuj_09.sh) | Image understanding | Multimodal |
| [10](file:///usr/local/google/home/philschmid/gemini-api-cli/tests/e2e/cuj_10.sh) | Missing file error | Error |
| [11](file:///usr/local/google/home/philschmid/gemini-api-cli/tests/e2e/cuj_11.sh) | Image generation | Generation |
| [12](file:///usr/local/google/home/philschmid/gemini-api-cli/tests/e2e/cuj_12.sh) | Image config | Generation |
| [13](file:///usr/local/google/home/philschmid/gemini-api-cli/tests/e2e/cuj_13.sh) | TTS | Generation |
| [14](file:///usr/local/google/home/philschmid/gemini-api-cli/tests/e2e/cuj_14.sh) | Image editing | Generation |
| [15](file:///usr/local/google/home/philschmid/gemini-api-cli/tests/e2e/cuj_15.sh) | Edit strength + mask | Generation |
| [16](file:///usr/local/google/home/philschmid/gemini-api-cli/tests/e2e/cuj_16.sh) | Code execution | Tools |
| [17](file:///usr/local/google/home/philschmid/gemini-api-cli/tests/e2e/cuj_17.sh) | Google Search | Tools |
| [18](file:///usr/local/google/home/philschmid/gemini-api-cli/tests/e2e/cuj_18.sh) | URL context | Tools |
| [19](file:///usr/local/google/home/philschmid/gemini-api-cli/tests/e2e/cuj_19.sh) | Multiple tools | Tools |
| [20](file:///usr/local/google/home/philschmid/gemini-api-cli/tests/e2e/cuj_20.sh) | Invalid tool | Error |
| [21](file:///usr/local/google/home/philschmid/gemini-api-cli/tests/e2e/cuj_21.sh) | Agent init | Agents |
| [22](file:///usr/local/google/home/philschmid/gemini-api-cli/tests/e2e/cuj_22.sh) | Agent init idempotent | Agents |
| [23](file:///usr/local/google/home/philschmid/gemini-api-cli/tests/e2e/cuj_23.sh) | Agent create (dry) | Agents |
| [24](file:///usr/local/google/home/philschmid/gemini-api-cli/tests/e2e/cuj_24.sh) | Agent create (live) | Agents |
| [25](file:///usr/local/google/home/philschmid/gemini-api-cli/tests/e2e/cuj_25.sh) | Agent list | Agents |
| [26](file:///usr/local/google/home/philschmid/gemini-api-cli/tests/e2e/cuj_26.sh) | Agent list JSON | Agents |
| [27](file:///usr/local/google/home/philschmid/gemini-api-cli/tests/e2e/cuj_27.sh) | Agent get | Agents |
| [28](file:///usr/local/google/home/philschmid/gemini-api-cli/tests/e2e/cuj_28.sh) | Agent delete | Agents |
| [29](file:///usr/local/google/home/philschmid/gemini-api-cli/tests/e2e/cuj_29.sh) | Agent full lifecycle | Agents |
| [30](file:///usr/local/google/home/philschmid/gemini-api-cli/tests/e2e/cuj_30.sh) | Agent test | Agents |
| [31](file:///usr/local/google/home/philschmid/gemini-api-cli/tests/e2e/cuj_31.sh) | antigravity-preview-05-2026 agent | Agent Run |
| [32](file:///usr/local/google/home/philschmid/gemini-api-cli/tests/e2e/cuj_32.sh) | Custom agent | Agent Run |
| [33](file:///usr/local/google/home/philschmid/gemini-api-cli/tests/e2e/cuj_33.sh) | Agent env persistence | Agent Run |
| [34](file:///usr/local/google/home/philschmid/gemini-api-cli/tests/e2e/cuj_34.sh) | Deep Research | Agent Run |
| [35](file:///usr/local/google/home/philschmid/gemini-api-cli/tests/e2e/cuj_35.sh) | Files list | Files |
| [36](file:///usr/local/google/home/philschmid/gemini-api-cli/tests/e2e/cuj_36.sh) | Files download | Files |
| [37](file:///usr/local/google/home/philschmid/gemini-api-cli/tests/e2e/cuj_37.sh) | Missing prompt | Error |
| [38](file:///usr/local/google/home/philschmid/gemini-api-cli/tests/e2e/cuj_38.sh) | Missing API key | Error |
| [39](file:///usr/local/google/home/philschmid/gemini-api-cli/tests/e2e/cuj_39.sh) | Invalid model | Error |
| [40](file:///usr/local/google/home/philschmid/gemini-api-cli/tests/e2e/cuj_40.sh) | Missing agent.yaml | Error |
| [41](file:///usr/local/google/home/philschmid/gemini-api-cli/tests/e2e/cuj_41.sh) | Dry-run on all commands | UX |
| [42](file:///usr/local/google/home/philschmid/gemini-api-cli/tests/e2e/cuj_42.sh) | Completion summary | UX |
| [43](file:///usr/local/google/home/philschmid/gemini-api-cli/tests/e2e/cuj_43.sh) | Interaction logging | UX |
| [44](file:///usr/local/google/home/philschmid/gemini-api-cli/tests/e2e/cuj_44.sh) | Invalid agent | Error |
