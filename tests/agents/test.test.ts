import { describe, test, expect } from "bun:test";
import { execSync } from "child_process";

describe("gemini-api agents test", () => {
  const runCli = (args: string) => {
    const cmd = `bun run src/cli.ts ${args} 2>&1`;
    try {
      return execSync(cmd, { encoding: "utf-8" });
    } catch (e: any) {
      return e.stdout;
    }
  };

  test("test runs interaction with local config fixture", () => {
    if (!process.env.GEMINI_API_KEY) {
        console.warn("Skipping live API test because GEMINI_API_KEY is not set");
        return;
    }
    const result = runCli(
      `agents test --prompt "Say exactly: agent-test-pass" --path ./tests/fixtures/agent-configs/valid --no-stream`
    );
    expect(result).toContain("agent-test-pass");
  });
});
