import { describe, test, expect, afterAll, beforeAll } from "bun:test";
import { execSync } from "node:child_process";
import * as fs from "node:fs";
import { join } from "node:path";

describe("interaction logging", () => {
  const logDir = join(process.cwd(), ".gemini", "logs");

  beforeAll(() => {
    fs.rmSync(logDir, { recursive: true, force: true });
  });

  test("run creates log file", () => {
    const result = execSync(
      'source ~/.bash_profile && bun run src/cli.ts run "Say hello" --no-stream 2>&1',
      { encoding: "utf-8", shell: "/bin/bash" }
    );
    
    const match = result.match(/interaction_id: ([^\s]+)/);
    const intId = match ? match[1] : null;
    expect(intId).toBeTruthy();

    const logFile = join(logDir, `${intId}.jsonl`);
    expect(fs.existsSync(logFile)).toBe(true);

    const lines = fs.readFileSync(logFile, "utf-8").trim().split("\n");
    expect(lines.length).toBe(2);

    const request = JSON.parse(lines[0]);
    expect(request.type).toBe("request");
    expect(request.data.input).toBeDefined();

    const response = JSON.parse(lines[1]);
    expect(response.type).toBe("response");
    expect(response.data.outputs).toBeDefined();
    expect(response.data.usage).toBeDefined();
  }, 60000);

  test("dry-run does NOT create log", () => {
    const before = fs.existsSync(logDir) ? fs.readdirSync(logDir).length : 0;
    execSync(
      'source ~/.bash_profile && bun run src/cli.ts run "Hello" --dry-run --api-key fake',
      { encoding: "utf-8", shell: "/bin/bash" }
    );
    const after = fs.existsSync(logDir) ? fs.readdirSync(logDir).length : 0;
    expect(after).toBe(before);
  });

  test("log does not include binary data", () => {
    fs.mkdirSync("tmp", { recursive: true });
    
    const result = execSync(
      'source ~/.bash_profile && bun run src/cli.ts run "Hello world" --model gemini-3.1-flash-tts-preview --voice Kore --output ./tmp/test.wav --no-stream 2>&1',
      { encoding: "utf-8", shell: "/bin/bash" }
    );
    
    const match = result.match(/interaction_id: ([^\s]+)/);
    const intId = match ? match[1] : null;
    expect(intId).toBeTruthy();
    
    const logFile = join(logDir, `${intId}.jsonl`);
    expect(fs.existsSync(logFile)).toBe(true);
    
    const lastLog = fs.readFileSync(logFile, "utf-8");
    
    // Should not contain base64 audio data
    expect(lastLog.length).toBeLessThan(10000);
  }, 60000);

  afterAll(() => {
    fs.rmSync("tmp", { recursive: true, force: true });
  });
});
