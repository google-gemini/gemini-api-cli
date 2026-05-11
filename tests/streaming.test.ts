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
import { apiStreamRequest, resolveContext } from "../src/lib/api";
import { processStream, type StreamEvent } from "../src/lib/stream";

describe("streaming (live API)", () => {
  test("text streaming produces complete result", async () => {
    if (!process.env.GEMINI_API_KEY) {
      console.warn("Skipping live API test because GEMINI_API_KEY is not set");
      return;
    }
    const ctx = resolveContext({});
    const response = await apiStreamRequest(ctx, "/interactions", {
      model: "gemini-3-flash-preview",
      input: "Say exactly: streaming-works",
      stream: true,
    });

    const events: StreamEvent[] = [];
    const result = await processStream(response, {
      onEvent: (e) => events.push(e),
      onComplete: () => {},
    });

    // Check events arrived
    expect(events.length).toBeGreaterThan(0);
    expect(events[0].type).toBe("interaction.created");

    // Check reassembled result
    expect(result.status).toBe("completed");
    expect(result.outputs.length).toBeGreaterThan(0);
    const textBlock = result.outputs.find((o) => o.type === "text");
    expect(textBlock).toBeDefined();
    expect((textBlock as any).text).toContain("streaming-works");
  }, 30000);

  test("code execution produces call + result blocks", async () => {
    if (!process.env.GEMINI_API_KEY) {
      console.warn("Skipping live API test because GEMINI_API_KEY is not set");
      return;
    }
    const ctx = resolveContext({});
    const response = await apiStreamRequest(ctx, "/interactions", {
      model: "gemini-3-flash-preview",
      input: "Use code execution to calculate 2+2",
      tools: [{ type: "code_execution" }],
      stream: true,
    });

    const result = await processStream(response, {
      onEvent: () => {},
      onComplete: () => {},
    });

    const codeCall = result.steps.find((s) => s.type === "code_execution_call");
    const codeResult = result.steps.find((s) => s.type === "code_execution_result");
    expect(codeCall).toBeDefined();
    expect(codeResult).toBeDefined();
  }, 60000);

  test("google search produces search call/result", async () => {
    if (!process.env.GEMINI_API_KEY) {
      console.warn("Skipping live API test because GEMINI_API_KEY is not set");
      return;
    }
    const ctx = resolveContext({});
    const response = await apiStreamRequest(ctx, "/interactions", {
      model: "gemini-3-flash-preview",
      input: "What happened in the news today?",
      tools: [{ type: "google_search" }],
      stream: true,
    });

    const result = await processStream(response, {
      onEvent: () => {},
      onComplete: () => {},
    });

    expect(result.status).toBe("completed");
    expect(result.steps.length).toBeGreaterThan(0);
  }, 90000);
});

// Mock-data unit tests for step events (no live API required)
describe("step event parsing", () => {
  function mockSSEResponse(events: object[]): Response {
    const lines = `${events.map((e) => `data: ${JSON.stringify(e)}`).join("\n")}\ndata: [DONE]\n`;
    const encoder = new TextEncoder();
    const stream = new ReadableStream({
      start(controller) {
        controller.enqueue(encoder.encode(lines));
        controller.close();
      },
    });
    return new Response(stream);
  }

  test("step.start creates step entry", async () => {
    const response = mockSSEResponse([
      { event_type: "interaction.start", interaction: { id: "test-123", status: "in_progress" } },
      { event_type: "step.start", index: 0, step: { type: "thinking", status: "in_progress" } },
      { event_type: "step.stop", index: 0, step: { type: "thinking", status: "completed" } },
      { event_type: "interaction.complete", interaction: { id: "test-123", status: "completed" } },
    ]);

    const events: StreamEvent[] = [];
    const result = await processStream(response, {
      onEvent: (e) => events.push(e),
      onComplete: () => {},
    });

    expect(result.steps.length).toBe(1);
    expect(result.steps[0].type).toBe("thinking");
    expect(result.steps[0].status).toBe("completed");
    expect(events.some((e) => e.type === "step.start")).toBe(true);
    expect(events.some((e) => e.type === "step.stop")).toBe(true);
  });

  test("step.delta accumulates text", async () => {
    const response = mockSSEResponse([
      { event_type: "interaction.start", interaction: { id: "test-456", status: "in_progress" } },
      { event_type: "step.start", index: 0, step: { type: "tool_use" } },
      { event_type: "step.delta", index: 0, delta: { text: "Searching " } },
      { event_type: "step.delta", index: 0, delta: { text: "the web..." } },
      { event_type: "step.stop", index: 0, step: { type: "tool_use", status: "completed" } },
      { event_type: "interaction.complete", interaction: { id: "test-456", status: "completed" } },
    ]);

    const result = await processStream(response, {
      onEvent: () => {},
      onComplete: () => {},
    });

    expect(result.steps[0].text).toBe("Searching the web...");
    expect(result.steps[0].type).toBe("tool_use");
  });

  test("multiple steps are tracked independently", async () => {
    const response = mockSSEResponse([
      { event_type: "interaction.start", interaction: { id: "test-789", status: "in_progress" } },
      { event_type: "step.start", index: 0, step: { type: "thinking" } },
      { event_type: "step.stop", index: 0, step: { type: "thinking", status: "completed" } },
      { event_type: "step.start", index: 1, step: { type: "tool_use" } },
      { event_type: "step.stop", index: 1, step: { type: "tool_use", status: "completed" } },
      { event_type: "interaction.complete", interaction: { id: "test-789", status: "completed" } },
    ]);

    const result = await processStream(response, {
      onEvent: () => {},
      onComplete: () => {},
    });

    expect(result.steps.length).toBe(2);
    expect(result.steps[0].type).toBe("thinking");
    expect(result.steps[1].type).toBe("tool_use");
  });
});
