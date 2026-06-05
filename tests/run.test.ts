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
import { execSync } from "node:child_process";

describe("gemini-api run", () => {
  // Helper to run CLI
  const runCli = (args: string, input?: string) => {
    const cmd = `bun run src/cli.ts ${args} 2>&1`;
    const options: any = { encoding: "utf-8" };
    if (input) {
      options.input = input;
    }
    try {
      return execSync(cmd, options);
    } catch (e: any) {
      return e.stdout; // Output is already combined
    }
  };

  test("basic text interaction returns response", () => {
    if (!process.env.GEMINI_API_KEY) {
      console.warn("Skipping live API test because GEMINI_API_KEY is not set");
      return;
    }
    const result = runCli('run "Say exactly: pong"');
    expect(result).toContain("pong");
    expect(result).toContain("interaction_id:");
  }, 30000);

  test("--json outputs JSONL", () => {
    if (!process.env.GEMINI_API_KEY) return;
    const result = runCli('run "Say hi" --json');
    const lines = result.trim().split("\n");
    const events = lines
      .map((l) => {
        try {
          return JSON.parse(l);
        } catch (_e) {
          return null;
        }
      })
      .filter((e) => e !== null);

    expect(events.length).toBeGreaterThan(0);
    expect(events[0].event_type).toBeDefined();
  }, 30000);

  test("streaming returns text incrementally", () => {
    if (!process.env.GEMINI_API_KEY) return;
    const result = runCli('run "Count to 5"');
    expect(result).toContain("1");
    expect(result).toContain("5");
    expect(result).toContain("✓ completed");
  }, 30000);

  test("stdin input works", () => {
    if (!process.env.GEMINI_API_KEY) return;
    const fs = require("node:fs");
    if (!fs.existsSync("tmp")) fs.mkdirSync("tmp");
    fs.writeFileSync("tmp/tmp_prompt.txt", "Say exactly: stdin-works");
    const cmd = `bun run src/cli.ts run - < tmp/tmp_prompt.txt`;
    const result = execSync(cmd, { encoding: "utf-8" });
    expect(result).toContain("stdin-works");
    fs.unlinkSync("tmp/tmp_prompt.txt");
  }, 30000);

  test("multi-turn with previous-interaction-id", () => {
    if (!process.env.GEMINI_API_KEY) return;
    // First turn
    const r1 = runCli('run "Remember the word: banana" --json');
    const lines = r1.trim().split("\n");
    let intId = "";
    for (const line of lines) {
      try {
        const data = JSON.parse(line);
        if (data.interaction?.id) {
          intId = data.interaction.id;
          break;
        }
      } catch (_e) {
        // Ignore
      }
    }

    expect(intId).toBeTruthy();

    // Second turn
    const r2 = runCli(
      `run "What word did I ask you to remember?" --previous-interaction-id ${intId}`,
    );
    expect(r2.toLowerCase()).toContain("banana");
  }, 60000);

  test("missing prompt shows error", () => {
    const result = runCli("run");
    expect(result).toContain("Missing prompt");
  });
});
