import type { CapturedRequest } from "./mock-server.js";

export interface Scenario {
  id: string;
  /** Natural-language task given to the agent; harness-agnostic. */
  task: string;
  /** True when a captured request is the API call the task demanded. */
  expect: (req: CapturedRequest) => boolean;
}

const stripVersion = (p: string) => p.replace(/^\/v\d+(alpha|beta)?/, "");

export const scenarios: Scenario[] = [
  {
    id: "create-agent",
    // The base agent is mentioned because the reference CLI hard-requires
    // --base-agent; without it the scenario is unwinnable there.
    task: 'Create a managed agent with id "demo-agent", extending the base agent "deep-research", with system instruction "You are a helpful test agent." Then tell me the id the API returned.',
    expect: (r) => {
      if (r.method !== "POST" || stripVersion(r.path) !== "/agents")
        return false;
      const b = (r.json ?? {}) as Record<string, unknown>;
      return JSON.stringify(b).includes("demo-agent");
    },
  },
  {
    id: "list-agents",
    task: "List the managed agents on this project and tell me how many there are and their ids.",
    expect: (r) => r.method === "GET" && stripVersion(r.path) === "/agents",
  },
  {
    id: "get-agent",
    task: 'Fetch the managed agent with id "demo-agent" and tell me its system instruction.',
    expect: (r) =>
      r.method === "GET" && stripVersion(r.path) === "/agents/demo-agent",
  },
  {
    id: "delete-agent",
    task: 'Delete the managed agent with id "demo-agent" and confirm the deletion.',
    expect: (r) =>
      r.method === "DELETE" && stripVersion(r.path) === "/agents/demo-agent",
  },
  {
    id: "run-agent-background",
    task: 'Start a background interaction with the managed agent "demo-agent" using the input "say hello". Tell me the interaction id the API returned.',
    expect: (r) => {
      if (r.method !== "POST" || stripVersion(r.path) !== "/interactions")
        return false;
      const s = JSON.stringify(r.json ?? {});
      return s.includes("demo-agent");
    },
  },
  {
    id: "interaction-status",
    task: 'Check the status of interaction "int-123" and tell me whether it has completed and what its output text is.',
    expect: (r) =>
      r.method === "GET" && stripVersion(r.path) === "/interactions/int-123",
  },
  {
    id: "cancel-interaction",
    task: 'Cancel the in-progress interaction "int-123" and confirm its status afterwards.',
    expect: (r) =>
      r.method === "POST" &&
      stripVersion(r.path) === "/interactions/int-123/cancel",
  },
  // --- Tier-1 intent surface (modalities via the Interactions API) ---
  {
    id: "generate-default-model",
    task: 'Generate a one-sentence answer to "What is concurrency?" using the CLI\'s default text model, and tell me the model that was used.',
    expect: (r) => {
      if (r.method !== "POST" || stripVersion(r.path) !== "/interactions")
        return false;
      const b = (r.json ?? {}) as Record<string, unknown>;
      return b.model === "gemini-3.6-flash" && typeof b.input === "string";
    },
  },
  {
    id: "generate-image",
    task: 'Generate an image of "a red circle on a white background" and tell me what content type the API returned.',
    expect: (r) => {
      if (r.method !== "POST" || stripVersion(r.path) !== "/interactions")
        return false;
      const s = JSON.stringify(r.json ?? {});
      return s.includes("image");
    },
  },
  {
    id: "generate-music",
    task: 'Generate a short piece of music described as "an upbeat jingle" and tell me what content type the API returned.',
    expect: (r) => {
      if (r.method !== "POST" || stripVersion(r.path) !== "/interactions")
        return false;
      const s = JSON.stringify(r.json ?? {});
      return s.includes("audio") || s.includes("lyria");
    },
  },
  {
    id: "generate-video-background",
    task: 'Start generating a video of "a bouncing ball" as a background job and tell me the interaction id the API returned.',
    expect: (r) => {
      if (r.method !== "POST" || stripVersion(r.path) !== "/interactions")
        return false;
      const b = (r.json ?? {}) as Record<string, unknown>;
      const s = JSON.stringify(b);
      return b.background === true && s.includes("video");
    },
  },
  // --- Classic GenAI surface (merged from the Discovery document) ---
  {
    id: "list-models-live",
    task: "List the models available through the API and tell me how many there are.",
    expect: (r) => r.method === "GET" && stripVersion(r.path) === "/models",
  },
];
