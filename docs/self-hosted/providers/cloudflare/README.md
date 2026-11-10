# Cloudflare Sandbox

Run the Gemini API self-hosted environment runtime inside a
[Cloudflare Container](https://developers.cloudflare.com/containers/) using
Cloudflare Workers (`DurableObject` + `ctx.container`) and the `wrangler` CLI.

Unlike interactive CLI microVMs, Cloudflare Sandboxes are triggered via a
minimal Cloudflare Worker (`wrangler.jsonc`, `Dockerfile`, and `src/index.ts` in
this directory). Deploying Cloudflare Containers requires the Cloudflare
**Workers Paid** plan and Docker running locally for `wrangler deploy`.

## Prerequisites

-   Node.js 20+ and `npm`
-   Docker running locally (used by `wrangler deploy` to build `Dockerfile`)
-   The [`gemini-api` CLI](https://github.com/google-gemini/gemini-api-cli) and
    a [Gemini API key](https://ai.google.dev/gemini-api/docs/api-key)
    (`GEMINI_API_KEY`)

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
export CLOUDFLARE_DISPATCH_SECRET="$(openssl rand -hex 32)"
```

## Quick Start

```bash
cd docs/self-hosted/providers/cloudflare
npm install

# 1. Authenticate Wrangler and deploy the Cloudflare Container & Worker first
npx wrangler login
npx wrangler deploy

# 2. Upload your runtime and dispatch secrets to the deployed Worker
printf '%s' "$GEMINI_ENVIRONMENT_KEY" | npx wrangler secret put GEMINI_ENVIRONMENT_KEY
printf '%s' "$CLOUDFLARE_DISPATCH_SECRET" | npx wrangler secret put CLOUDFLARE_DISPATCH_SECRET

# 3. Start the worker loop inside a Cloudflare Sandbox instance
curl -fsSL -X POST "https://gemini-api-self-hosted-cloudflare.<YOUR_SUBDOMAIN>.workers.dev/connect" \
  -H "Authorization: Bearer $CLOUDFLARE_DISPATCH_SECRET" \
  -H "Content-Type: application/json" \
  -d "{\"environment_id\": \"$GEMINI_ENVIRONMENT_ID\"}"
```

## Run an Interaction

```bash
gemini-api agent run \
  "Create hello.txt in /workspace and run uname -a in bash." \
  --agent antigravity-preview-05-2026 \
  --body "{\"environment\": \"$GEMINI_ENVIRONMENT_ID\"}"
```

## Cleanup

```bash
npx wrangler delete
```
