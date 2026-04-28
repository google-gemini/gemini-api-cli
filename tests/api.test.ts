import { describe, test, expect } from "bun:test";
import { resolveContext, buildInteractionRequest, apiRequest } from "../src/lib/api";
import { CLIError } from "../src/lib/errors";

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
