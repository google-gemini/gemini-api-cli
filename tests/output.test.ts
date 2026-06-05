// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

import { describe, expect, test } from "bun:test";
import { HumanStreamRenderer, printCompletionSummary } from "../src/lib/output";
import type { StreamResult } from "../src/lib/stream";

describe("HumanStreamRenderer Normal Mode (Concise)", () => {
  test("renders thought step as [thought] line", () => {
    let output = "";
    const mockStdout = {
      write(data: string) {
        output += data;
        return true;
      },
    } as typeof process.stdout;

    const renderer = new HumanStreamRenderer(mockStdout, false);

    renderer.handleStepStart({
      type: "step.start",
      data: { index: 0, step: { type: "thought" } },
      raw: "",
    });
    expect(output).toBe("[thought]\n");
  });

  test("buffers tool call and outputs combined single line on result", () => {
    let output = "";
    const mockStdout = {
      write(data: string) {
        output += data;
        return true;
      },
    } as typeof process.stdout;

    const renderer = new HumanStreamRenderer(mockStdout, false);

    // 1. Function Call step starts, deltas arrive, and stops
    renderer.handleStepStart({
      type: "step.start",
      data: { index: 1, step: { type: "function_call", name: "write_file" } },
      raw: "",
    });
    renderer.handleStepDelta({
      type: "step.delta",
      data: {
        index: 1,
        delta: { name: "write_file", arguments: '{"path":"/hello.py","content":"print(1)"}' },
      },
      raw: "",
    });
    renderer.handleStepStop({
      type: "step.stop",
      data: { index: 1 },
      raw: "",
    });

    // Output should still be empty because tool call is buffered
    expect(output).toBe("");

    // 2. Function Result step starts, deltas arrive, and stops
    renderer.handleStepStart({
      type: "step.start",
      data: { index: 2, step: { type: "function_result", name: "write_file" } },
      raw: "",
    });
    renderer.handleStepDelta({
      type: "step.delta",
      data: { index: 2, delta: { result: '{"success":true}' } },
      raw: "",
    });
    renderer.handleStepStop({
      type: "step.stop",
      data: { index: 2 },
      raw: "",
    });

    expect(output).toBe('[tool] write_file(path="/hello.py") -> {"success":true}\n');
  });

  test("buffers code execution and outputs combined single line on result", () => {
    let output = "";
    const mockStdout = {
      write(data: string) {
        output += data;
        return true;
      },
    } as typeof process.stdout;

    const renderer = new HumanStreamRenderer(mockStdout, false);

    // 1. Code execution call starts and stops
    renderer.handleStepStart({
      type: "step.start",
      data: { index: 1, step: { type: "code_execution_call" } },
      raw: "",
    });
    renderer.handleStepDelta({
      type: "step.delta",
      data: { index: 1, delta: { arguments: { code: "print(2 + 2)\n" } } },
      raw: "",
    });
    renderer.handleStepStop({
      type: "step.stop",
      data: { index: 1 },
      raw: "",
    });

    expect(output).toBe("");

    // 2. Code execution result starts and stops
    renderer.handleStepStart({
      type: "step.start",
      data: { index: 2, step: { type: "code_execution_result" } },
      raw: "",
    });
    renderer.handleStepDelta({
      type: "step.delta",
      data: { index: 2, delta: { result: "4\n", is_error: false } },
      raw: "",
    });
    renderer.handleStepStop({
      type: "step.stop",
      data: { index: 2 },
      raw: "",
    });

    expect(output).toBe('[code] print(2 + 2) -> "4"\n');
  });

  test("renders model text output directly without padding", () => {
    let output = "";
    const mockStdout = {
      write(data: string) {
        output += data;
        return true;
      },
    } as typeof process.stdout;

    const renderer = new HumanStreamRenderer(mockStdout, false);

    renderer.handleStepStart({
      type: "step.start",
      data: { index: 1, step: { type: "model_output" } },
      raw: "",
    });
    renderer.handleStepDelta({
      type: "step.delta",
      data: { index: 1, delta: { text: "Hello\nworld" } },
      raw: "",
    });
    renderer.handleStepStop({
      type: "step.stop",
      data: { index: 1 },
      raw: "",
    });

    expect(output).toBe("[text]\nHello\nworld");
  });

  test("does not render [text] header if there is no text output (e.g. media-only output)", () => {
    let output = "";
    const mockStdout = {
      write(data: string) {
        output += data;
        return true;
      },
    } as typeof process.stdout;

    const renderer = new HumanStreamRenderer(mockStdout, false);

    renderer.handleStepStart({
      type: "step.start",
      data: { index: 1, step: { type: "model_output" } },
      raw: "",
    });
    renderer.handleStepDelta({
      type: "step.delta",
      data: { index: 1, delta: { data: "base64bytes...", mime_type: "image/png" } },
      raw: "",
    });
    renderer.handleStepStop({
      type: "step.stop",
      data: { index: 1 },
      raw: "",
    });

    expect(output).toBe("");
  });

  test("uses step type from block if step type was unknown at start", () => {
    let output = "";
    const mockStdout = {
      write(data: string) {
        output += data;
        return true;
      },
    } as typeof process.stdout;

    const renderer = new HumanStreamRenderer(mockStdout, false);

    // 1. Code execution call starts with unknown type, but resolved in delta
    renderer.handleStepStart({
      type: "step.start",
      data: { index: 1 },
      raw: "",
    });
    renderer.handleStepDelta(
      {
        type: "step.delta",
        data: { index: 1, delta: { arguments: { code: "print(2 + 2)\n" } } },
        raw: "",
      },
      {
        type: "code_execution_call",
        arguments: { code: "print(2 + 2)\n" },
        id: "call_1",
      }
    );
    renderer.handleStepStop({
      type: "step.stop",
      data: { index: 1 },
      raw: "",
    });

    expect(output).toBe("");

    // 2. Code execution result starts and stops
    renderer.handleStepStart({
      type: "step.start",
      data: { index: 2, step: { type: "code_execution_result" } },
      raw: "",
    });
    renderer.handleStepDelta({
      type: "step.delta",
      data: { index: 2, delta: { result: "4\n", is_error: false } },
      raw: "",
    });
    renderer.handleStepStop({
      type: "step.stop",
      data: { index: 2 },
      raw: "",
    });

    expect(output).toBe('[code] print(2 + 2) -> "4"\n');
  });
});

describe("HumanStreamRenderer Verbose Mode", () => {
  test("prints completed step as a single JSON line", () => {
    let output = "";
    const mockStdout = {
      write(data: string) {
        output += data;
        return true;
      },
    } as typeof process.stdout;

    const renderer = new HumanStreamRenderer(mockStdout, true);

    renderer.handleStepStart({
      type: "step.start",
      data: { index: 0, step: { type: "thought" } },
      raw: "",
    });
    renderer.handleStepDelta({
      type: "step.delta",
      data: { index: 0, delta: { signature: "EvQBC..." } },
      raw: "",
    });
    renderer.handleStepStop({
      type: "step.stop",
      data: { index: 0 },
      raw: "",
    });

    const parsed = JSON.parse(output.trim());
    expect(parsed.index).toBe(0);
    expect(parsed.type).toBe("thought");
    expect(parsed.status).toBe("completed");
    expect(parsed.thought.signature).toBe("EvQBC...");
  });

  test("prints verbose completion summary as interaction JSON", () => {
    let consoleOutput = "";
    const originalLog = console.log;
    console.log = (msg) => {
      consoleOutput += msg;
    };

    try {
      const mockResult: StreamResult = {
        interactionId: "v1_test123",
        status: "completed",
        environmentId: "env_abc",
        outputs: [],
        steps: [],
        usage: {
          inputTokens: 100,
          outputTokens: 50,
          thoughtTokens: 25,
          cachedTokens: 10,
        },
      };

      printCompletionSummary(mockResult, 5.5, true);

      const parsed = JSON.parse(consoleOutput.trim());
      expect(parsed.interaction).toBeDefined();
      expect(parsed.interaction.id).toBe("v1_test123");
      expect(parsed.interaction.environment_id).toBe("env_abc");
      expect(parsed.interaction.status).toBe("completed");
      expect(parsed.interaction.usage.total_tokens).toBe(150);
      expect(parsed.interaction.usage.total_input_tokens).toBe(100);
      expect(parsed.interaction.usage.total_output_tokens).toBe(50);
      expect(parsed.interaction.usage.total_thought_tokens).toBe(25);
      expect(parsed.interaction.usage.total_cached_tokens).toBe(10);
    } finally {
      console.log = originalLog;
    }
  });
});
