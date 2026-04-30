# Agent Calls

This reference covers how to interact with Gemini Agents using the `gemini-api run` command.

## Waverunner (Custom Agents)

When you create a custom agent (which defaults to using `waverunner` as the base agent), you interact with it using the `--agent` flag followed by the agent ID.

```bash
gemini-api run "Analyze this dataset for trends" --agent my-custom-agent
```

If the agent requires files in its workspace, ensure they are provided or already uploaded.

### Passing Environment Sources

When using `--agent waverunner` (or a custom agent based on it), you can pass environment sources using the `--source` flag. This allows you to seed files or clone repositories into the agent's environment. Custom sources override the default auto-enabled environment.

```bash
gemini-api run "Generate a video" \
  --agent waverunner \
  --source "inline:/.agents/README.md:# Instructions" \
  --source "github:https://github.com/user/repo:/.agents"
```

Supported source types:
- `inline:<target>:<content>`: Creates a file with the specified content.
- `github:<url>:<target>`: Clones a GitHub repository to the target path.
- `gcs:<source>:<target>`: Copies files from Google Cloud Storage to the target path.

## Deep Research

Deep Research is a specialized agent designed for long-running, complex research tasks. You can invoke it using the specific agent ID listed in the models/agents table.

Invoke the latest Deep Research agent:

```bash
gemini-api run "Research the latest developments in room-temperature superconductors" \
  --agent deep-research-preview-04-2026
```

Invoke Deep Research Max (for more extensive research):

```bash
gemini-api run "Provide a comprehensive analysis of the global semiconductor supply chain" \
  --agent deep-research-max-preview-04-2026
```

> [!NOTE]
> Deep Research queries often take longer to complete and may involve multiple steps and tool usage (like web search) automatically.
