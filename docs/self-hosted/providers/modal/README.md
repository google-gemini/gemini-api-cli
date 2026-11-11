# Modal

Run the Gemini API self-hosted environment runtime inside a
[Modal](https://modal.com/) container using the `modal` CLI.

## Prerequisites

Install the [`gemini-api` CLI](https://github.com/google-gemini/gemini-api-cli)
and [Gemini API key](https://ai.google.dev/gemini-api/docs/api-key) on your
local machine, plus the `modal` CLI:

```bash
pip install modal
modal setup
```

Create a `self_hosted` environment and export both `GEMINI_ENVIRONMENT_ID` and
`GEMINI_ENVIRONMENT_KEY` on your local machine (`environment_key` is returned
**only once at creation time**):

```bash
export GEMINI_API_KEY="your-api-key"

curl -fsSL -X POST \
  "https://generativelanguage.googleapis.com/v1beta/environments" \
  -H "x-goog-api-key: $GEMINI_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"mode": "self_hosted"}'

export GEMINI_ENVIRONMENT_ID="..."   # "id" from response (no "environments/" prefix)
export GEMINI_ENVIRONMENT_KEY="..."  # "environment_key" from response
```

## Quick Start

Run all steps from your host terminal in a single non-interactive command:

```bash
modal shell --no-pty -c \
  "apt-get update && apt-get install -y curl ca-certificates && curl -fsSL https://raw.githubusercontent.com/google-gemini/gemini-api-cli/main/scripts/install.sh \
  | bash && PATH=\"\$HOME/.local/bin:\$PATH\" gemini-api environments connect --environment-key=$GEMINI_ENVIRONMENT_KEY"
```

Or open an interactive Modal shell and export `GEMINI_ENVIRONMENT_KEY` inside
the remote container before connecting:

```bash
# 1. Open a shell inside an isolated Modal container
modal shell

# 2. Inside the Modal shell, install curl and gemini-api, then connect:
export GEMINI_ENVIRONMENT_KEY="..."
apt-get update && apt-get install -y curl ca-certificates
curl -fsSL https://raw.githubusercontent.com/google-gemini/gemini-api-cli/main/scripts/install.sh \
  | bash
PATH="$HOME/.local/bin:$PATH" gemini-api environments connect \
  --environment-key="$GEMINI_ENVIRONMENT_KEY"
```

## Run an Interaction

From a second local terminal, send an agent interaction to your connected Modal
worker:

```bash
gemini-api agent run \
  "Create hello.txt and run uname -a in bash." \
  --agent antigravity-preview-05-2026 \
  --body "{\"environment\": \"$GEMINI_ENVIRONMENT_ID\"}"
```

## Cleanup

Press `Ctrl+C` (and `exit` if inside an interactive `modal shell`)—Modal
automatically terminates the ephemeral shell container when the session exits.
