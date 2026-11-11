# Docker Guide for Gemini API Self-Hosted Environments

Run Gemini API Self-Hosted Environments inside hardened **Docker** containers
directly with `docker` and the
[`gemini-api` CLI](https://github.com/google-gemini/gemini-api-cli)—either as a
**single-container worker** (`environments connect`) or as a **split
control-plane poller + zero-secret sandbox workers** (`environments poll` +
`environments serve`).

## Prerequisites

-   Docker Engine 24.0+ (or Docker Desktop)
-   The [`gemini-api` CLI](https://github.com/google-gemini/gemini-api-cli) on
    the host (plus `jq` if running the split poller in Option 2)
-   A [Gemini API key](https://ai.google.dev/gemini-api/docs/api-key)
    (`GEMINI_API_KEY`) or a Self-Hosted Environment key
    (`GEMINI_ENVIRONMENT_KEY`)

## Step 1: Build the Runtime Container Image

Build `gemini-self-hosted-runtime:latest` from
[`docs/self-hosted/containers/Dockerfile`](https://github.com/google-gemini/gemini-api-cli/blob/main/docs/self-hosted/containers/Dockerfile):

```bash
cd docs/self-hosted/containers/docker
docker build -t gemini-self-hosted-runtime:latest -f ../Dockerfile ..
```

Verify the image and CLI version:

```bash
docker run --rm gemini-self-hosted-runtime:latest --help
```

--------------------------------------------------------------------------------

## Option 1: Single Container (`environments connect`)

A single hardened Docker container connects directly to one pre-registered
`self_hosted` environment, polls for interactions, and executes file and bash
tools inside `/workspace`.

1.  Create a `self_hosted` environment and export both `GEMINI_ENVIRONMENT_ID`
    and `GEMINI_ENVIRONMENT_KEY` (`environment_key` is returned **only once at
    creation time**):

    ```bash
    curl -fsSL -X POST \
      "https://generativelanguage.googleapis.com/v1beta/environments" \
      -H "x-goog-api-key: ${GEMINI_API_KEY}" \
      -H "Content-Type: application/json" \
      -d '{"mode": "self_hosted"}'

    export GEMINI_ENVIRONMENT_ID="..."   # "id" from response (no "environments/" prefix)
    export GEMINI_ENVIRONMENT_KEY="..."  # "environment_key" from response
    ```

2.  Run the hardened worker container in Terminal 1 (`-e GEMINI_ENVIRONMENT_KEY`
    forwards the variable from your shell environment without exposing its value
    in process arguments):

    ```bash
    docker run --rm -i \
      --label self-hosted.managed=true \
      --cap-drop=ALL \
      --security-opt=no-new-privileges \
      --pids-limit=256 \
      --memory=2g --cpus=2 \
      --read-only \
      --tmpfs /tmp:rw,noexec,nosuid,size=512m \
      -v gemini-self-hosted-workspace:/workspace:rw \
      -e GEMINI_ENVIRONMENT_KEY \
      gemini-self-hosted-runtime:latest \
      environments connect --root-dir=/workspace --workdir=/workspace
    ```

3.  In Terminal 2, run an agent interaction targeting your environment:

    ```bash
    gemini-api agent run \
      "Create hello.txt in /workspace and run uname -a in bash." \
      --agent antigravity-preview-05-2026 \
      --body "{\"environment\": \"${GEMINI_ENVIRONMENT_ID}\"}"
    ```

--------------------------------------------------------------------------------

## Option 2: Split Poller + Zero-Secret Sandbox Workers (`environments poll` + `environments serve`)

For multi-environment fleets or stricter credential isolation, run
`gemini-api environments poll` on the host with your `GEMINI_API_KEY` and let
`--provision-cmd` launch a fresh ephemeral `--rm` Docker container per turn:

-   Long-lived credentials (`GEMINI_API_KEY` / `GEMINI_ENVIRONMENT_KEY`) **never
    enter the sandbox container**—each container receives only the short-lived,
    single-interaction `--lease-token`.
-   Mounting a deterministic per-environment volume
    (`gemini-self-hosted-<sha256_prefix>`) at `/workspace` preserves files
    across multi-turn conversations while scaling idle containers to zero
    (`--rm`) between turns.

1.  Install the `gemini-api` CLI on your host (for Linux, macOS, or WSL) and
    ensure `jq` is installed:

    ```bash
    curl -fsSL https://raw.githubusercontent.com/google-gemini/gemini-api-cli/main/scripts/install.sh \
      | bash
    export PATH="$HOME/.local/bin:$PATH"
    ```

2.  In Terminal 1, export `GEMINI_API_KEY`, clear any single-environment
    variables from Option 1 (unless scoping the poller to a specific
    environment), and start `environments poll` with a direct `docker run`
    `--provision-cmd`:

    ```bash
    export GEMINI_API_KEY="your-api-key"
    unset GEMINI_ENVIRONMENT_ID GEMINI_ENVIRONMENT_KEY

    PROVISION_CMD="$(cat <<'EOF'
    set -eu
    vars="$(jq -re '
      def ok: type == "string" and test("\\A[^-\u0000-\u0020\u007f][^\u0000-\u0020\u007f]*\\z");
      def valid_srv: . == null or . == "" or (ok and (test("@") | not) and test("\\A(https://[A-Za-z0-9.\\[\\]:-]+(:[0-9]+)?(/.*)?|http://(localhost|127\\.0\\.0\\.1|\\[::1\\])(:[0-9]+)?(/.*)?)\\z"));
      if (type == "object")
         and (.schema_version == null or .schema_version == "v1")
         and (.interaction_id | ok)
         and (.lease_token | ok)
         and ((.environment_id // "default") | type == "string" and length > 0)
         and (.server_url | valid_srv)
      then
        "id=\(.interaction_id | @sh)\ntok=\(.lease_token | @sh)\nenv_id=\((.environment_id // "default") | @sh)\nsrv=\((.server_url // "") | @sh)"
      else error("invalid provision payload") end
    ')"
    eval "$vars"
    vol="gemini-self-hosted-$(printf '%s' "$env_id" | (sha256sum 2>/dev/null || shasum -a 256) | cut -c1-16)"
    exec docker run --rm -d \
      --label self-hosted.managed=true \
      --cap-drop=ALL \
      --security-opt=no-new-privileges \
      --pids-limit=256 \
      --memory=2g --cpus=2 \
      --read-only \
      --tmpfs /tmp:rw,noexec,nosuid,size=512m \
      -v "${vol}:/workspace:rw" \
      gemini-self-hosted-runtime:latest \
      environments serve \
        --interaction-id="$id" \
        --lease-token="$tok" \
        ${srv:+--server-url="$srv"} \
        --root-dir=/workspace \
        --workdir=/workspace >/dev/null
    EOF
    )"

    gemini-api environments poll --provision-cmd "$PROVISION_CMD"
    ```

    *(For fully ephemeral single-turn `/workspace` storage instead of
    per-environment volume persistence, replace `-v "${vol}:/workspace:rw"` with
    `--tmpfs /workspace:rw,exec,nosuid,uid=1000,gid=1000,size=1g`.)*

3.  In Terminal 2, create a `self_hosted` environment and dispatch an
    interaction:

    ```bash
    GEMINI_ENVIRONMENT_ID=$(curl -fsSL -X POST \
      "https://generativelanguage.googleapis.com/v1beta/environments" \
      -H "x-goog-api-key: ${GEMINI_API_KEY}" \
      -H "Content-Type: application/json" \
      -d '{"mode": "self_hosted"}' | jq -r '.id')

    gemini-api agent run \
      "Create hello.txt in /workspace and run id in bash." \
      --agent antigravity-preview-05-2026 \
      --body "{\"environment\": \"${GEMINI_ENVIRONMENT_ID}\"}"
    ```

## Inspecting and Cleaning Up Docker Containers & Volumes

```bash
# List active self-hosted Docker sandbox worker containers
docker ps -a --filter "label=self-hosted.managed=true"

# Stop and remove any active self-hosted Docker sandbox worker containers
for id in $(docker ps -aq --filter "label=self-hosted.managed=true"); do
  docker rm -f "$id"
done

# Remove persistent /workspace Docker volumes
for vol in $(docker volume ls -q --filter "name=gemini-self-hosted-"); do
  docker volume rm -f "$vol"
done
```
