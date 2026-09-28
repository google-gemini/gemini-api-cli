"""Antigravity SDK driver for the harness eval.

Reads a JSON config file (argv[1]) and drives one agent session with the
run_command tool only. Emits JSONL events on stdout:
  {"type": "tool_call", "name": "...", "command": "..."}
  {"type": "result", "text": "...", "turns": N, "toolCalls": N,
   "model": "...", "usage": {...}, "error": null}

Config:
  {"task": str, "system": str, "cwd": str, "env": {..}, "model": str|null,
   "maxToolCalls": int, "apiKey": str}
"""

import asyncio
import json
import os
import sys

# The config's env dict (intended for the CLI under test's subprocesses) is
# applied broadly by the runtime; scrub any inherited Gemini/Google overrides
# so the driver's own model traffic can't be redirected before we pin an
# explicit endpoint below.
for _k in [k for k in os.environ if k.startswith(("GEMINI_", "GOOGLE_"))]:
    del os.environ[_k]

from google.antigravity import (
    Agent,
    BuiltinTools,
    CapabilitiesConfig,
    LocalAgentConfig,
)
from google.antigravity.hooks.policy import allow_all
from google.antigravity import models

GEMINI_API_URL = "https://generativelanguage.googleapis.com"


def emit(obj):
    sys.stdout.write(json.dumps(obj) + "\n")
    sys.stdout.flush()


async def main() -> None:
    with open(sys.argv[1], "r", encoding="utf-8") as f:
        cfg = json.load(f)

    model = cfg.get("model") or models.DEFAULT_MODEL
    # Pin the driver's model endpoint explicitly so the GOOGLE_GEMINI_BASE_URL
    # in the subprocess env (which points the CLI under test at the mock
    # server) can never redirect the driver's own Gemini traffic.
    target = models.ModelTarget(
        name=model,
        endpoint=models.GeminiAPIEndpoint(base_url=GEMINI_API_URL, api_key=cfg["apiKey"]),
    )
    config = LocalAgentConfig(
        system_instructions=cfg["system"],
        capabilities=CapabilitiesConfig(
            enabled_tools=[BuiltinTools.RUN_COMMAND, BuiltinTools.FINISH],
            enable_subagents=False,
        ),
        policies=[allow_all()],
        workspaces=[cfg["cwd"]],
        env=cfg.get("env") or {},
        model=target,
        api_key=cfg["apiKey"],
    )

    tool_calls = 0
    max_calls = int(cfg.get("maxToolCalls") or 12)
    error = None
    text = ""
    usage = None
    turns = 0
    cancelled = False

    try:
        async with Agent(config) as agent:
            response = await agent.chat(cfg["task"])
            async for call in response.tool_calls:
                name = str(getattr(call, "name", ""))
                if "run_command" in name.lower():
                    tool_calls += 1
                    args = getattr(call, "args", {}) or {}
                    command = (
                        args.get("command")
                        or args.get("command_line")
                        or json.dumps(args)
                    )
                    emit({"type": "tool_call", "name": name, "command": str(command)})
                    if tool_calls >= max_calls:
                        emit({"type": "info", "message": "max tool calls reached; cancelling"})
                        cancelled = True
                        await asyncio.wait_for(response.cancel(), timeout=5)
                        break
            if cancelled:
                error = f"max tool calls reached ({max_calls})"
            else:
                text = await response.text()
            meta = response.usage_metadata
            if meta is not None:
                usage = meta.model_dump(mode="json") if hasattr(meta, "model_dump") else str(meta)
            conversation = getattr(agent, "conversation", None)
            turns = getattr(conversation, "turn_count", 0) or 0
    except Exception as exc:  # surface everything to the runner
        error = f"{type(exc).__name__}: {exc}"

    emit(
        {
            "type": "result",
            "text": text,
            "turns": turns,
            "toolCalls": tool_calls,
            "model": model,
            "usage": usage,
            "error": error,
        }
    )


if __name__ == "__main__":
    asyncio.run(main())
