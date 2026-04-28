import { describe, test, expect } from "bun:test";
import { resolveContext, apiStreamRequest } from "../src/lib/api";
import { processStream, StreamEvent } from "../src/lib/stream";

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
    expect(events[0].type).toBe("interaction.start");
    expect(events.some(e => e.type === "content.delta")).toBe(true);
    
    // Check reassembled result
    expect(result.status).toBe("completed");
    expect(result.outputs.length).toBeGreaterThan(0);
    const textBlock = result.outputs.find(o => o.type === "text");
    expect(textBlock).toBeDefined();
    expect((textBlock as any).text).toContain("streaming-works");
  });

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

    const codeCall = result.outputs.find(o => o.type === "code_execution_call");
    const codeResult = result.outputs.find(o => o.type === "code_execution_result");
    expect(codeCall).toBeDefined();
    expect(codeResult).toBeDefined();
  }, 20000);

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
    expect(result.outputs.length).toBeGreaterThan(0);
  }, 30000);
});
