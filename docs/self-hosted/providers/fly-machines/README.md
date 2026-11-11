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

export GEMINI_ENVIRONMENT_ID="..."   # "id" from response (no "environments/" prefix)
export GEMINI_ENVIRONMENT_KEY="..."  # "environment_key" from response
```

## Quick Start (Fly.io Sprites — `sprite` CLI)

A Sprite pauses when idle, and outbound polling does not count as activity, so
running `gemini-api environments connect` as a Sprite service with a task
heartbeat keeps the worker active and restarts it automatically if it exits:

```bash
# 1. Create an isolated Sprite microVM
sprite create --skip-console gemini-self-hosted

# 2. Install the gemini-api CLI inside the Sprite
sprite exec -s gemini-self-hosted --no-stdin -- bash -c \
  "curl -fsSL https://raw.githubusercontent.com/google-gemini/gemini-api-cli/main/scripts/install.sh \
  | bash"

# 3. Store the environment key in a user-only file inside the Sprite
sprite exec -s gemini-self-hosted --no-stdin \
  --env "GEMINI_ENVIRONMENT_KEY=$GEMINI_ENVIRONMENT_KEY" -- bash -c \
  'umask 077; printf "GEMINI_ENVIRONMENT_KEY=%s\n" "$GEMINI_ENVIRONMENT_KEY" > ~/.gemini-env'

# 4. Write the worker script (holds a 5-minute task to keep the Sprite awake) and register it as a service
sprite exec -s gemini-self-hosted --no-stdin -- bash -c 'cat > ~/gemini-connect.sh <<"EOF"
#!/usr/bin/env bash
set -a; . "$HOME/.gemini-env"; set +a
export PATH="$HOME/.local/bin:$PATH"
mkdir -p "$HOME/workspace"

( while true; do
    sprite-env curl -X PUT /v1/tasks/gemini-connect -d "{\"expire\": \"5m\"}" >/dev/null 2>&1
    sleep 60
  done ) &
heartbeat=$!
trap "kill $heartbeat 2>/dev/null; sprite-env curl -X DELETE /v1/tasks/gemini-connect >/dev/null 2>&1" EXIT

gemini-api environments connect --root-dir "$HOME/workspace" --workdir "$HOME/workspace"
EOF
chmod +x ~/gemini-connect.sh'

sprite exec -s gemini-self-hosted --no-stdin -- \
  sprite-env services create gemini-connect --cmd /home/sprite/gemini-connect.sh
```

> [!NOTE]
>
> An active Sprite incurs compute billing while the worker is running. To pause
> billing between uses, stop the service with
> `sprite-env services stop gemini-connect` (via `sprite exec -s
> gemini-self-hosted -- ...`) so the Sprite can pause, and start it again later
> with `sprite-env services start gemini-connect`. Service logs are written to
> `/.sprite/logs/services/gemini-connect.log`.

## Alternative: Raw Fly Machines (`fly` CLI)

If using `fly` (`flyctl`) directly, store `GEMINI_ENVIRONMENT_KEY` as an app
secret with `fly secrets set` (instead of `-e`, so it is not stored in the
Machine config) and pass `-a "$FLY_APP"` for an existing Fly app (or run inside
a directory containing `fly.toml`):

```bash
export FLY_APP="your-fly-app"
fly apps create "$FLY_APP" || true
fly secrets set -a "$FLY_APP" --stage GEMINI_ENVIRONMENT_KEY="$GEMINI_ENVIRONMENT_KEY"

fly machine run ubuntu:24.04 \
  -a "$FLY_APP" \
  -- bash -c "apt-get update && apt-get install -y curl ca-certificates && \
    curl -fsSL https://raw.githubusercontent.com/google-gemini/gemini-api-cli/main/scripts/install.sh \
    | bash && mkdir -p /workspace && \
    PATH=\"\$HOME/.local/bin:\$PATH\" gemini-api environments connect --root-dir /workspace --workdir /workspace"
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
