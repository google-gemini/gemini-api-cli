# Cloud & MicroVM Sandbox Providers

Most subfolders show how to start an isolated microVM or container with a
provider's CLI, install `gemini-api`, and run `environments connect` in three
commands. [Google Cloud Run](./cloud-run/README.md) instead runs split
`environments poll` and `environments serve` on a Cloud Run Worker Pool with
Cloud Run Sandboxes:

```bash
# 1. Create an isolated instance with the provider's CLI
# 2. Install gemini-api inside the instance:
curl -fsSL https://raw.githubusercontent.com/google-gemini/gemini-api-cli/main/scripts/install.sh \
  | bash

# 3. Connect to your Gemini API environment:
PATH="$HOME/.local/bin:$PATH" gemini-api environments connect \
  --environment-key="$GEMINI_ENVIRONMENT_KEY"
```

## Creating an Environment & Running an Interaction

1.  Export your [Gemini API key](https://ai.google.dev/gemini-api/docs/api-key)
    and create a `self_hosted` environment to obtain its `id`
    (`GEMINI_ENVIRONMENT_ID`) and scoped `environment_key`
    (`GEMINI_ENVIRONMENT_KEY`, returned **only once at creation time**):

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

2.  Once your worker is running `environments connect` (or `environments poll`
    on [Google Cloud Run](./cloud-run/README.md)), send an interaction from your
    local machine with `gemini-api agent run`:

```bash
gemini-api agent run \
  "Create hello.txt and run uname -a in bash." \
  --agent antigravity-preview-05-2026 \
  --body "{\"environment\": \"$GEMINI_ENVIRONMENT_ID\"}"
```

## Providers

Provider                        | CLI                           | Guide
:------------------------------ | :---------------------------- | :----
**Vercel Sandbox**              | `sandbox` (`@vercel/sandbox`) | [`vercel/README.md`](./vercel/README.md)
**Daytona**                     | `daytona`                     | [`daytona/README.md`](./daytona/README.md)
**E2B**                         | `e2b` (`@e2b/cli`)            | [`e2b/README.md`](./e2b/README.md)
**Modal**                       | `modal`                       | [`modal/README.md`](./modal/README.md)
**Fly.io (Sprites & Machines)** | `sprite` / `fly` (`flyctl`)   | [`fly-machines/README.md`](./fly-machines/README.md)
**Google Cloud Run**            | `gcloud`                      | [`cloud-run/README.md`](./cloud-run/README.md) (split `poll`/`serve` Worker Pool + Sandboxes)
**Cloudflare Sandbox**          | `npx wrangler`                | [`cloudflare/README.md`](./cloudflare/README.md)
