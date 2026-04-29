# Experimental Gemini API CLI

> An experimental CLI for the Gemini API.

> [!CAUTION]
> **Disclaimer**: This is not a supported Google product.

**Status:** Under development.

## Features

- **Model Interactions**: Run prompts against Gemini models with support for streaming, system instructions, and service tiers.
- **Multimodal Support**: Handle image understanding, image generation, and text-to-speech (TTS).
- **Agent Lifecycle**: Scaffold (`init`), deploy (`create`), test, and delete custom agents.
- **Deep Research**: Support for long-running Deep Research tasks with automatic polling and reconnection.
- **Tools**: Integrate tools like Code Execution and Google Search.
- **Environment Management**: Download snapshots of agent environments.

## Installation

The easiest way to install is via the install script:

```bash
curl -fsSL https://raw.githubusercontent.com/google-gemini/gemini-api-cli/main/scripts/install.sh | bash
```

See [DOCS.md](./DOCS.md) for full installation instructions (Manual binary download, npm, or from source).

### Quick Start (from source)

```bash
# Install dependencies
bun install

# Run in dev mode
bun run dev -- run "Hello"

# Build standalone binary
bun run compile ./dist/gemini-api --version
```

## Commands

```bash
# Run an interaction against a model
gemini-api run "What is the capital of France?"

# Scaffold, test, and deploy agents
gemini-api agents init my-agent
gemini-api agents test --prompt "Hello"
gemini-api agents create

# Manage environment files
gemini-api files download <env-id>
```

## E2E Tests

We have a suite of End-to-End (E2E) tests located in `tests/e2e/`.
These scripts run both the `--dry-run` variant (to show the command) and the live variant (to show the result).

To run a specific test:
```bash
bash tests/e2e/cuj_01.sh
```

See [E2E.md](./tests/e2e/E2E.md) for a full list of Critical User Journeys and their status.

## Skills

This repository includes a skill for AI agents to understand how to use this CLI.

- [Gemini API CLI Skill](./skills/gemini-api-cli/SKILL.md) — Comprehensive guide for agents, covering:
  - Normal model calls (text, multi-turn, tools)
  - Agent lifecycle management (init, create, test, delete)
  - Agent calls (invoking Waverunner and Deep Research)
  - Genmedia (image generation, editing, TTS, music)

## Documentation

- [DOCS.md](./DOCS.md) — Full user guide and reference
- [tests/e2e/E2E.md](./tests/e2e/E2E.md) — End-to-End Test Plan


## License

Apache-2.0 — See [LICENSE](./LICENSE)
