# Design: Gemini Self-Hosted Environments on Agent Substrate (`ate-gemini`)

This document describes how `ate-gemini` (`package substrate`) adapts the Gemini
API CLI (`gemini-api`) self-hosted runtime (`environments poll` and
`environments serve`) to Agent Substrate `v1`
(`go.ate.dev/substrate/api/ateapi/v1` and `atenet-router`). For installation
steps, see [INSTALL.md](INSTALL.md).

## Design Goals and Constraints

-   **External CLI runtime binary**: Treat `gemini-api` strictly as an external
    CLI binary invoked via its `environments poll --provision-cmd` and
    `environments serve` subcommands. Do not link internal `gemini-api` Go
    packages into `ate-gemini`.
-   **Zero credentials inside actor pods**: Keep `GEMINI_API_KEY` confined to
    the controller pod (`gemini-credentials` `Secret`, with no RBAC granted to
    `ate-api-server` or any other namespace). Substrate actors receive only
    per-interaction `lease_token` values over gRPC `WorkerService.StartWork`.
-   **Settings as Deployment flags**: Keep only secret credentials
    (`GEMINI_API_KEY` and optional `GEMINI_ENVIRONMENT_KEY`) inside the
    Kubernetes `Secret`. Pass cluster-level infrastructure settings
    (`--ateapi-addr`, `--ateapi-ca-file`, `--atenet-router-addr`,
    `--atenet-client-bundle`, `--control-plane-url`,
    `--max-concurrent-interactions`, `--worker-idle-timeout`,
    `--allowed-atespaces`, `--allowed-templates`) as CLI flags on a single
    cluster-wide `gemini-substrate-controller` `Deployment` so config changes
    roll the pod automatically.
-   **`custom_config` as the only input**: The Gemini API treats `custom_config`
    as opaque JSON for self-hosted environments. `ate-gemini` reads only
    `environment_config.custom_config` (`atespace`, `template`,
    `worker_url_template`, `env`, `egress_policy`) and ignores every other
    `environment_config` field. Callers set it inline on `CreateInteraction`
    (`POST /v1beta/interactions` with `"environment": {"type": "remote", "mode":
    "self_hosted", "custom_config": {...}}`) or when creating a reusable
    `self_hosted` environment (`POST /v1beta/environments`). Unknown keys inside
    `custom_config` are rejected so typos never silently change behavior.
-   **Substrate-native egress passthrough**: `custom_config.egress_policy` is an
    Agent Substrate `EgressPolicy` in protobuf JSON (only `rules`) passed
    unchanged to `ateapi.CreateActorEgressPolicy`, so callers choose which hosts
    and ports actors may reach with Substrate's `http`, `https`, and
    `tlsPassthrough` rules. Rule `effects` (credential header injection and
    actor JWTs) are rejected, because anyone who can create interactions
    controls `custom_config` and could otherwise send operator credentials or
    actor identity tokens to a host of their choosing. `ate-gemini` only appends
    TLS passthrough to the Gemini API host so the worker can connect. Without
    `egress_policy`, actors get Substrate's default deny plus the Gemini API
    host.
-   **Immutable initial egress policy**: The `default` `EgressPolicy` is created
    once when the actor is first created and stays unchanged across subsequent
    turns and resumes.
-   **Memory snapshot lifecycle and idle sweeping**: Use Substrate `v1` memory
    snapshots (`SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY`) with a background
    `WorkerService.GetIdleStatus` sweeper in `ate-gemini controller`
    (`--worker-idle-timeout=1m0s`) so idle actors automatically checkpoint
    process memory and `/workspace` state via `ateapi.SuspendActor` and resume
    on demand for follow-up turns.

## Key Architectural Choices

<!-- mdformat off(GFM single-line table for GitHub compatibility) -->
Concern                               | `ate-gemini` Design Choice                                                                              | Rationale
:------------------------------------ | :------------------------------------------------------------------------------------------------------ | :--------
**Control-plane polling**             | `gemini-api environments poll --provision-cmd="ate-gemini provision ..."`                               | Reuses the upstream CLI's lease polling, background handoff heartbeating, and credential scrubbing with zero external message broker.
**Actor lifecycle (`ateapi` `v1`)**   | `substrate.Manager` (`go.ate.dev/substrate/api/ateapi/v1`) in `provision`, idle sweeper, and `suspend`  | Maps each `environment_id` deterministically to a reusable gVisor actor (`<prefix>-<sha256>`) and suspends continuously idle `RUNNING` actors automatically.
**Tenant & template governance**      | Fail-closed `--allowed-atespaces` and `--allowed-templates` allowlists + OpenFGA `AccessPolicy`         | Restricts caller-supplied `custom_config.atespace` and `custom_config.template` values to operator-approved targets (defaulting strictly to `--atespace` and `--template`).
**Router dispatch (`atenet-router`)** | mTLS gRPC (`podidentity` client cert on `:443`) `WorkerService.StartWork` & `GetIdleStatus`             | Authenticates the controller's SPIFFE identity at `atenet-router:443`, which forwards h2c gRPC with `ate-target-actor` metadata to the target actor on port `80`.
**In-actor process**                  | `ate-gemini actor-daemon --port=80` spawning one-shot `gemini-api environments serve`                   | Keeps a lightweight gRPC/HTTP daemon alive for Substrate `WakeupProbe` checks and `GetIdleStatus` queries while isolating each turn inside its own `environments serve` child process.
**Least-privilege port 80 binding**   | Non-root user `1000:1000` with `capabilities.drop: ["ALL"]` and zero file capabilities (`no setcap`)    | Substrate's gVisor and micro-VM runtimes permit unprivileged port `80` binds with all Linux capabilities dropped, avoiding `execve` `-EPERM` failures when file capabilities are set.
<!-- mdformat on -->

## Hook Protocol & Idle Sweeper (`gemini-api` to `ate-gemini`)

### 1. Provision Hook (`--provision-cmd`)

Callers pass per-environment Substrate and runtime options (`atespace`,
`template`, `worker_url_template`, `env`, `egress_policy`) inside
`custom_config`—for example inline in `CreateInteraction`
(`POST /v1beta/interactions` with `"environment": {"type": "remote", "mode":
"self_hosted", "custom_config": {...}}`) or on `CreateEnvironment`
(`POST /v1beta/environments` with
`{"mode": "self_hosted", "custom_config": {...}}`). The Gemini API echoes
`custom_config` on `LeaseInteractions`, and when `gemini-api environments poll`
leases an interaction for `environment_id`, it executes `ate-gemini provision`
and writes a `v1` JSON `ProvisionRequest` to `stdin` (the `gemini-self-hosted`
and `gemini-env-actor` values below are example names; `custom_config` is the
sole canonical source for per-environment options):

```json
{
  "schema_version": "v1",
  "environment_id": "environments/env_01abc",
  "interaction_id": "inter_02xyz",
  "lease_token": "lt_v1_secret",
  "server_url": "https://generativelanguage.googleapis.com",
  "environment_config": {
    "custom_config": {
      "atespace": "gemini-self-hosted",
      "template": "gemini-env-actor",
      "egress_policy": {
        "rules": [
          {"https": {"hostnames": ["pypi.org", "files.pythonhosted.org"]}},
          {"tlsPassthrough": {"hostnames": ["*.github.com"], "ports": {"all": {}}}}
        ]
      },
      "env": {
        "APP_ENV": {"value": "staging"}
      }
    }
  }
}
```

`ate-gemini provision` performs:

1.  **Deterministic naming & allowlist enforcement**: Computes
    `ActorName(environment_id)` as `<sanitized-prefix>-<16-char-sha256-hex>`
    (valid RFC 1123 DNS label, up to 63 chars), validates `atespace` and
    `template` as RFC 1123 DNS labels, and checks both against the operator's
    `--allowed-atespaces` and `--allowed-templates` allowlists (which default
    fail-closed to permitting only `--atespace` and `--template` unless
    additional names or `*` are configured).
2.  **`Atespace` verification and actor lookup/creation**: Verifies the resolved
    `atespace` exists via `ateapi.GetAtespace` (failing on `NotFound` rather
    than auto-creating namespaces), then calls `ateapi.GetActor`. On `NotFound`,
    calls `ateapi.CreateActor` referencing the resolved `ActorTemplate`.
3.  **Immutable initial egress policy creation**: Creates the actor's `default`
    `EgressPolicy` via `ateapi.CreateActorEgressPolicy` from
    `custom_config.egress_policy.rules` (rejecting any rule with `effects`),
    passed through unchanged and validated by `ate-api-server`, plus one
    `tlsPassthrough` rule on every port for `generativelanguage.googleapis.com`
    and any custom `server_url` hostname. That extra rule is skipped for a host
    the caller already names on every port, since `ate-api-server` rejects two
    rules for the same host on the same ports. Without `egress_policy`, the
    actor gets only the Gemini API rule (Substrate's default deny for everything
    else). Once the `default` `EgressPolicy` exists, it is **immutable** for the
    lifetime of that environment (`AlreadyExists` preserves it without
    overwriting).
4.  **State convergence and wakeup (`substrate.ateapi.v1`)**:
    -   `ACTOR_STATE_RUNNING`: Returns immediately.
    -   `ACTOR_STATE_SUSPENDED` / `ACTOR_STATE_RESUMING` /
        `ACTOR_STATE_UNSPECIFIED`: Calls `ateapi.ResumeActor`, which blocks
        until the actor's `/healthz` `WakeupProbe` returns HTTP 200.
    -   `ACTOR_STATE_SUSPENDING` / `ACTOR_STATE_REVERTING`: Polls `GetActor`
        until settled, then calls `ResumeActor`.
    -   `ACTOR_STATE_CRASHED`: Calls `RevertActor` followed by `ResumeActor`.
5.  **Work dispatch (`WorkerService.StartWork`)**: Dials
    `atenet-router.ate-system.svc.cluster.local:443` over mTLS (presenting the
    projected `podidentity` client certificate and verifying `servicedns-ca`)
    with `ate-target-actor: <atespace>/<actor_name>` metadata and calls
    `WorkerService.StartWork`. Inside the actor, `ate-gemini actor-daemon`
    spawns a one-shot
    `gemini-api environments serve --interaction-id=<id> --lease-token=<token>`
    subprocess.
6.  **Response**: Writes `v1` `ProvisionResponse` JSON to `stdout`:

```json
{
  "schema_version": "v1",
  "uri": "atenet-router.ate-system.svc.cluster.local:443",
  "headers": {
    "ate-target-actor": "gemini-self-hosted/environments-env-01abc-c4820b025bf5c2b5",
    "x-envoy-upstream-rq-timeout-ms": "60000"
  },
  "worker_id": "environments-env-01abc-c4820b025bf5c2b5",
  "metadata": {
    "action": "created",
    "atespace": "gemini-self-hosted",
    "template": "gemini-env-actor"
  }
}
```

### 2. Background Idle-Actor Sweeper & Standalone `ate-gemini suspend`

Because `gemini-api environments poll` only invokes `--provision-cmd` when
leasing interactions, `ate-gemini controller` runs a built-in background idle
sweeper (`--worker-idle-timeout=1m0s`, `--idle-sweep-interval=10s`; to disable
it, run `ate-gemini controller --worker-idle-timeout=0`, since
`ate-gemini manifest` requires a positive timeout):

1.  Calls `ateapi.ListActors` across allowed `Atespace`s and filters strictly to
    `ACTOR_STATE_RUNNING` actors matching `--allowed-templates` (skipping
    `SUSPENDED` actors so `atenet-router` never auto-wakes them, and skipping
    36-character UUID golden-snapshot bake actors).
2.  Queries `WorkerService.GetIdleStatus` on each running actor over
    `atenet-router:443`.
3.  Once an actor reports `running == false && active_interactions == 0`
    continuously for `--worker-idle-timeout`, calls `ateapi.SuspendActor` to
    checkpoint its memory and durable `/workspace` snapshot.

Operators or external workflows can also suspend a specific actor on demand via
`ate-gemini suspend` with a `v1` `SuspendRequest` JSON payload on `stdin`:

```json
{
  "schema_version": "v1",
  "environment_id": "environments/env_01abc",
  "worker_id": "environments-env-01abc-c4820b025bf5c2b5",
  "idle_duration": "62.4s",
  "environment_config": {
    "custom_config": {
      "atespace": "gemini-self-hosted"
    }
  }
}
```

## Internal Controller-to-Actor `WorkerService` gRPC Protocol (`pkg/workerpb`)

`pkg/workerpb` defines the **internal** gRPC control protocol between
`ate-gemini` in the controller Pod and `ate-gemini actor-daemon` inside the
gVisor actor over `atenet-router`:

```protobuf
syntax = "proto3";

package gemini.selfhosted.worker;

option go_package = "github.com/google-gemini/gemini-api-cli/docs/self-hosted/agent-substrate/pkg/workerpb;workerpb";

service WorkerService {
  rpc StartWork(StartWorkRequest) returns (StartWorkResponse);
  rpc GetIdleStatus(GetIdleStatusRequest) returns (GetIdleStatusResponse);
}
```

## Security Boundary

-   **Credential isolation**: `GEMINI_API_KEY` is stored in the
    `gemini-credentials` Kubernetes `Secret` and mounted **only** into the
    hardened `gemini-substrate-controller` `Deployment` (`runAsNonRoot: true`,
    `readOnlyRootFilesystem: true`, `capabilities.drop: ["ALL"]`). No RBAC
    `Role` or `RoleBinding` grants `ate-api-server` or any other subject read
    access to `gemini-credentials`. `gemini-api environments poll` scrubs
    `GEMINI_API_KEY`, `GOOGLE_API_KEY`, `GEMINI_ENVIRONMENT_KEY`, and
    `GEMINI_LEASE_TOKEN` from child process environments.
-   **`atenet-router` mTLS & `ateapi` OpenFGA authorization**: The controller
    runs under a dedicated `gemini-substrate-controller` `ServiceAccount`,
    authenticates to `ate-api-server:443` with a projected `ServiceAccountToken`
    scoped by an `Atespace` OpenFGA `AccessPolicy` (`role: editor`), and
    authenticates to `atenet-router:443` over mTLS using a projected
    `podidentity.podcert.ate.dev/identity` client certificate allowlisted via
    `ATE_INGRESS_ALLOWED_SPIFFE_IDS`.
-   **`ActorTemplate` and `actor-daemon` isolation**: `BuildActorTemplate` drops
    `ALL` Linux capabilities (`capabilities.drop: ["ALL"]`), adds zero
    capabilities, and never injects `GEMINI_API_KEY` or
    `GEMINI_ENVIRONMENT_KEY`. `ate-gemini actor-daemon` runs as non-root user
    `agent` (`1000:1000` with no `setcap` file capabilities), scrubs ambient
    credentials and cluster discovery variables from `os.Environ()`, and blocks
    overrides of `PATH`, `HOME`, `LD_*`, `BASH_ENV`, `PYTHONPATH`,
    `NODE_OPTIONS`, `KUBERNETES_SERVICE_*`, `SSL_CERT_FILE`, `SSL_CERT_DIR`,
    proxy settings, and credential variables.
-   **No end-user authentication or authorization**: Everything above protects
    `ate-gemini`'s own credentials and its calls to Agent Substrate. It does not
    identify or authorize the callers who create interactions: any interaction
    leased to the project is provisioned with its `custom_config`, limited only
    by `--allowed-atespaces` and `--allowed-templates`. Deployments that serve
    other users or tenants must enforce authentication and access control in
    the service that calls the Gemini API on their behalf.
