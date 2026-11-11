# Vercel Sandbox

Run the Gemini API self-hosted environment runtime inside a
[Vercel Sandbox](https://vercel.com/docs/vercel-sandbox) microVM using the
`sandbox` CLI.

## Prerequisites

Install the [`gemini-api` CLI](https://github.com/google-gemini/gemini-api-cli)
and [Gemini API key](https://ai.google.dev/gemini-api/docs/api-key) on your
local machine, plus the Vercel `sandbox` CLI:

```bash
npm install -g sandbox
sandbox login
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

export GEMINI_ENVIRONMENT_ID="..."   # "id" from response (no "environments/" prefix)
export GEMINI_ENVIRONMENT_KEY="..."  # "environment_key" from response
```

## Quick Start

```bash
# 1. Create a persistent named microVM (use --timeout 45m on Hobby)
sandbox create --name gemini-self-hosted --image vercel/sandbox/universal:latest --timeout 24h

# 2. Install the gemini-api CLI inside the microVM
sandbox exec gemini-self-hosted -- bash -lc \
  'curl -fsSL https://raw.githubusercontent.com/google-gemini/gemini-api-cli/main/scripts/install.sh \
  | bash'

# 3. Connect the microVM to your Gemini API environment
sandbox exec --env GEMINI_ENVIRONMENT_KEY="$GEMINI_ENVIRONMENT_KEY" gemini-self-hosted -- \
  bash -lc 'PATH="$HOME/.local/bin:$PATH" gemini-api environments connect --environment-key="$GEMINI_ENVIRONMENT_KEY"'
```

> [!NOTE]
>
> Session timeouts default to **5 minutes** when `--timeout` is omitted.
> **Hobby** supports up to **45 minutes** per session (`--timeout 45m`), while
> **Pro** and **Enterprise** support up to **24 hours** (`--timeout 24h`). A
> session is one unit of work—you can run unlimited sessions over time on the
> same persistent microVM.

## Run an Interaction

From a second terminal, run an agent interaction targeting your connected Vercel
microVM:

```bash
gemini-api agent run \
  "Create hello.txt and run uname -a in bash." \
  --agent antigravity-preview-05-2026 \
  --body "{\"environment\": \"$GEMINI_ENVIRONMENT_ID\"}"
```

## Reusing a Persistent Sandbox

Named sandboxes are persistent by default: when a session stops, Vercel
snapshots the filesystem (preserving the installed `gemini-api` CLI and your
agent workspace) and restores it on the next session. After completing the
initial setup once, reconnect `gemini-self-hosted` in later sessions by name:

```bash
sandbox exec --env GEMINI_ENVIRONMENT_KEY="$GEMINI_ENVIRONMENT_KEY" gemini-self-hosted -- \
  bash -lc 'PATH="$HOME/.local/bin:$PATH" gemini-api environments connect --environment-key="$GEMINI_ENVIRONMENT_KEY"'
```

## Cleanup

```bash
# Stop the current session (snapshots and preserves gemini-self-hosted for later reuse):
sandbox stop gemini-self-hosted

# Or permanently delete the named microVM and its saved snapshots
# (without --delete-orphan-snapshots, snapshots are kept for 30 days by default):
# sandbox remove gemini-self-hosted --delete-orphan-snapshots
```
