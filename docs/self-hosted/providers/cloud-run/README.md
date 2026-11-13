# Google Cloud Run

Run the Gemini API self-hosted environment runtime on a
[Google Cloud Run Worker Pool](https://cloud.google.com/run/docs/deploy-worker-pools)
with
[Cloud Run Sandboxes](https://cloud.google.com/run/docs/configuring/workerpools/sandboxes)
(`--sandbox-launcher`) using the `gcloud` CLI. You don't need a custom
`Dockerfile` or an inbound HTTP endpoint.

The outer worker pool container runs `gemini-api environments poll` and
dispatches each leased interaction into an isolated nested gVisor sandbox
(`/usr/local/gcp/bin/sandbox do`) running `gemini-api environments serve`. Each
sandbox keeps `GEMINI_ENVIRONMENT_KEY` out of its environment, blocks
`169.254.169.254` (Metadata Server) via kernel `nftables`, and isolates
`/workspace` per environment.

## Prerequisites

Install the [gemini-api CLI](https://github.com/google-gemini/gemini-api-cli)
and [Gemini API key](https://ai.google.dev/gemini-api/docs/api-key) on your
local machine, plus the `gcloud` CLI (`gcloud components install beta`):

```bash
export PROJECT_ID="YOUR_PROJECT_ID"
export REGION="us-central1"

gcloud auth login
gcloud config set project "$PROJECT_ID"
gcloud services enable run.googleapis.com iam.googleapis.com
gcloud config set run/region "$REGION"
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
# 1. Create a dedicated zero-permission service account (one-time per project)
gcloud iam service-accounts create gemini-self-hosted

# 2. Deploy a 1-instance Worker Pool with Cloud Run Sandboxes (--sandbox-launcher)
STARTUP_SCRIPT="$(cat <<'OUTER'
set -eu
apt-get update && apt-get install -y --no-install-recommends curl ca-certificates git jq python3 ripgrep util-linux
curl -fsSL https://raw.githubusercontent.com/google-gemini/gemini-api-cli/main/scripts/install.sh \
  | GEMINI_INSTALL_DIR=/usr/local/bin bash
mkdir -p /workspace /tmp/workspaces
cat <<'EOF' > /usr/local/bin/provision-sandbox.sh
#!/bin/sh
set -eu
eval "$(jq -re '"id=\(.interaction_id | @sh) tok=\(.lease_token | @sh) env_id=\((.environment_id // "default") | @sh) srv=\((.server_url // "") | @sh)"')"
ws="/tmp/workspaces/$(printf '%s' "$env_id" | sha256sum | cut -c1-16)"
mkdir -p "$ws"
setsid /usr/local/gcp/bin/sandbox do \
  --stdin=false --allow-egress --write --workdir=/workspace \
  --mount="type=bind,src=${ws},dst=/workspace" \
  -e PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \
  -e HOME=/root -e USER=root -e LANG=C.UTF-8 \
  -- /usr/local/bin/gemini-api environments serve \
    --interaction-id="$id" --lease-token="$tok" \
    ${srv:+--server-url="$srv"} \
    --root-dir=/workspace --workdir=/workspace </dev/null >/dev/null 2>&1 &
EOF
chmod +x /usr/local/bin/provision-sandbox.sh
exec gemini-api environments poll --provision-cmd /usr/local/bin/provision-sandbox.sh
OUTER
)"

gcloud beta run worker-pools deploy gemini-self-hosted \
  --image debian:bookworm-slim \
  --instances 1 \
  --service-account "gemini-self-hosted@${PROJECT_ID}.iam.gserviceaccount.com" \
  --sandbox-launcher \
  --set-env-vars GEMINI_ENVIRONMENT_KEY="$GEMINI_ENVIRONMENT_KEY" \
  --command bash \
  --args "^@@^-c@@${STARTUP_SCRIPT}"

# 3. Run an interaction targeting your environment
gemini-api agent run \
  "Create hello.txt and run uname -a in bash." \
  --agent antigravity-preview-05-2026 \
  --body "{\"environment\": \"$GEMINI_ENVIRONMENT_ID\"}"
```

> [!NOTE]
>
> -   **Multi-environment multiplexing**: Pass
>     `--set-env-vars GEMINI_API_KEY=YOUR_API_KEY` (or `--set-secrets`) instead
>     of `GEMINI_ENVIRONMENT_KEY` so one worker pool instance can serve multiple
>     `self_hosted` environments concurrently. Each environment gets an isolated
>     `/tmp/workspaces/<hash>` directory bind-mounted at `/workspace`, while an
>     isolated `/tmp` `tmpfs` masks host `/tmp/workspaces` inside gVisor.
>
> -   **Workspace persistence and Secret Manager**: Keep `--instances 1` so
>     multi-turn sessions reuse `/tmp/workspaces/<hash>` across turns. For
>     long-lived setups, pass
>     `--set-secrets GEMINI_ENVIRONMENT_KEY=YOUR_SECRET:latest` and optionally
>     mount a Cloud Storage bucket via
>     `--add-volume name=ws,type=cloud-storage,bucket=YOUR_BUCKET` and
>     `--add-volume-mount volume=ws,mount-path=/tmp/workspaces`. Grant
>     `gemini-self-hosted@${PROJECT_ID}.iam.gserviceaccount.com`
>     `roles/secretmanager.secretAccessor` on the secret when using
>     `--set-secrets`, and `roles/storage.objectUser` on `YOUR_BUCKET` when
>     mounting a Cloud Storage volume.

## Pause and resume

Scale the worker pool down to `0` instances when idle to stop compute billing,
and scale back to `1` to resume polling:

```bash
# Pause compute billing:
gcloud beta run worker-pools update gemini-self-hosted --instances 0

# Resume polling:
gcloud beta run worker-pools update gemini-self-hosted --instances 1
```

## Cleanup

```bash
gcloud beta run worker-pools delete gemini-self-hosted --region "$REGION" --quiet
gcloud iam service-accounts delete "gemini-self-hosted@${PROJECT_ID}.iam.gserviceaccount.com" --quiet
```
