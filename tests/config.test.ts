import { test, describe, expect } from "bun:test";
import { AgentConfigSchema } from "../src/lib/schemas";
import { loadAgent } from "../src/lib/config";

describe("AgentConfigSchema", () => {
  test("valid minimal config", () => {
    const result = AgentConfigSchema.safeParse({
      id: "my-agent",
      base_agent: "waverunner",
    });
    expect(result.success).toBe(true);
  });

  test("valid full config", () => {
    const result = AgentConfigSchema.safeParse({
      id: "my-agent",
      base_agent: "waverunner",
      description: "Test agent",
      system_instruction: "You are helpful",
      tools: [
        { type: "code_execution" },
        { type: "google_search" },
      ],
      environment: { enabled: true },
    });
    expect(result.success).toBe(true);
  });

  test("missing id fails", () => {
    const result = AgentConfigSchema.safeParse({
      base_agent: "waverunner",
    });
    expect(result.success).toBe(false);
  });

  test("invalid tool type fails", () => {
    const result = AgentConfigSchema.safeParse({
      id: "my-agent",
      tools: [{ type: "invalid_tool" }],
    });
    expect(result.success).toBe(false);
  });

  test("empty object fails", () => {
    const result = AgentConfigSchema.safeParse({});
    expect(result.success).toBe(false);
  });

  test("unknown fields are rejected", () => {
    const result = AgentConfigSchema.safeParse({
      id: "my-agent",
      unknown_field: "value",
    });
    expect(result.success).toBe(false);
  });
});

describe("loadAgent", () => {
  test("loads valid agent.yaml from fixture", async () => {
    const agent = await loadAgent("./tests/fixtures/agent-configs/valid");
    expect(agent.config.id).toBe("test-agent");
  });

  test("throws ConfigError for missing file", async () => {
    expect(loadAgent("./nonexistent")).rejects.toThrow();
  });

  test("throws ConfigError for invalid yaml", async () => {
    expect(loadAgent("./tests/fixtures/agent-configs/invalid")).rejects.toThrow();
  });

  test("throws ConfigError for malformed yaml", async () => {
    expect(loadAgent("./tests/fixtures/agent-configs/malformed")).rejects.toThrow();
  });

  test("throws ConfigError for empty yaml", async () => {
    expect(loadAgent("./tests/fixtures/agent-configs/empty")).rejects.toThrow();
  });
});
