# Daytona

Run the Gemini API self-hosted environment runtime inside a
[Daytona](https://www.daytona.io/) workspace using the `daytona` CLI.

## Prerequisites

Install the [`gemini-api` CLI](https://github.com/google-gemini/gemini-api-cli)
and [Gemini API key](https://ai.google.dev/gemini-api/docs/api-key) on your
local machine, then install the `daytona` CLI and log in:

```bash
daytona login
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
# 1. Create a Daytona workspace with your environment key and export its ID
daytona create -e GEMINI_ENVIRONMENT_KEY="$GEMINI_ENVIRONMENT_KEY"
export SANDBOX_ID="..."

# 2. Install the gemini-api CLI inside the workspace
daytona exec "$SANDBOX_ID" -- \
  'curl -fsSL https://raw.githubusercontent.com/google-gemini/gemini-api-cli/main/scripts/install.sh \
  | bash'

# 3. Connect the workspace to your Gemini API environment
daytona exec "$SANDBOX_ID" -- \
  'PATH="$HOME/.local/bin:$PATH" gemini-api environments connect'
```

## Run an Interaction

From a second terminal, send an agent interaction targeting your connected
Daytona workspace:

```bash
gemini-api agent run \
  "Create hello.txt and run uname -a in bash." \
  --agent antigravity-preview-05-2026 \
  --body "{\"environment\": \"$GEMINI_ENVIRONMENT_ID\"}"
```

## Cleanup

```bash
daytona delete "$SANDBOX_ID" -y
```
