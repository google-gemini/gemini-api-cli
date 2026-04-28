# gemini-api CLI

> The official CLI for the Gemini API.

**Status:** Under development. See [SPEC.md](./SPEC.md) and [tasks/TASK.md](./tasks/TASK.md) for progress.

## Installation

See [DOCS.md](./DOCS.md) for full installation instructions (Binary, npm, or from source).

### Quick Start (from source)

```bash
# Install dependencies
bun install

# Run in dev mode
bun run dev -- run "Hello"

# Build standalone binary
bun run compile
./dist/gemini-api --version
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
gemini-api files list <env-id>
```

## Documentation

- [DOCS.md](./DOCS.md) — Full user guide and reference
- [SPEC.md](./SPEC.md) — Architecture specification
- [tasks/TASK.md](./tasks/TASK.md) — Implementation task tracker
- [CLI-skill.md](./CLI-skill.md) — Agent-first CLI design principles

## License

Apache-2.0 — See [LICENSE](./LICENSE)
