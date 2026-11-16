# Cloudflare Sandbox

Run the Gemini API self-hosted environment runtime inside a
[Cloudflare Container](https://developers.cloudflare.com/containers/) using
Cloudflare Workers (`DurableObject` + `ctx.container`) and the `wrangler` CLI.

Unlike interactive CLI microVMs, Cloudflare Sandboxes are triggered via a
minimal Cloudflare Worker (`wrangler.jsonc`, `Dockerfile`, and `src/index.ts` in
this directory). Deploying Cloudflare Containers requires the Cloudflare
**Workers Paid** plan. You can deploy through the dashboard or manually.

## Prerequisites

-   The [`gemini-api` CLI](https://github.com/google-gemini/gemini-api-cli) and
    a [Gemini API key](https://ai.google.dev/gemini-api/docs/api-key)
    (`GEMINI_API_KEY`)
-   For manual deployment: Git, Node.js 22.12+, `npm`, and Docker running locally
    (used by `wrangler deploy` to build `Dockerfile`)

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
export CLOUDFLARE_DISPATCH_SECRET="$(openssl rand -hex 32)"
```

## Quick Start

Choose one deployment method, then connect the runtime to your Gemini environment.

### Deploy to Cloudflare

[![Deploy to Cloudflare](https://deploy.workers.cloudflare.com/button)](https://deploy.workers.cloudflare.com/?url=https://github.com/google-gemini/gemini-api-cli/tree/main/docs/self-hosted/providers/cloudflare)

Follow the prompts to create a repository. Enter these values when prompted,
then complete the deployment:

| Secret | Value |
| --- | --- |
| `GEMINI_ENVIRONMENT_KEY` | The `environment_key` returned when you created the environment |
| `CLOUDFLARE_DISPATCH_SECRET` | The dispatch secret you generated in your local shell |

The deployment form reads the secret names from `.env.example` and their
descriptions from `package.json`. Save the deployed Worker URL for the connection
request.

### Deploy manually

1. Clone the repository, open this example, and install its dependencies:

   ```bash
   git clone https://github.com/google-gemini/gemini-api-cli.git
   cd gemini-api-cli/docs/self-hosted/providers/cloudflare
   npm install
   npx wrangler login
   ```

2. Copy the example secrets file:

   ```bash
   cp .env.example .env
   ```

   In `.env`, fill in `GEMINI_ENVIRONMENT_KEY` and
   `CLOUDFLARE_DISPATCH_SECRET` with the values you saved earlier. This local file
   is excluded from version control.

3. With Docker running, deploy the Worker, container image, and secrets together:

   ```bash
   npx wrangler deploy --secrets-file .env
   ```

   If Wrangler reports that Durable Object Container settings were not fully
   applied on the first deployment, run this command again.

### Connect the runtime

After either deployment method, set `WORKER_URL` to your deployed Worker URL and
start the runtime:

```bash
export WORKER_URL="https://gemini-api-self-hosted-cloudflare.<YOUR_SUBDOMAIN>.workers.dev"

curl -fsSL -X POST "$WORKER_URL/connect" \
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

## Add or update secrets

After deployment, use `wrangler secret put` to add a secret or replace its value.
Run these commands from the project directory containing your deployed Worker's
Wrangler configuration:

```bash
npx wrangler secret put GEMINI_ENVIRONMENT_KEY
npx wrangler secret put CLOUDFLARE_DISPATCH_SECRET
```

Each command prompts for the value, creates a new Worker version, and deploys it
immediately. To add another secret, use its binding name instead.

If you also use `.env`, update the matching value there before your next
`wrangler deploy --secrets-file .env`, so it does not restore an old value.
Secrets omitted from the file are preserved. Keep the dispatch secret in your
calling application in sync with the Worker secret.

## Cleanup

Delete the Worker and its backing Cloudflare Container application (deleting
only the Worker leaves the Container application active and billing):

```bash
npx wrangler delete
npx wrangler containers list
npx wrangler containers delete <CONTAINER_ID>
```
