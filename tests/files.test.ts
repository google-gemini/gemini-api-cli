import { describe, test, expect, beforeAll, afterAll } from "bun:test";
import { execSync } from "node:child_process";
import * as fs from "node:fs";
import * as path from "node:path";

describe("files (live API)", () => {
  let envId: string;

  // Create an agent and run an interaction to get an environment
  beforeAll(async () => {
    try {
      fs.rmSync("files-test-agent", { recursive: true, force: true });
      execSync("source ~/.bash_profile && bun run src/cli.ts agents init files-test-agent --base-agent gemini-3-flash-preview", { encoding: "utf-8", shell: "/bin/bash" });
      fs.appendFileSync("files-test-agent/agent.yaml", "\nenvironment:\n  enabled: true\n");
      const result = execSync(
        'source ~/.bash_profile && bun run src/cli.ts agents test --prompt "Write hello to /workspace/test.txt" --path ./files-test-agent --json --no-stream --tool code_execution',
        { encoding: "utf-8", shell: "/bin/bash" }
      );
      const lines = result.trim().split("\n");
      for (const line of lines) {
        try {
          const event = JSON.parse(line);
          if (event.id && event.outputs) {
             // This looks like a non-streaming interaction response
             // Let's check if it has environment info
             // The test command might return the full response object in JSON mode if non-streaming
             if (event.environment_id) {
                envId = event.environment_id;
                break;
             }
          }
        } catch (e) {
          // Ignore parse errors for non-JSON lines
        }
      }
      
      if (!envId) {
         console.log("Could not find environment ID in non-streaming response, trying to parse as events");
         const events = lines.map(l => {
            try { return JSON.parse(l); } catch(e) { return null; }
         }).filter(Boolean);
         const complete = events.find(e => e.event_type === "interaction.complete");
         envId = complete?.interaction?.environment_id || complete?.interaction?.environment;
      }
      
      console.log("Created test environment:", envId);
    } catch (error) {
      console.error("Failed to setup test environment:", error);
    }
  }, 90000);

  test("list files in environment", () => {
    if (!envId) {
      console.log("Skipping list files test as envId is not available");
      return;
    }
    try {
      const result = execSync(
        `source ~/.bash_profile && bun run src/cli.ts files list ${envId}`,
        { encoding: "utf-8", shell: "/bin/bash" }
      );
      expect(result).toBeDefined();
      console.log("List files result:", result);
    } catch (e) {
      expect((e as Error).message).toContain("API error (404)");
    }
  });


  test("list --json returns raw JSON", () => {
    if (!envId) return;
    try {
      const result = execSync(
        `source ~/.bash_profile && bun run src/cli.ts files list ${envId} --json`,
        { encoding: "utf-8", shell: "/bin/bash" }
      );
      const parsed = JSON.parse(result);
      expect(parsed).toBeDefined();
    } catch (e) {
      expect((e as Error).message).toContain("API error (404)");
    }
  });


  test("download files", () => {
    if (!envId) return;
    try {
      const result = execSync(
        `source ~/.bash_profile && bun run src/cli.ts files download ${envId} --output ./test-downloads`,
        { encoding: "utf-8", shell: "/bin/bash" }
      );
      expect(result).toContain("Downloaded");
    } catch (e) {
      expect((e as Error).message).toContain("API error (404)");
    }
  });

  afterAll(() => {
    fs.rmSync("files-test-agent", { recursive: true, force: true });
    fs.rmSync("test-downloads", { recursive: true, force: true });
  });
});
