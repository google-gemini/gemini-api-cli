# Harness comparison eval

Measures which CLI harness lets a coding agent make the correct API call in the
fewest turns: the **generated** Speakeasy CLI (`gemini-api`, this repo) vs the
**reference** hand-written CLI (`gemini-api-cli`, `reference/gemini-api-cli-poc`).

## How it works

Each run:

1. Starts a fresh mock Gemini Interactions API server (captures every request,
   serves canned happy-path JSON for agents/interactions routes).
2. Spawns a Gemini agent via the **Antigravity SDK** (`google-antigravity`
   Python runtime, driven from `driver/antigravity_driver.py`) with
   **run_command as the only tool**, a system prompt naming the CLI under
   test, and env plumbing pointing the CLI at the mock server (`--server-url`
   instruction for the generated CLI, `GOOGLE_GEMINI_BASE_URL` for the
   reference CLI — each harness's native affordance).
3. Gives the agent a harness-agnostic natural-language task ("Create a managed
   agent with id …").
4. Watches the mock server for the expected request (method + path + body
   predicate) and records how many Bash invocations / assistant turns it took.

The agent is not told command syntax — discovering usage via `--help` is part
of what's being measured.

## Metrics per run

- `apiCallMade` — the correct request hit the mock server
- `bashCallsToApiCall` / `assistantTurnsToApiCall` — effort until the call
- `totalBashCalls`, `numTurns`, `durationMs`, `costUsd`
- `bashCommands` + `requestLog` — full traces for qualitative comparison

## Scenarios

`create-agent`, `list-agents`, `get-agent`, `delete-agent`,
`run-agent-background`, `interaction-status`, `cancel-interaction` — the
surface both CLIs share (managed agents + interactions).

## Usage

```bash
cd evals/harness-comparison
npm install
npm run setup                 # builds both CLIs into bin/<harness>/

# Full matrix (11 scenarios × 2 harnesses × 1 repeat)
npm run eval

# Subset / options
npm run eval -- --scenarios list-agents,create-agent --harnesses generated \
  --repeats 3 --max-turns 12 --model gemini-3.8-flash
```

Auth: export `EVAL_GEMINI_API_KEY` (a Gemini API key; the driver passes it
to the Antigravity SDK explicitly, so it never collides with the fake
`GEMINI_API_KEY` given to the CLI under test). Each run costs real tokens —
a full matrix is 22 agent sessions.

Results land in `results/run-<timestamp>.{json,md}` plus a PR-comment-ready
`run-<timestamp>-comment.md` (prompt / success / turns-to-API-call table);
`--comment-out <path>` also writes that comment to a stable path. Re-render a
comment from saved results without re-running agents:
`npx tsx src/render-comment.ts results/run-....json`.

Each agent session has a 60-second deadline by default (override with
`--case-timeout-seconds`), and the runner terminates the session's full process
group on expiry. Results and the PR comment are checkpointed after every case.

## CI

`.github/workflows/harness-eval.yaml` reruns the eval on every pull request
and upserts a sticky PR comment (marker `<!-- harness-eval-comment -->`) with
the prompt/success/turns table, plus uploads raw results as an artifact.
Requires the `GEMINI_EVAL_API_KEY` repository secret; the job skips itself
gracefully when the secret is unavailable (fork PRs). Model is pinned to
`gemini-3.6-flash` for cost and comparability (and reported in the comment) —
a full matrix is 22 agent sessions per PR.

## Fairness notes / caveats

- Both harnesses get an identical system-prompt template; only the CLI name and
  the one-line server plumbing differ.
- The generated CLI runs in plain non-interactive mode unless the agent opts
  into `--agent-mode`. Its eval shim redirects incidental stdin to `/dev/null`
  because Antigravity keeps an otherwise-empty pipe open; explicit
  `--body @-` input is preserved.
- Reference `agent ls` / `pull` and all modality commands (generate, image, …)
  hit APIs outside the Interactions spec and are not in the scenario set.
- `GEMINI_*`/`GOOGLE_*` env vars are stripped and CLI config is redirected to a
  temp `XDG_CONFIG_HOME`/`GEMINI_CONFIG_DIR`, so runs can't touch real
  credentials or state.
