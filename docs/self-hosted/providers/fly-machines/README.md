# Fly.io (Sprites & Fly Machines)

Run the Gemini API self-hosted environment runtime inside a
[Fly.io Sprite](https://sprites.dev/) stateful Firecracker microVM using the
`sprite` CLI (or a raw [Fly Machine](https://fly.io/docs/machines/) using the
`fly` CLI).

## Prerequisites

Install the [`gemini-api` CLI](https://github.com/google-gemini/gemini-api-cli)
and [Gemini API key](https://ai.google.dev/gemini-api/docs/api-key) on your
local machine, then install the `sprite` CLI and authenticate with your Fly.io
account:

```bash
curl -fsSL https://sprites.dev/install.sh \
  | sh
sprite login
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

## Quick Start (Fly.io Sprites — `sprite` CLI)

```bash
# 1. Create an isolated Sprite microVM
sprite create --skip-console gemini-self-hosted

# 2. Install the gemini-api CLI inside the Sprite
sprite exec -s gemini-self-hosted --no-stdin -- bash -c \
  "curl -fsSL https://raw.githubusercontent.com/google-gemini/gemini-api-cli/main/scripts/install.sh \
  | bash"

# 3. Connect the Sprite to your Gemini API environment
sprite exec -s gemini-self-hosted --no-stdin \
  --env "GEMINI_ENVIRONMENT_KEY=$GEMINI_ENVIRONMENT_KEY" -- bash -c \
  'PATH="$HOME/.local/bin:$PATH" gemini-api environments connect'
```

## Alternative: Raw Fly Machines (`fly` CLI)

If using `fly` (`flyctl`) directly, pass `-a "$FLY_APP"` for an existing Fly app
(or run inside a directory containing `fly.toml`):

```bash
export FLY_APP="your-fly-app"
fly apps create "$FLY_APP" || true

fly machine run ubuntu:24.04 \
  -a "$FLY_APP" \
  -e GEMINI_ENVIRONMENT_KEY="$GEMINI_ENVIRONMENT_KEY" \
  -- bash -c "apt-get update && apt-get install -y curl ca-certificates && \
    curl -fsSL https://raw.githubusercontent.com/google-gemini/gemini-api-cli/main/scripts/install.sh \
    | bash && PATH=\"\$HOME/.local/bin:\$PATH\" gemini-api environments connect"
```

## Run an Interaction

From a second terminal, send an agent interaction to your connected Sprite or
Fly Machine:

```bash
gemini-api agent run \
  "Create hello.txt and run uname -a in bash." \
  --agent antigravity-preview-05-2026 \
  --body "{\"environment\": \"$GEMINI_ENVIRONMENT_ID\"}"
```

## Cleanup

```bash
# Delete the Sprite microVM:
sprite destroy -s gemini-self-hosted --force

# Or delete the Fly app if you created one for raw Fly Machines:
# fly apps destroy "$FLY_APP" --yes
```
