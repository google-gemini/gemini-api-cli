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
import { loadAgent } from "../src/lib/config";
import { AgentConfigSchema } from "../src/lib/schemas";

function restoreEnv(name: string, value: string | undefined) {
  if (value === undefined) {
    delete process.env[name];
  } else {
    process.env[name] = value;
  }
}

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
      tools: [{ type: "code_execution" }, { type: "google_search" }],
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

  test("valid base_environment with remote sources and network", () => {
    const result = AgentConfigSchema.safeParse({
      id: "my-agent",
      base_environment: {
        type: "remote",
        sources: [{ type: "gcs", source: "gs://bucket/path", target: "/target" }],
        network: { allowlist: [{ domain: "example.com" }] },
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

  test("valid config with examples", () => {
    const result = AgentConfigSchema.safeParse({
      id: "my-agent",
      base_agent: "antigravity-preview-05-2026",
      examples: [
        { title: "Write a poem", prompt: "Write a short poem about coding" },
        { title: "Explain AI", prompt: "Explain artificial intelligence" },
      ],
    });
    expect(result.success).toBe(true);
    if (result.success) {
      expect(result.data.examples).toHaveLength(2);
      expect(result.data.examples?.[0].title).toBe("Write a poem");
      expect(result.data.examples?.[0].prompt).toBe("Write a short poem about coding");
    }
  });

  test("examples with missing prompt fails", () => {
    const result = AgentConfigSchema.safeParse({
      id: "my-agent",
      examples: [
        { title: "Write a poem" }, // missing prompt
      ],
    });
    expect(result.success).toBe(false);
  });

  test("examples with missing title fails", () => {
    const result = AgentConfigSchema.safeParse({
      id: "my-agent",
      examples: [
        { prompt: "Write something" }, // missing title
      ],
    });
    expect(result.success).toBe(false);
  });

  test("valid base_environment with type:remote format", () => {
    const result = AgentConfigSchema.safeParse({
      id: "my-agent",
      base_environment: {
        type: "remote",
        sources: [{ type: "gcs", source: "gs://bucket/path", target: "/target" }],
      },
    });
    expect(result.success).toBe(true);
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

  test("loads agent.yaml with examples from fixture", async () => {
    const agent = await loadAgent("./tests/fixtures/agent-configs/with-examples");
    expect(agent.config.id).toBe("test-agent-examples");
    expect(agent.config.examples).toHaveLength(2);
    expect(agent.config.examples?.[0].title).toBe("Write a poem");
  });

  test("resolves agent.yaml environment variable placeholders from process env", async () => {
    const oldGithubToken = process.env.GITHUB_TOKEN;
    const oldGithubPat = process.env.GITHUB_PAT;
    const oldGeminiApiKey = process.env.GEMINI_API_KEY;
    try {
      process.env.GITHUB_TOKEN = 'process-"github"-token';
      process.env.GITHUB_PAT = "process-$&-github-pat";
      process.env.GEMINI_API_KEY = "process-gemini-api-key";

      const agent = await loadAgent("./tests/fixtures/agent-configs/with-env-vars");
      expect(agent.config.id).toBe("test-agent-with-env-vars");
      expect(agent.config.sources).toEqual([
        {
          type: "github",
          source: "https://process-$&-github-pat@github.com/my-org/private-repo",
          target: "/workspace/private-repo",
        },
      ]);
      expect(agent.config.environment).toEqual({
        type: "remote",
        network: {
          allowlist: [
            {
              domain: "api.github.com",
              transform: {
                Authorization: 'Bearer process-"github"-token',
                "X-GitHub-Token": 'process-"github"-token',
              },
            },
            {
              domain: "generativelanguage.googleapis.com",
              transform: {
                "x-goog-api-key": "process-gemini-api-key",
                Authorization: "Bearer process-gemini-api-key",
              },
            },
          ],
        },
      });
    } finally {
      restoreEnv("GITHUB_TOKEN", oldGithubToken);
      restoreEnv("GITHUB_PAT", oldGithubPat);
      restoreEnv("GEMINI_API_KEY", oldGeminiApiKey);
    }
  });

  test("resolves agent.yaml environment variable placeholders from env file first", async () => {
    const oldGithubToken = process.env.GITHUB_TOKEN;
    const oldGithubPat = process.env.GITHUB_PAT;
    const oldGeminiApiKey = process.env.GEMINI_API_KEY;
    try {
      process.env.GITHUB_TOKEN = "process-github-token";
      process.env.GITHUB_PAT = "process-github-pat";
      process.env.GEMINI_API_KEY = "process-gemini-api-key";

      const agent = await loadAgent("./tests/fixtures/agent-configs/with-env-vars", {
        envFile: "./tests/fixtures/agent-configs/with-env-vars/.env",
      });
      expect(agent.config.sources).toEqual([
        {
          type: "github",
          source: "https://env-file-github-pat@github.com/my-org/private-repo",
          target: "/workspace/private-repo",
        },
      ]);
      expect(agent.config.environment).toEqual({
        type: "remote",
        network: {
          allowlist: [
            {
              domain: "api.github.com",
              transform: {
                Authorization: "Bearer env-file-github-token",
                "X-GitHub-Token": "env-file-github-token",
              },
            },
            {
              domain: "generativelanguage.googleapis.com",
              transform: {
                "x-goog-api-key": "env-file-gemini-api-key",
                Authorization: "Bearer env-file-gemini-api-key",
              },
            },
          ],
        },
      });
    } finally {
      restoreEnv("GITHUB_TOKEN", oldGithubToken);
      restoreEnv("GITHUB_PAT", oldGithubPat);
      restoreEnv("GEMINI_API_KEY", oldGeminiApiKey);
    }
  });

  test("throws when agent.yaml references missing environment variables", async () => {
    const oldGithubToken = process.env.GITHUB_TOKEN;
    const oldGithubPat = process.env.GITHUB_PAT;
    const oldGeminiApiKey = process.env.GEMINI_API_KEY;
    try {
      delete process.env.GITHUB_TOKEN;
      delete process.env.GITHUB_PAT;
      delete process.env.GEMINI_API_KEY;

      await expect(loadAgent("./tests/fixtures/agent-configs/with-env-vars")).rejects.toThrow(
        "Missing environment variable GITHUB_PAT",
      );
    } finally {
      restoreEnv("GITHUB_TOKEN", oldGithubToken);
      restoreEnv("GITHUB_PAT", oldGithubPat);
      restoreEnv("GEMINI_API_KEY", oldGeminiApiKey);
    }
  });

  test("throws a clear error when env file is missing", async () => {
    await expect(
      loadAgent("./tests/fixtures/agent-configs/valid", {
        envFile: "./tests/fixtures/agent-configs/valid/missing.env",
      }),
    ).rejects.toThrow(
      "Environment file not found: ./tests/fixtures/agent-configs/valid/missing.env",
    );
  });
});
