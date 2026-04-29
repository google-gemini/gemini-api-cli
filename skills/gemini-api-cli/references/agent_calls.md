# Agent Calls

This reference covers how to interact with Gemini Agents using the `gemini-api run` command.

## Waverunner (Custom Agents)

When you create a custom agent (which defaults to using `waverunner` as the base agent), you interact with it using the `--agent` flag followed by the agent ID.

```bash
gemini-api run "Analyze this dataset for trends" --agent my-custom-agent
```

If the agent requires files in its workspace, ensure they are provided or already uploaded.

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
