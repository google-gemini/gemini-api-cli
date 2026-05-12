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

import { test, describe, expect } from "bun:test";
import { AgentConfigSchema } from "../src/lib/schemas";
import { loadAgent } from "../src/lib/config";

describe("AgentConfigSchema", () => {
  test("valid minimal config", () => {
    const result = AgentConfigSchema.safeParse({
      id: "my-agent",
      base_agent: "antigravity-preview-05-2026",
    });
    expect(result.success).toBe(true);
  });

  test("valid full config", () => {
    const result = AgentConfigSchema.safeParse({
      id: "my-agent",
      base_agent: "antigravity-preview-05-2026",
      description: "Test agent",
      instructions: "You are helpful",
      tools: [
        { type: "code_execution" },
        { type: "google_search" },
      ],
    });
    expect(result.success).toBe(true);
  });

  test("valid base_environment as string", () => {
    const result = AgentConfigSchema.safeParse({
      id: "my-agent",
      base_environment: "env-123",
    });
    expect(result.success).toBe(true);
  });

  test("valid base_environment with config sources", () => {
    const result = AgentConfigSchema.safeParse({
      id: "my-agent",
      base_environment: {
        config: {
          sources: [
            { type: "gcs", source: "gs://bucket/path", target: "/target" },
          ],
        },
      },
    });
    expect(result.success).toBe(true);
  });

  test("missing id fails", () => {
    const result = AgentConfigSchema.safeParse({
      base_agent: "antigravity-preview-05-2026",
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
