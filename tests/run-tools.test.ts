import { describe, test, expect } from "bun:test";
import { execSync } from "node:child_process";

describe("tools (live API)", () => {
  const runCli = (args: string) => {
    const cmd = `source ~/.bash_profile && bun run src/cli.ts ${args} 2>&1`;
    try {
      return execSync(cmd, { encoding: "utf-8" });
    } catch (e: any) {
      return e.stdout;
    }
  };

  test("code_execution works", () => {
    if (!process.env.GEMINI_API_KEY) {
      console.warn("Skipping live API test because GEMINI_API_KEY is not set");
      return;
    }
    const result = runCli(
      'run "Use code execution to calculate 2+2 and return only the number" --tool code_execution'
    );
    expect(result).toContain("4");
    expect(result).not.toContain("API error");
  }, 30000);

  test("google_search works", () => {
    if (!process.env.GEMINI_API_KEY) return;
    const result = runCli(
      'run "What is the current population of Tokyo? Use search." --tool google_search'
    );
    expect(result).toContain("✓ completed");
    expect(result).not.toContain("API error");
  }, 30000);

  test("multiple tools", () => {
    if (!process.env.GEMINI_API_KEY) return;
    const result = runCli(
      'run "Search for the GDP of France then calculate GDP per capita" --tool google_search --tool code_execution'
    );
    expect(result).toContain("✓ completed");
    expect(result).not.toContain("API error");
  }, 30000);

  test("invalid tool shows error", () => {
    const result = runCli('run "Hello" --tool invalid_tool');
    expect(result).toContain("Unknown tool");
    expect(result).toContain("code_execution");
  });

  test("dry-run includes tools in curl", () => {
    const result = runCli(
      'run "Hello" --tool code_execution --dry-run --api-key fake'
    );
    expect(result).toContain("code_execution");
  });

  test("dry-run includes complex tools in curl", () => {
    const result = runCli(
      'run "Hello" --tool \'mcp_server:weather:https://example.com/mcp\' --tool \'function:get_weather:{"type":"object","properties":{"location":{"type":"string"}}}\' --dry-run --api-key fake'
    );
    expect(result).toContain("mcp_server");
    expect(result).toContain("weather");
    expect(result).toContain("https://example.com/mcp");
    expect(result).toContain("function");
    expect(result).toContain("get_weather");
    expect(result).toContain("parameters");
  });

  test("dry-run includes tool-choice in curl", () => {
    const result = runCli(
      'run "Hello" --tool code_execution --tool-choice any --dry-run --api-key fake'
    );
    expect(result).toContain("tool_choice");
    expect(result).toContain("any");
  });
});
