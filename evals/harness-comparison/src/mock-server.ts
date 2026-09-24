import http from "node:http";
import type { AddressInfo } from "node:net";

export interface CapturedRequest {
  method: string;
  path: string;
  query: string;
  headers: Record<string, string | string[] | undefined>;
  body: string;
  json?: unknown;
  at: number;
}

/**
 * Minimal mock of the Gemini Interactions API surface both CLIs target.
 * Captures every request and serves canned JSON so either CLI can complete
 * a happy-path command. Route matching strips the /{api_version} prefix.
 */
export class MockGeminiServer {
  readonly requests: CapturedRequest[] = [];
  private server: http.Server;
  baseUrl = "";
  // Minimal statefulness so agents that verify their work see consistent
  // results (a stateless mock made deletes look like failures).
  private deletedAgents = new Set<string>();
  private interactionStatus = new Map<string, string>();

  constructor() {
    this.server = http.createServer((req, res) => {
      let body = "";
      req.on("data", (c) => (body += c));
      req.on("end", () => {
        const url = new URL(req.url ?? "/", "http://localhost");
        const captured: CapturedRequest = {
          method: req.method ?? "GET",
          path: url.pathname,
          query: url.search.replace(/^\?/, ""),
          headers: req.headers,
          body,
          at: Date.now(),
        };
        try {
          captured.json = body ? JSON.parse(body) : undefined;
        } catch {
          /* non-JSON body */
        }
        this.requests.push(captured);
        const [status, payload] = this.respond(captured);
        res.writeHead(status, { "Content-Type": "application/json" });
        res.end(JSON.stringify(payload));
      });
    });
  }

  private respond(req: CapturedRequest): [number, unknown] {
    // Normalize away the api version prefix (/v1beta/... or bare /...).
    const p = req.path.replace(/^\/v\d+(alpha|beta)?/, "");
    const m = req.method;

    if (m === "POST" && p === "/agents") {
      const body = (req.json ?? {}) as Record<string, unknown>;
      return [200, { ...body, id: body.id ?? "demo-agent", createTime: "2026-08-10T12:00:00Z" }];
    }
    if (m === "GET" && p === "/agents") {
      return [
        200,
        {
          agents: [
            {
              id: "demo-agent",
              displayName: "Demo Agent",
              systemInstruction: "You are a helpful test agent.",
              createTime: "2026-08-10T12:00:00Z",
            },
          ],
        },
      ];
    }
    let match = p.match(/^\/agents\/([^/]+)$/);
    if (match) {
      const id = match[1];
      if (this.deletedAgents.has(id)) {
        return [404, { error: { code: 404, message: `agent ${id} not found` } }];
      }
      if (m === "GET")
        return [
          200,
          {
            id,
            displayName: "Demo Agent",
            systemInstruction: "You are a helpful test agent.",
            createTime: "2026-08-10T12:00:00Z",
          },
        ];
      if (m === "DELETE") {
        this.deletedAgents.add(id);
        return [200, {}];
      }
    }
    if (m === "POST" && p === "/interactions") {
      const b = (req.json ?? {}) as Record<string, unknown>;
      if (b.background) {
        this.interactionStatus.set("int-123", "in_progress");
        return [
          200,
          { id: "int-123", status: "in_progress", createTime: "2026-08-10T12:00:00Z" },
        ];
      }
      // Foreground interactions complete immediately with content matching
      // the requested response modalities (text when unspecified).
      const modalities = Array.isArray(b.response_modalities)
        ? (b.response_modalities as string[])
        : ["text"];
      const content = modalities.map((modality) =>
        modality === "text"
          ? { type: "text", text: "hello from the mock server" }
          : { type: modality, data: "bW9jaw==", mime_type: `${modality}/mock` },
      );
      return [
        200,
        {
          id: "int-456",
          object: "interaction",
          status: "completed",
          model: b.model ?? "gemini-3.6-flash",
          steps: [{ type: "model_output", content }],
          created: "2026-08-10T12:00:00Z",
          updated: "2026-08-10T12:00:00Z",
        },
      ];
    }
    // Classic GenAI surface (models)
    if (m === "GET" && p === "/models") {
      return [
        200,
        {
          models: [
            { name: "models/gemini-3.6-flash", displayName: "Gemini 3.6 Flash" },
            { name: "models/gemini-2.5-pro", displayName: "Gemini 2.5 Pro" },
            { name: "models/gemini-embedding-2", displayName: "Gemini Embedding 2" },
          ],
        },
      ];
    }
    match = p.match(/^\/models\/([^/:]+):embedContent$/);
    if (match && m === "POST") {
      return [200, { embedding: { values: [0.1, -0.2, 0.3] } }];
    }
    match = p.match(/^\/models\/([^/:]+):countTokens$/);
    if (match && m === "POST") {
      return [200, { totalTokens: 5 }];
    }
    match = p.match(/^\/interactions\/([^/]+)$/);
    if (match) {
      const id = match[1];
      if (m === "GET") {
        const status = this.interactionStatus.get(id) ?? "completed";
        return [
          200,
          {
            id,
            status,
            outputs:
              status === "completed"
                ? [
                    {
                      type: "message",
                      role: "model",
                      content: [{ type: "text", text: "hello from the mock server" }],
                    },
                  ]
                : [],
          },
        ];
      }
      if (m === "DELETE") return [200, {}];
    }
    match = p.match(/^\/interactions\/([^/]+)\/cancel$/);
    if (match && m === "POST") {
      this.interactionStatus.set(match[1], "cancelled");
      return [200, { id: match[1], status: "cancelled" }];
    }
    return [404, { error: { code: 404, message: `no mock route for ${m} ${req.path}` } }];
  }

  async start(): Promise<string> {
    await new Promise<void>((resolve) => this.server.listen(0, "127.0.0.1", resolve));
    const addr = this.server.address() as AddressInfo;
    this.baseUrl = `http://127.0.0.1:${addr.port}`;
    return this.baseUrl;
  }

  async stop(): Promise<void> {
    await new Promise<void>((resolve) => this.server.close(() => resolve()));
  }
}
