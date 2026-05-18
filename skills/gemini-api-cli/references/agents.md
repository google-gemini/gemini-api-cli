# Gemini Agents

This reference covers managing the lifecycle of Gemini Agents using the CLI.

## Scaffolding a New Agent

Create a new agent project directory:

```bash
gemini-api agents init my-agent
```

This creates a directory with the default structure.

### Agent Directory Structure

```
my-agent/
├── agent.yaml       # Configuration (ID, tools, etc.) — not inlined
├── AGENTS.md        # System instructions (inlined to /.agents/AGENTS.md)
├── .env             # Credentials (inlined to /credentials/.env)
├── skills/          # Custom skills (all files inlined recursively)
└── workspace/       # Files seeded into remote environment (all files inlined recursively)
```

> **Note:** Only `AGENTS.md`, `.env`, `workspace/`, and `skills/` are inlined from the agent directory. All other root-level files and directories (e.g., `README.md`, `package.json`) are ignored during `agents test` and `agents create`.

### Adding Skills

To add a skill to your agent, place the skill files (e.g., `SKILL.md`, `references/`) in the `skills/` directory of your agent project. The agent will have access to these skills when running in its environment.

### Agent Configuration (`agent.yaml`)

Required fields:
- `id`: Agent identifier
- `base_agent`: Base model (Supported only: `antigravity-preview-05-2026`)

Optional fields:
- `description`: Description of the agent
- `system_instruction`: Short system instruction (prefer `AGENTS.md` for long instructions)
- `tools`: List of tools (e.g., `code_execution`, `google_search`)
- `environment`: `"remote"`, a base environment ID, or a structured config object to configure sources, network allowlists, and secrets injection:
  ```yaml
  environment:
    type: "remote"
    sources:
      - type: "gcs"
        source: "gs://my-bucket/folder/"
        target: ".agents/workspace"
    network:
      allowlist:
        - domain: "api.github.com"
          transform:
            Authorization: "Bearer your-github-token"
        - domain: "*" # Catch-all
  ```

## Testing an Agent Locally

You can test your agent configuration locally before creating it on the platform:

```bash
cd my-agent
gemini-api agents test --prompt "Hello, what are your instructions?"
```

Or specify the path:

```bash
gemini-api agents test --prompt "Hello" --path ./my-agent
```

## Creating/Deploying an Agent

Deploy the agent to the platform:

```bash
cd my-agent
gemini-api agents create
```

This will output the agent ID and environment ID.

## Running an Interaction with an Agent

Once created, you can run prompts against the agent:

```bash
gemini-api run "Analyze my data" --agent my-agent
```

## Listing Agents

List all agents you have created:

```bash
gemini-api agents list
```

Use `--json` for machine-readable output:

```bash
gemini-api agents list --json
```

## Deleting an Agent

Delete an agent from the platform:

```bash
gemini-api agents delete my-agent
```

Use `--force` to skip confirmation:

```bash
gemini-api agents delete my-agent --force
```

## Downloading Files from Agent Environment

You can download all files from an agent's environment (e.g., generated data, logs) as a snapshot:

```bash
gemini-api files download <env-id>
```

Specify an output directory:

```bash
gemini-api files download <env-id> --output ./results
```

## Handling Long-Running Tests

Tests initiated via `agents test` can take multiple minutes to complete.
- **Do Not Interrupt**: Be patient and allow the command to run.
- **Streaming**: Streaming is enabled by default. The CLI will stream output live.

## Best Practices

- Always check that the current directory contains `agent.yaml` before running local commands.
- Use `--dry-run` to see what the CLI would do without executing.
- Prefer `AGENTS.md` for detailed instructions rather than putting them all in `agent.yaml`'s `system_instruction`.
