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
import { spawn } from "node:child_process";

describe("gemini-api agents test streaming validation", () => {
  test("validates that step.delta is not printed", async () => {
    // Start a mock server
    const server = Bun.serve({
      port: 0, // random port
      fetch(req) {
        const url = new URL(req.url);
        if (url.pathname === "/interactions") {
          // Return SSE stream
          const stream = new ReadableStream({
            start(controller) {
              const encoder = new TextEncoder();
              const sendEvent = (data: object) => {
                controller.enqueue(encoder.encode(`data: ${JSON.stringify(data)}\n\n`));
              };

              sendEvent({
                event_type: "interaction.created",
                interaction: { id: "test-id", status: "in_progress" },
              });
              sendEvent({
                event_type: "step.start",
                index: 0,
                step: { type: "thought", status: "in_progress" },
              });
              sendEvent({ event_type: "step.delta", index: 0, delta: { text: "Thinking delta" } });
              sendEvent({
                event_type: "step.stop",
                index: 0,
                step: { type: "thought", status: "completed" },
              });
              sendEvent({ event_type: "content.start", index: 1, content: { type: "text" } });
              sendEvent({
                event_type: "content.delta",
                index: 1,
                delta: { text: "Content delta" },
              });
              sendEvent({ event_type: "content.stop", index: 1 });
              sendEvent({
                event_type: "interaction.completed",
                interaction: { id: "test-id", status: "completed" },
              });
              controller.enqueue(encoder.encode("data: [DONE]\n\n"));
              controller.close();
            },
          });

          return new Response(stream, {
            headers: {
              "Content-Type": "text/event-stream",
              "Cache-Control": "no-cache",
              Connection: "keep-alive",
            },
          });
        }
        return new Response("Not Found", { status: 404 });
      },
    });

    const baseUrl = `http://localhost:${server.port}`;

    const child = spawn(
      "bun",
      [
        "run",
        "src/cli.ts",
        "agents",
        "test",
        "--prompt",
        "Hello",
        "--path",
        "./tests/fixtures/agent-configs/valid",
      ],
      {
        env: { ...process.env, GEMINI_API_BASE_URL: baseUrl, GEMINI_API_KEY: "test-key" },
      },
    );

    let output = "";
    child.stdout.on("data", (data) => {
      output += data.toString();
    });

    child.stderr.on("data", (data) => {
      console.error("CLI stderr:", data.toString());
    });

    const exitCode = await new Promise((resolve) => {
      child.on("close", resolve);
    });

    console.log("CLI Output:", output);
    console.log("Exit Code:", exitCode);

    server.stop();

    // Verify that "Content delta" is present
    expect(output).toContain("Content delta");

    // Verify that "Thinking delta" is NOT present (this confirms the bug)
    expect(output).not.toContain("Thinking delta");
  }, 30000);
});
