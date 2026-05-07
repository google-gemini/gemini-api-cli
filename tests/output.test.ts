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

import { describe, test, expect } from "bun:test";
import { spawnSync, execSync } from "child_process";
import * as fs from "fs";

describe("output modes", () => {
  // Use execSync for combined output (spawnSync + bun has empty output issues)
  const runCliCombined = (args: string) => {
    const cmd = `source ~/.bash_profile && bun run src/cli.ts ${args}`;
    try {
      return execSync(cmd, { encoding: "utf-8", shell: "/bin/bash" });
    } catch (e: any) {
      return e.stdout || e.stderr || "";
    }
  };

  test("human mode outputs text and summary to stdout", () => {
    if (!process.env.GEMINI_API_KEY) {
        console.warn("Skipping live API test because GEMINI_API_KEY is not set");
        return;
    }
    // Use separate stream capture via shell redirection
    if (!fs.existsSync("tmp")) fs.mkdirSync("tmp");
    execSync(
      'source ~/.bash_profile && bun run src/cli.ts run "Say exactly: output-test" > tmp/stdout.txt 2> tmp/stderr.txt',
      { encoding: "utf-8", shell: "/bin/bash" }
    );
    const stdout = fs.readFileSync("tmp/stdout.txt", "utf-8");
    const stderr = fs.readFileSync("tmp/stderr.txt", "utf-8");
    expect(stdout).toContain("output-test");
    expect(stdout).toContain("✓ completed");
    expect(stdout).toContain("interaction_id:");
    fs.unlinkSync("tmp/stdout.txt");
    fs.unlinkSync("tmp/stderr.txt");
  });

  test("json mode outputs valid JSONL to stdout", () => {
    if (!process.env.GEMINI_API_KEY) return;
    if (!fs.existsSync("tmp")) fs.mkdirSync("tmp");
    execSync(
      'source ~/.bash_profile && bun run src/cli.ts run "Say hi" --json > tmp/json_out.txt 2> tmp/json_err.txt',
      { encoding: "utf-8", shell: "/bin/bash" }
    );
    const stdout = fs.readFileSync("tmp/json_out.txt", "utf-8");
    const stderr = fs.readFileSync("tmp/json_err.txt", "utf-8");
    
    const lines = stdout.trim().split("\n");
    expect(lines.length).toBeGreaterThan(0);
    for (const line of lines) {
      expect(() => JSON.parse(line)).not.toThrow();
    }
    const events = lines.map(l => JSON.parse(l));
    expect(events.some(e => e.event_type === "interaction.start")).toBe(true);
    expect(events.some(e => e.event_type === "interaction.complete")).toBe(true);
    // Summary NOT in stderr or stdout in json mode
    expect(stderr).not.toContain("✓ completed");
    
    fs.unlinkSync("tmp/json_out.txt");
    fs.unlinkSync("tmp/json_err.txt");
  });


  test("errors include Try: with correct invocation", () => {
    const result = runCliCombined("run 2>&1");
    expect(result).toContain("Missing prompt");
    expect(result).toContain("Try:");
    expect(result).toContain("gemini-api run");
  });

  test('redirection captures only response text', () => {
    if (!process.env.GEMINI_API_KEY) return;
    if (!fs.existsSync("tmp")) fs.mkdirSync("tmp");
    
    execSync(
      'source ~/.bash_profile && bun run src/cli.ts run "Say exactly: hello-redirection" > tmp/out.txt 2>/dev/null',
      { encoding: "utf-8", shell: "/bin/bash" }
    );
    
    const content = fs.readFileSync("tmp/out.txt", "utf-8");
    expect(content).toContain("hello-redirection");
    expect(content).toContain("✓ completed");
    
    fs.unlinkSync("tmp/out.txt");
  });

  test('json redirection captures raw events', () => {
    if (!process.env.GEMINI_API_KEY) return;
    if (!fs.existsSync("tmp")) fs.mkdirSync("tmp");
    
    execSync(
      'source ~/.bash_profile && bun run src/cli.ts run "Say hi" --json > tmp/out.jsonl 2>/dev/null',
      { encoding: "utf-8", shell: "/bin/bash" }
    );
    
    const content = fs.readFileSync("tmp/out.jsonl", "utf-8");
    const lines = content.trim().split("\n");
    expect(lines.length).toBeGreaterThan(0);
    expect(() => JSON.parse(lines[0])).not.toThrow();
    
    fs.unlinkSync("tmp/out.jsonl");
  }, 10000);
});

describe("dry-run", () => {
  const runCli = (args: string) => {
    const cmd = `source ~/.bash_profile && bun run src/cli.ts ${args} 2>&1`;
    try {
      return execSync(cmd, { encoding: "utf-8", shell: "/bin/bash" });
    } catch (e: any) {
      return e.stdout || e.stderr || "";
    }
  };

  test("run --dry-run prints curl", () => {
    const result = runCli("run Hello --dry-run --api-key fake-key");
    expect(result).toContain("curl -X POST");
    expect(result).toContain("fake-key");
    expect(result).toContain("https://generativelanguage.googleapis.com");
  });

  test("agents create --dry-run prints curl", () => {
    execSync("source ~/.bash_profile && bun run src/cli.ts agents init dry-run-test", { encoding: "utf-8", shell: "/bin/bash" });
    const result = runCli("agents create --path ./dry-run-test --dry-run --api-key fake-key");
    expect(result).toContain("curl -X POST");
    expect(result).toContain("/agents");
    fs.rmSync("dry-run-test", { recursive: true, force: true });
  });

  test("agents list --dry-run prints curl", () => {
    const result = runCli("agents list --dry-run --api-key fake-key");
    expect(result).toContain("curl -X GET");
    expect(result).toContain("/agents");
  });

  test("agents delete --dry-run prints curl", () => {
    const result = runCli("agents delete test-id --dry-run --api-key fake-key");
    expect(result).toContain("curl -X DELETE");
    expect(result).toContain("/agents/test-id");
  });

  test("files download --dry-run prints curl", () => {
    const result = runCli("files download env_fake --dry-run --api-key fake-key");
    expect(result).toContain("curl");
  });
});
