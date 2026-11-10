# Local Host Execution (`local/`)

Connect your local machine directly to the Gemini API as a **Self-Hosted
Environment (`self_hosted`)** and run agent interactions using the
[`gemini-api` CLI](https://github.com/google-gemini/gemini-api-cli).

> [!CAUTION]
> **Security Risk — Host Shell Execution (`execute_bash`)**
>
> Running the environment runtime directly on your workstation or host operating
> system without container or virtual machine isolation is **inherently
> unsafe**:
>
> -   **Unconfined Shell Execution**: Unless you pass `--disable-bash`, the
>     `execute_bash` tool executes model-generated shell commands
>     (`bash -c ...`) directly on your host OS under your current user account.
> -   **Scope of `--root-dir`**: Passing `--root-dir` confines only the
>     structured file tools (`view_file` / `read_file`, `write_to_file` /
>     `write_file`, `replace_file_content` / `edit_file`, `list_dir`,
>     `grep_search`, and `find_by_name`) so they cannot traverse above
>     `--root-dir`. It **does not** restrict `execute_bash` from reading or
>     modifying `$HOME`, SSH keys, cloud credentials, or local network services.
> -   **Recommended Alternatives**: Only use bare-host local execution for
>     trusted local debugging (or pass `--disable-bash` to restrict the worker
>     to root-confined file tools only). For untrusted prompts or production
>     workloads, isolate the worker using
>     [`containers/`](https://github.com/google-gemini/gemini-api-cli/tree/main/docs/self-hosted/containers)
>     (Docker / Podman),
>     [`agent-substrate/`](https://github.com/google-gemini/gemini-api-cli/tree/main/docs/self-hosted/agent-substrate),
>     or
>     [`providers/`](https://github.com/google-gemini/gemini-api-cli/tree/main/docs/self-hosted/providers).

## Prerequisites

Install the `gemini-api` CLI and export your
[Gemini API key](https://ai.google.dev/gemini-api/docs/api-key):

```bash
curl -fsSL https://raw.githubusercontent.com/google-gemini/gemini-api-cli/main/scripts/install.sh \
  | bash
export PATH="$HOME/.local/bin:$PATH"
export GEMINI_API_KEY="your-api-key"
```

## Quick Start

### 1. Create a `self_hosted` Environment

```bash
curl -fsSL -X POST \
  "https://generativelanguage.googleapis.com/v1beta/environments" \
  -H "x-goog-api-key: $GEMINI_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"mode": "self_hosted"}'
```

Export both the `id` (`GEMINI_ENVIRONMENT_ID`) and `environment_key`
(`GEMINI_ENVIRONMENT_KEY`) from the JSON response. Note that `environment_key`
is returned **only once at creation time**:

```bash
export GEMINI_ENVIRONMENT_ID="environments/..."
export GEMINI_ENVIRONMENT_KEY="ek_..."
```

### 2. Connect the Local Worker (Terminal 1)

Pass only `GEMINI_ENVIRONMENT_KEY` (`--environment-key`) to the worker so your
primary `GEMINI_API_KEY` is not exposed in the worker's environment:

```bash
mkdir -p /tmp/gemini-self-hosted-workspace

env -u GEMINI_API_KEY gemini-api environments connect \
  --environment-key "$GEMINI_ENVIRONMENT_KEY" \
  --root-dir /tmp/gemini-self-hosted-workspace \
  --workdir /tmp/gemini-self-hosted-workspace
```

To allow only root-confined file operations with shell execution disabled, add
`--disable-bash`:

```bash
env -u GEMINI_API_KEY gemini-api environments connect \
  --environment-key "$GEMINI_ENVIRONMENT_KEY" \
  --root-dir /tmp/gemini-self-hosted-workspace \
  --workdir /tmp/gemini-self-hosted-workspace \
  --disable-bash
```

### 3. Run an Interaction (Terminal 2)

In a second terminal, ensure `GEMINI_API_KEY` and `GEMINI_ENVIRONMENT_ID` are
exported, then stream an agent interaction targeting your connected environment:

```bash
export GEMINI_API_KEY="your-api-key"
export GEMINI_ENVIRONMENT_ID="environments/..."

gemini-api agent run \
  "Create a file named hello.txt containing 'Hello from self-hosted!' and read it back." \
  --agent antigravity-preview-05-2026 \
  --body "{\"environment\": \"$GEMINI_ENVIRONMENT_ID\"}"
```

To run asynchronously in the background and inspect the status later:

```bash
INTERACTION_ID=$(gemini-api agent run \
  "Create hello.txt and list the workspace directory." \
  --agent antigravity-preview-05-2026 \
  --background \
  --stream=false \
  --output-format json \
  --jq '.id' \
  --body "{\"environment\": \"$GEMINI_ENVIRONMENT_ID\"}")

gemini-api agent status "$INTERACTION_ID" --stream=false --output-format json
```

## Cleanup

Stop the worker in Terminal 1 (`Ctrl+C`), remove the temporary workspace
directory, and optionally delete the environment resource:

```bash
rm -rf /tmp/gemini-self-hosted-workspace

curl -fsSL -X DELETE \
  "https://generativelanguage.googleapis.com/v1beta/${GEMINI_ENVIRONMENT_ID}" \
  -H "x-goog-api-key: $GEMINI_API_KEY"
```

## CLI Flags (`gemini-api environments connect`)

<!-- mdformat off(GFM single-line table for GitHub compatibility) -->
Flag                              | Environment Variable     | Default               | Description
:-------------------------------- | :----------------------- | :-------------------- | :----------
`--environment-key <key>`         | `GEMINI_ENVIRONMENT_KEY` | *(required)*          | Environment key authenticating this worker to its `self_hosted` environment.
`--root-dir <dir>`                | *(none)*                 | `.`                   | Root directory confining structured file tools via `os.Root`. Does **not** confine `execute_bash`.
`--workdir <dir>`                 | *(none)*                 | `.`                   | Working directory for `execute_bash` and relative tool paths (must reside within `--root-dir`).
`--disable-bash`                  | *(none)*                 | `false`               | Disables the `execute_bash` tool completely.
`--max-concurrent <n>`            | *(none)*                 | `4`                   | Maximum concurrent interactions to execute on this worker.
`--poll-interval <duration>`      | *(none)*                 | `2s`                  | Interval between poll requests when idle.
`--heartbeat-interval <duration>` | *(none)*                 | `5s`                  | In-band stream heartbeat interval during tool execution.
`--command-timeout <duration>`    | *(none)*                 | `2m`                  | Maximum timeout per `execute_bash` command.
`--worker-id <id>`                | `GEMINI_WORKER_ID`       | `worker-<host>-<pid>` | Worker identifier reported when leasing interactions.
`--quiet`                         | *(none)*                 | `false`               | Suppresses operational progress logs on `stderr`.
<!-- mdformat on -->
