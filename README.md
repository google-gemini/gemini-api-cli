# gemini-api CLI

> The official CLI for the Gemini API.

**Status:** Under development. See [SPEC.md](./SPEC.md) and [tasks/TASK.md](./tasks/TASK.md) for progress.

## Quick Start

```bash
# Install dependencies
bun install

# Run in dev mode
bun run dev -- run "Hello" --api-key YOUR_KEY

# Build standalone binary
bun run compile
./dist/gemini-api --version
```

## Commands

```bash
# Run an interaction against a model
gemini-api run "What is the capital of France?"
gemini-api run "Explain this" --model gemini-3-pro-preview

# Scaffold, test, and deploy agents
gemini-api agents init my-agent
gemini-api agents test --prompt "Hello"
gemini-api agents create
gemini-api agents list

# Manage environment files
gemini-api files list <env-id>
gemini-api files download <env-id>
```

## Documentation

- [SPEC.md](./SPEC.md) — Architecture specification
- [tasks/TASK.md](./tasks/TASK.md) — Implementation task tracker
- [tasks/DOCS.md](./tasks/DOCS.md) — User-facing documentation (in progress)
- [CLI-skill.md](./CLI-skill.md) — Agent-first CLI design principles

## License

Apache-2.0 — See [LICENSE](./LICENSE)
