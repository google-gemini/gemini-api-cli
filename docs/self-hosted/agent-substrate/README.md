# Gemini Self-Hosted Environments for Agent Substrate (`ate-gemini`)

`ate-gemini` integrates the Gemini API CLI (`gemini-api`) self-hosted
environment runtime with Agent Substrate `v1`
(`go.ate.dev/substrate/api/ateapi/v1` and `atenet-router`) on Kubernetes. For a
full cluster setup and deployment walkthrough, see [INSTALL.md](INSTALL.md); for
protocol and security details, see [DESIGN.md](DESIGN.md).

> [!WARNING]
> `ate-gemini` does not authenticate or authorize the users behind an
> interaction. It provisions an actor for every interaction leased to its Gemini
> API project, using the `atespace`, `template`, `env`, and `egress_policy`
> named in that interaction's `custom_config` (limited only by
> `--allowed-atespaces` and `--allowed-templates`). Anyone who can create
> interactions with the project's API key can run code in your Substrate actors.
> The mTLS and `ateapi` authorization described in [DESIGN.md](DESIGN.md) only
> authenticate `ate-gemini`'s own components to Agent Substrate. If other users
> or tenants reach this system through your own service, that service must
> enforce authentication and access control (ACL) checks before calling the
> Gemini API, including which callers may use which `atespace`, `template`, and
> egress rules.

## Architecture Overview

Unlike setups that require an external queue and custom tool execution daemon,
the `gemini-api` CLI binary implements both the stateless control-plane poller
(`gemini-api environments poll`) and the one-shot sandbox worker
(`gemini-api environments serve`). `ate-gemini` bridges those two modes to Agent
Substrate `v1`:

1.  **Controller & Idle-Actor Sweeper (`ate-gemini controller`)**:
    -   Starts `gemini-api environments poll` wired with
        `--provision-cmd="ate-gemini provision ..."`.
    -   Runs a background `WorkerService.GetIdleStatus` sweeper
        (`--worker-idle-timeout=1m0s`) over `atenet-router:443` mTLS that
        automatically suspends continuously idle `RUNNING` actors via
        `ateapi.SuspendActor`.
2.  **Actor Provisioning & Dispatch Hook (`ate-gemini provision`)**:
    -   Reads a `v1` `ProvisionRequest` JSON payload from `stdin`, extracting
        per-environment Substrate and runtime options (`atespace`, `template`,
        `worker_url_template`, `env`, `egress_policy`) only from
        `environment_config.custom_config` (and validating `atespace` and
        `template` against `--allowed-atespaces` and `--allowed-templates`).
    -   Calls `ateapi.Control` (`GetAtespace`, `GetActor`, `CreateActor`,
        `CreateActorEgressPolicy`, `ResumeActor`) in the requested `atespace`
        for a deterministic actor name derived from `environment_id`.
        `custom_config.egress_policy` is a Substrate `EgressPolicy` whose
        `http`, `https`, and `tlsPassthrough` rules are passed through unchanged
        (plus TLS passthrough to the Gemini API host; default deny otherwise).
        Rule `effects` (credential injection, actor JWTs) are rejected. The
        policy is immutable once the actor is created.
    -   Calls gRPC `WorkerService.StartWork` over mTLS via
        `atenet-router.ate-system.svc.cluster.local:443` with metadata
        `ate-target-actor: <atespace>/<actor_name>`.
3.  **In-Actor Daemon & Sandbox Worker (`ate-gemini actor-daemon` +
    `gemini-api environments serve`)**:
    -   `ate-gemini actor-daemon` runs inside the gVisor `ActorTemplate`
        container on port `80` as non-root user `1000:1000` with
        `capabilities.drop: ["ALL"]` and zero file capabilities.
    -   Serves gRPC `WorkerService` (`StartWork`, `GetIdleStatus`) multiplexed
        with HTTP/1.1 `GET /healthz` (`200 OK`) on port `80` for Substrate's
        `WakeupProbe`.
    -   For each `StartWork` RPC, spawns a one-shot `gemini-api environments
        serve --interaction-id=<id> --lease-token=<token>` process that attaches
        directly to the Gemini API bidi stream using only `lease_token` (never
        `GEMINI_API_KEY`).

## Quickstart

### 1. Build `ate-gemini`

```shell
cd docs/self-hosted/agent-substrate
GOTOOLCHAIN=auto CGO_ENABLED=0 go build -o ate-gemini ./cmd/ate-gemini
```

### 2. Package the Container Image

Build and push an OCI container image containing both `ate-gemini` and
`gemini-api` on `$PATH` (in `/usr/local/bin/`) using the included
[`Containerfile`](Containerfile), pinned by digest
(`repo/ate-gemini@sha256:...`):

```shell
docker build --file=Containerfile --tag=us-central1-docker.pkg.dev/YOUR_PROJECT_ID/ate-images/ate-gemini:latest .
docker push us-central1-docker.pkg.dev/YOUR_PROJECT_ID/ate-images/ate-gemini:latest
```

See [INSTALL.md](INSTALL.md) for full cluster setup (including
`ATE_INGRESS_ALLOWED_SPIFFE_IDS` for `atenet-router` mTLS) and deployment
commands.

### 3. Create an Example `Atespace`, Grant `AccessPolicy`, and Apply the `ActorTemplate`

`gemini-self-hosted` and `gemini-env-actor` below are **example** names (and
fallback defaults when omitted from `custom_config`). You can pre-create
multiple tenant `Atespace`s and `ActorTemplate`s on the cluster, list them in
`--allowed-atespaces` / `--allowed-templates`, and select among them per
interaction via `custom_config`:

```shell
kubectl ate create atespace gemini-self-hosted

kubectl ate create access-policy -a gemini-self-hosted -f - <<'EOF'
metadata:
  name: default
bindings:
- role: editor
  members:
  - user:system:serviceaccount:gemini-self-hosted:gemini-substrate-controller
EOF

./ate-gemini manifest template \
    --namespace=gemini-self-hosted \
    --template=gemini-env-actor \
    --worker-image=us-central1-docker.pkg.dev/YOUR_PROJECT_ID/ate-images/ate-gemini@sha256:... \
    | kubectl ate create actor-template -f -
```

### 4. Generate and Apply the Kubernetes Manifests

```shell
export GEMINI_API_KEY="your-gemini-api-key"

./ate-gemini manifest \
    --namespace=gemini-self-hosted \
    --template=gemini-env-actor \
    --worker-image=us-central1-docker.pkg.dev/YOUR_PROJECT_ID/ate-images/ate-gemini@sha256:... \
    --worker-pool-image=ghcr.io/agent-substrate/substrate/ateworker-gvisor:v1.0.0-rc.1 \
    --replicas=3 \
    | kubectl apply --filename=-
```

### 5. Run an Interaction with `custom_config`

Pass the target `atespace`, `template`, and optional `env` or Substrate
`egress_policy` via `environment.custom_config` in the `CreateInteraction`
(`POST /v1beta/interactions`) payload. `ate-gemini` reads only `custom_config`,
and the initial `egress_policy` is immutable once the actor is created:

```shell
curl --silent --fail \
    --request POST \
    --header "Content-Type: application/json" \
    --header "x-goog-api-key: ${GEMINI_API_KEY}" \
    --data '{
      "agent": "antigravity-preview-05-2026",
      "input": "Run uname -a and ls -la /workspace in bash.",
      "stream": false,
      "environment": {
        "type": "remote",
        "mode": "self_hosted",
        "custom_config": {
          "atespace": "gemini-self-hosted",
          "template": "gemini-env-actor"
        }
      }
    }' \
    "https://generativelanguage.googleapis.com/v1beta/interactions"
```

## CLI Subcommands

<!-- mdformat off(GFM single-line table for GitHub compatibility) -->
Subcommand                     | Purpose
:----------------------------- | :------
`ate-gemini manifest`          | Emit Kubernetes YAML (`Namespace` with `ate.dev/api-access=true`, `ServiceAccount`, `Secret` for `GEMINI_API_KEY`, `ate.dev/v1` `WorkerPool`, and hardened controller `Deployment` with `podidentity` mTLS and CLI flags).
`ate-gemini manifest template` | Emit Substrate `v1` `ActorTemplate` YAML (`SNAPSHOT_FIDELITY_MEMORY`, `capabilities.drop: ["ALL"]`) running `ate-gemini actor-daemon --port=80`.
`ate-gemini controller`        | Run `gemini-api environments poll` wired to `ate-gemini provision` alongside a background `WorkerService.GetIdleStatus` idle-actor sweeper (`--worker-idle-timeout=1m0s`).
`ate-gemini actor-daemon`      | Run the in-actor gRPC `WorkerService` server (`StartWork`, `GetIdleStatus`, plus `/healthz`) that spawns one-shot `gemini-api environments serve` worker processes.
`ate-gemini provision`         | Provision/resume a Substrate actor via `ateapi` using options from `environment_config.custom_config` (enforcing `--allowed-atespaces` / `--allowed-templates`) and dispatch the leased interaction via mTLS gRPC `WorkerService.StartWork` through `atenet-router:443`.
`ate-gemini suspend`           | Suspend an idle Substrate actor on demand via `ateapi.SuspendActor`.
`ate-gemini version`           | Print the `ate-gemini` version string.
<!-- mdformat on -->

## Running Tests

```shell
GOTOOLCHAIN=auto go test ./...
```
