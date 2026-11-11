# Google Cloud Run

Run the Gemini API self-hosted environment runtime inside an isolated
[Google Cloud Run Job](https://cloud.google.com/run/docs/create-jobs) using the
`gcloud` CLI—with no custom `Dockerfile` required.

## Prerequisites

Install the [`gemini-api` CLI](https://github.com/google-gemini/gemini-api-cli)
and [Gemini API key](https://ai.google.dev/gemini-api/docs/api-key) on your
local machine, plus the `gcloud` CLI:

```bash
export REGION="us-central1"
gcloud auth login
gcloud config set project YOUR_PROJECT_ID
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
# 1. Create a Cloud Run Job using a stock Ubuntu container
gcloud run jobs deploy gemini-self-hosted \
  --image ubuntu:24.04 \
  --set-env-vars GEMINI_ENVIRONMENT_KEY="$GEMINI_ENVIRONMENT_KEY" \
  --command bash \
  --args "-c,apt-get update && apt-get install -y curl ca-certificates && \
curl -fsSL https://raw.githubusercontent.com/google-gemini/gemini-api-cli/main/scripts/install.sh \
| GEMINI_INSTALL_DIR=/usr/local/bin bash && gemini-api environments connect"

# 2. Start the job in the background
gcloud run jobs execute gemini-self-hosted --async

# 3. Run an interaction targeting your environment
gemini-api agent run \
  "Create hello.txt and run uname -a in bash." \
  --agent antigravity-preview-05-2026 \
  --body "{\"environment\": \"$GEMINI_ENVIRONMENT_ID\"}"
```

## Cleanup

```bash
gcloud run jobs delete gemini-self-hosted --region "$REGION" --quiet
```
