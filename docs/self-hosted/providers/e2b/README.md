# E2B

Run the Gemini API self-hosted environment runtime inside an
[E2B](https://e2b.dev/) Firecracker microVM using the `e2b` CLI.

## Prerequisites

Install the [`gemini-api` CLI](https://github.com/google-gemini/gemini-api-cli)
and [Gemini API key](https://ai.google.dev/gemini-api/docs/api-key) on your
local machine, plus the `e2b` CLI:

```bash
npm install -g @e2b/cli
e2b auth login
```

Create a `self_hosted` environment and export both `GEMINI_ENVIRONMENT_ID` and
`GEMINI_ENVIRONMENT_KEY` (`environment_key` is returned **only once at creation
time**):

```bash
export GEMINI_API_KEY="your-api-key"

curl -fsSL -X POST \
  "https://generativelanguage.googleapis.com/v1beta/environments" \
  -H "x-goog-api-key: $GEMINI_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"mode": "self_hosted"}'

export GEMINI_ENVIRONMENT_ID="environments/..."
export GEMINI_ENVIRONMENT_KEY="ek_..."
```

## Quick Start

```bash
# 1. Create a detached E2B microVM
SANDBOX_ID=$(e2b sandbox create --detach base)

# 2. Install the gemini-api CLI inside the microVM
e2b sandbox exec "$SANDBOX_ID" bash -c \
  "curl -fsSL https://raw.githubusercontent.com/google-gemini/gemini-api-cli/main/scripts/install.sh \
  | bash"

# 3. Connect the microVM to your Gemini API environment
e2b sandbox exec -b -e GEMINI_ENVIRONMENT_KEY="$GEMINI_ENVIRONMENT_KEY" \
  "$SANDBOX_ID" bash -c \
  'PATH="$HOME/.local/bin:$PATH" gemini-api environments connect'
```

Or run interactively inside the microVM terminal (exporting
`GEMINI_ENVIRONMENT_KEY` inside the microVM shell):

```bash
e2b sandbox create base

# Inside the microVM shell:
export GEMINI_ENVIRONMENT_KEY="ek_..."
curl -fsSL https://raw.githubusercontent.com/google-gemini/gemini-api-cli/main/scripts/install.sh \
  | bash
PATH="$HOME/.local/bin:$PATH" gemini-api environments connect \
  --environment-key="$GEMINI_ENVIRONMENT_KEY"
```

## Run an Interaction

```bash
gemini-api agent run \
  "Create hello.txt and run uname -a in bash." \
  --agent antigravity-preview-05-2026 \
  --body "{\"environment\": \"$GEMINI_ENVIRONMENT_ID\"}"
```

## Cleanup

```bash
e2b sandbox kill "$SANDBOX_ID"
```
