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
import { apiRequest, buildInteractionRequest, resolveContext } from "../src/lib/api";
import { printCurl } from "../src/lib/output";

describe("resolveContext", () => {
  test("reads from GEMINI_API_KEY", () => {
    const oldKey = process.env.GEMINI_API_KEY;
    const oldAutopushKey = process.env.GEMINI_AUTOPUSH_API_KEY;
    process.env.GEMINI_API_KEY = "test-key";
    delete process.env.GEMINI_AUTOPUSH_API_KEY;
    const ctx = resolveContext({});
    expect(ctx.apiKey).toBe("test-key");
    process.env.GEMINI_API_KEY = oldKey; // Restore
    process.env.GEMINI_AUTOPUSH_API_KEY = oldAutopushKey; // Restore
  });

  test("flag overrides env var", () => {
    const oldKey = process.env.GEMINI_API_KEY;
    process.env.GEMINI_API_KEY = "env-key";
    const ctx = resolveContext({ apiKey: "flag-key" });
    expect(ctx.apiKey).toBe("flag-key");
    process.env.GEMINI_API_KEY = oldKey; // Restore
  });

  test("throws when no key", () => {
    const oldKey = process.env.GEMINI_API_KEY;
    const oldAutopushKey = process.env.GEMINI_AUTOPUSH_API_KEY;
    delete process.env.GEMINI_API_KEY;
    delete process.env.GEMINI_AUTOPUSH_API_KEY;
    expect(() => resolveContext({})).toThrow("No API key found");
    process.env.GEMINI_API_KEY = oldKey; // Restore
    process.env.GEMINI_AUTOPUSH_API_KEY = oldAutopushKey; // Restore
  });
});

describe("buildInteractionRequest", () => {
  test("model interaction", () => {
    const body = buildInteractionRequest({
      model: "gemini-3-flash-preview",
      input: "Hello",
      stream: true,
    });
    expect(body).toHaveProperty("model", "gemini-3-flash-preview");
    expect(body).toHaveProperty("input", "Hello");
    expect(body).toHaveProperty("stream", true);
  });

  test("agent interaction", () => {
    const body = buildInteractionRequest({
      agent: "my-agent",
      input: "Hello",
    });
    expect(body).toHaveProperty("agent", "my-agent");
    expect(body).not.toHaveProperty("model");
  });
  test("sources produce environment with type:'remote' instead of config wrapper", () => {
    const body = buildInteractionRequest({
      input: "Hello",
      sources: [{ type: "gcs", source: "gs://bucket/path", target: "/target" }],
    }) as any;
    expect(body.environment).toEqual({
      type: "remote",
      sources: [{ type: "gcs", source: "gs://bucket/path", target: "/target" }],
    });
    expect(body.environment.config).toBeUndefined();
  });

  test("github type source is normalized to repository", () => {
    const body = buildInteractionRequest({
      input: "Hello",
      sources: [{ type: "github", source: "https://github.com/foo/bar", target: "/app" }],
    }) as any;
    expect(body.environment).toEqual({
      type: "remote",
      sources: [{ type: "repository", source: "https://github.com/foo/bar", target: "/app" }],
    });
  });

  test("repository type source is parsed correctly", () => {
    const body = buildInteractionRequest({
      input: "Hello",
      sources: [{ type: "repository", source: "https://github.com/foo/bar", target: "/app" }],
    }) as any;
    expect(body.environment).toEqual({
      type: "remote",
      sources: [{ type: "repository", source: "https://github.com/foo/bar", target: "/app" }],
    });
  });

  test("direct environment mapping for strings", () => {
    const bodyRemote = buildInteractionRequest({
      input: "Hello",
      environment: "remote",
    }) as any;
    expect(bodyRemote.environment).toBe("remote");

    const bodyEnv = buildInteractionRequest({
      input: "Hello",
      environment: "env_xyz123",
    }) as any;
    expect(bodyEnv.environment).toBe("env_xyz123");
  });

  test("network configuration is preserved and sources normalized in object environments", () => {
    const body = buildInteractionRequest({
      input: "Hello",
      environment: {
        type: "remote",
        sources: [{ type: "github", source: "https://github.com/foo/bar", target: "/app" }],
        network: "disabled",
      },
    }) as any;
    expect(body.environment).toEqual({
      type: "remote",
      sources: [{ type: "repository", source: "https://github.com/foo/bar", target: "/app" }],
      network: "disabled",
    });
  });

  test("outbound network config with allowlist", () => {
    const body = buildInteractionRequest({
      input: "Hello",
      environment: {
        type: "remote",
        network: {
          allowlist: [{ domain: "api.github.com" }],
        },
      },
    }) as any;
    expect(body.environment.network).toEqual({
      allowlist: [{ domain: "api.github.com" }],
    });
  });

  test("outbound network config with allowlist and transform rules", () => {
    const body = buildInteractionRequest({
      input: "Hello",
      environment: {
        type: "remote",
        network: {
          allowlist: [
            {
              domain: "api.github.com",
              transform: {
                "X-Forwarded-For": "1.2.3.4",
              },
            },
          ],
        },
      },
    }) as any;
    expect(body.environment.network).toEqual({
      allowlist: [
        {
          domain: "api.github.com",
          transform: {
            "X-Forwarded-For": "1.2.3.4",
          },
        },
      ],
    });
  });

  test("throws error when custom source target is '/'", () => {
    expect(() => {
      buildInteractionRequest({
        input: "Hello",
        sources: [{ type: "gcs", source: "gs://bucket/path", target: "/" }],
      });
    }).toThrow('Invalid source target: "/". Custom sources cannot be mounted at root.');
  });
});

describe("Api-Revision header", () => {
  test("printCurl includes Api-Revision header for /interactions URL", () => {
    const output: string[] = [];
    const origLog = console.log;
    console.log = (...args: any[]) => output.push(args.join(" "));
    printCurl("POST", "https://example.com/v1beta/interactions", "test-key", { input: "Hello" });
    console.log = origLog;
    const curl = output.join("\n");
    expect(curl).toContain("Api-Revision: 2026-05-20");
  });

  test("printCurl does NOT include Api-Revision for non-interactions URL", () => {
    const output: string[] = [];
    const origLog = console.log;
    console.log = (...args: any[]) => output.push(args.join(" "));
    printCurl("GET", "https://example.com/v1beta/agents", "test-key");
    console.log = origLog;
    const curl = output.join("\n");
    expect(curl).not.toContain("Api-Revision");
  });
});

// Integration test (requires GEMINI_API_KEY)
describe("apiRequest (live API)", () => {
  test("GET /agents returns list", async () => {
    // Only run if GEMINI_API_KEY is set
    if (!process.env.GEMINI_API_KEY) {
      console.warn("Skipping live API test because GEMINI_API_KEY is not set");
      return;
    }
    const ctx = resolveContext({});
    try {
      const result = await apiRequest<{ agents?: any[] }>(ctx, "GET", "/agents");
      expect(result).toHaveProperty("agents");
    } catch (e) {
      // If it fails with 404 or something else, it might be because the endpoint is not available in sandbox yet
      // or we need to create an agent first.
      // But the task implies it should work.
      console.error("apiRequest failed:", e);
      throw e;
    }
  });
});
