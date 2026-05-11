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

import { CLIError } from "./errors";

export const DEFAULT_BASE_URL = "https://generativelanguage.googleapis.com/v1beta";

export interface CLIContext {
  apiKey: string;
  baseUrl: string;
}

export interface SharedFlags {
  apiKey?: string;
  baseUrl?: string;
  json?: boolean;
  dryRun?: boolean;
  verbose?: boolean;
}

export function resolveContext(flags: SharedFlags): CLIContext {
  const apiKey = flags.apiKey ?? process.env.GEMINI_API_KEY;
  if (!apiKey) {
    throw new CLIError(
      'No API key found.\n\n  Try:\n    export GEMINI_API_KEY="your-api-key"\n    gemini-api run "Hello" --api-key "your-api-key"',
    );
  }

  const baseUrl = flags.baseUrl ?? process.env.GEMINI_API_BASE_URL ?? DEFAULT_BASE_URL;

  return { apiKey, baseUrl };
}

function cleanErrorMessage(message: string): string {
  return message.replace(/Did you mean '.*?'\?/, "").trim();
}

export async function apiRequest<T>(
  ctx: CLIContext,
  method: string,
  path: string,
  body?: unknown,
): Promise<T> {
  const url = `${ctx.baseUrl}${path}`;
  const headers: Record<string, string> = {
    "Content-Type": "application/json",
    "x-goog-api-key": ctx.apiKey,
    "x-server-timeout": "600",
  };

  if (path.includes("/interactions")) {
    headers["Api-Revision"] = "2026-05-20";
  }

  const response = await fetch(url, {
    method,
    headers,
    body: body ? JSON.stringify(body) : undefined,
  });

  if (!response.ok) {
    let errorMsg = `API error (${response.status})`;
    try {
      const errorData = await response.json();
      if (response.status === 400) {
        console.error("400 Error Data:", JSON.stringify(errorData, null, 2));
      }
      if (errorData.error?.message) {
        errorMsg += `: ${cleanErrorMessage(errorData.error.message)}`;
      }
    } catch {
      // Ignore JSON parse error
    }

    if (response.status === 401) {
      throw new CLIError(`${errorMsg}\n\n  Try:\n    export GEMINI_API_KEY="your-api-key"`);
    }

    if (response.status === 400) {
      throw new CLIError(
        `${errorMsg}\n\n  Try:\n    gemini-api run "Hello" --model gemini-3-flash-preview`,
      );
    }

    throw new CLIError(errorMsg);
  }

  return response.json() as Promise<T>;
}

export async function apiGetRequest<T>(
  ctx: CLIContext,
  path: string,
  params?: Record<string, string>,
): Promise<T> {
  let url = `${ctx.baseUrl}${path}`;
  if (params) {
    const qs = new URLSearchParams(params).toString();
    url += `?${qs}`;
  }
  const headers: Record<string, string> = {
    "x-goog-api-key": ctx.apiKey,
  };

  const response = await fetch(url, { method: "GET", headers });

  if (!response.ok) {
    let errorMsg = `API error (${response.status})`;
    try {
      const errorData = await response.json();
      if (errorData.error?.message) {
        errorMsg += `: ${cleanErrorMessage(errorData.error.message)}`;
      }
    } catch {
      // Ignore JSON parse error
    }
    throw new CLIError(errorMsg);
  }

  return response.json() as Promise<T>;
}

export async function apiGetStreamRequest(
  ctx: CLIContext,
  path: string,
  params?: Record<string, string>,
): Promise<Response> {
  let url = `${ctx.baseUrl}${path}`;
  if (params) {
    const qs = new URLSearchParams(params).toString();
    url += `?${qs}`;
  }
  const headers: Record<string, string> = {
    "x-goog-api-key": ctx.apiKey,
  };

  const response = await fetch(url, { method: "GET", headers });

  if (!response.ok) {
    let errorMsg = `API error (${response.status})`;
    try {
      const errorData = await response.json();
      if (errorData.error?.message) {
        errorMsg += `: ${cleanErrorMessage(errorData.error.message)}`;
      }
    } catch {
      // Ignore JSON parse error
    }
    throw new CLIError(errorMsg);
  }

  return response;
}

export async function apiStreamRequest(
  ctx: CLIContext,
  path: string,
  body: unknown,
): Promise<Response> {
  const url = `${ctx.baseUrl}${path}`;
  const headers: Record<string, string> = {
    "Content-Type": "application/json",
    "x-goog-api-key": ctx.apiKey,
    "x-server-timeout": "600",
  };

  if (path.includes("/interactions")) {
    headers["Api-Revision"] = "2026-05-20";
  }

  const response = await fetch(url, {
    method: "POST",
    headers,
    body: JSON.stringify(body),
  });

  if (!response.ok) {
    let errorMsg = `API error (${response.status})`;
    try {
      const errorData = await response.json();
      if (response.status === 400) {
        console.error("400 Error Data:", JSON.stringify(errorData, null, 2));
      }
      if (errorData.error?.message) {
        errorMsg += `: ${cleanErrorMessage(errorData.error.message)}`;
      }
    } catch {
      // Ignore JSON parse error
    }
    throw new CLIError(errorMsg);
  }

  return response;
}

export type InteractionsInput = string | { parts: Array<any> };

export interface Tool {
  type: string;
  [key: string]: unknown;
}

export function parseToolFlag(value: string): Tool {
  // Simple tool types
  if (
    [
      "code_execution",
      "google_search",
      "url_context",
      "computer_use",
      "file_search",
      "google_maps",
      "retrieval",
    ].includes(value)
  ) {
    return { type: value };
  }

  // mcp_server:name:url
  if (value.startsWith("mcp_server:")) {
    const firstColon = value.indexOf(":");
    const secondColon = value.indexOf(":", firstColon + 1);
    if (secondColon === -1) {
      throw new CLIError("Invalid mcp_server format. Expected: mcp_server:name:url");
    }
    const name = value.substring(firstColon + 1, secondColon);
    const url = value.substring(secondColon + 1);
    if (!name || !url) {
      throw new CLIError("Invalid mcp_server format. Expected: mcp_server:name:url");
    }
    return { type: "mcp_server", name, url };
  }

  // function:name:schema
  if (value.startsWith("function:")) {
    const [_, name, ...rest] = value.split(":");
    if (!name || rest.length === 0) {
      throw new CLIError("Invalid function format. Expected: function:name:schema");
    }
    const parametersStr = rest.join(":");
    try {
      const parameters = JSON.parse(parametersStr);
      return { type: "function", name, parameters };
    } catch (e) {
      throw new CLIError(`Invalid JSON in function schema: ${(e as Error).message}`);
    }
  }

  throw new CLIError(
    `Unknown tool: '${value}'\n\n  Available: code_execution, google_search, url_context, computer_use, mcp_server, file_search, google_maps, retrieval, function`,
  );
}

export interface Source {
  type: string;
  [key: string]: string;
}

export function parseSourceFlag(value: string): Source {
  // inline:<target>:<content>
  if (value.startsWith("inline:")) {
    const rest = value.substring(7);
    const idx = rest.indexOf(":");
    if (idx === -1) {
      throw new CLIError("Invalid inline source format. Expected: inline:<target>:<content>");
    }
    return { type: "inline", target: rest.substring(0, idx), content: rest.substring(idx + 1) };
  }

  // github:<url>:<target> — split on last colon since URLs contain colons
  if (value.startsWith("github:")) {
    const rest = value.substring(7);
    const idx = rest.lastIndexOf(":");
    if (idx === -1) {
      throw new CLIError("Invalid github source format. Expected: github:<url>:<target>");
    }
    return { type: "github", source: rest.substring(0, idx), target: rest.substring(idx + 1) };
  }

  // gcs:<source>:<target> — split on last colon
  if (value.startsWith("gcs:")) {
    const rest = value.substring(4);
    const idx = rest.lastIndexOf(":");
    if (idx === -1) {
      throw new CLIError("Invalid gcs source format. Expected: gcs:<source>:<target>");
    }
    return { type: "gcs", source: rest.substring(0, idx), target: rest.substring(idx + 1) };
  }

  throw new CLIError(`Unknown source type in '${value}'\n\n  Available: inline, github, gcs`);
}

export interface RunOptions {
  model?: string;
  agent?: string;
  input: InteractionsInput;
  systemInstruction?: string;
  tools?: Tool[];
  responseModalities?: string[];
  responseFormat?: unknown;
  responseMimeType?: string;
  serviceTier?: string;
  previousInteractionId?: string;
  stream?: boolean;

  voice?: string;
  language?: string;
  aspectRatio?: string;
  imageSize?: string;
  toolChoice?: string;
  editStrength?: number;
  mask?: string;

  sources?: Source[];
  // Environment override (for agents test)
  environment?: object;
}

// Agents that automatically get `environment: { enabled: true }` when used
// via `gemini-api run --agent <name>` (i.e., without an agent.yaml config).
const _ENVIRONMENT_ENABLED_AGENTS = ["waverunner"];

// Known agent name prefixes. Everything else is treated as a model name.
const AGENT_PREFIXES = ["waverunner", "deep-research"];

// Deep Research agent prefixes — these get background:true and agent_config auto-injected.
const DEEP_RESEARCH_PREFIX = "deep-research";

/** Returns true if the base_agent value is an agent name (not a model). */
export function isAgentName(name?: string): boolean {
  if (!name) return false;
  return AGENT_PREFIXES.some((prefix) => name === prefix || name.startsWith(`${prefix}-`));
}

/** Returns true if the agent is a Deep Research agent. */
export function isDeepResearchAgent(agent?: string): boolean {
  if (!agent) return false;
  return agent === DEEP_RESEARCH_PREFIX || agent.startsWith(`${DEEP_RESEARCH_PREFIX}-`);
}

export function buildInteractionRequest(opts: RunOptions): object {
  const body: any = {
    input: opts.input,
  };

  // Environment: custom sources take priority over auto-enable
  if (opts.sources && opts.sources.length > 0) {
    body.environment = { type: "remote", sources: opts.sources };
  } else if (opts.agent && !isDeepResearchAgent(opts.agent)) {
    body.environment = { enabled: true };
  }

  if (opts.model) body.model = opts.model;
  if (opts.agent) body.agent = opts.agent;
  if (opts.systemInstruction) body.system_instruction = opts.systemInstruction;
  if (opts.tools) body.tools = opts.tools;
  if (opts.responseModalities) body.response_modalities = opts.responseModalities;
  if (opts.responseFormat) body.response_format = opts.responseFormat;
  if (opts.responseMimeType) body.response_mime_type = opts.responseMimeType;
  if (opts.serviceTier) body.service_tier = opts.serviceTier;
  if (opts.previousInteractionId) body.previous_interaction_id = opts.previousInteractionId;
  if (opts.stream !== undefined) body.stream = opts.stream;

  // Deep Research agents: auto-inject background:true and agent_config
  if (isDeepResearchAgent(opts.agent)) {
    body.background = true;
    body.agent_config = {
      type: "deep-research",
      thinking_summaries: "auto",
    };
  }

  // Environment override (from agents test command)
  if (opts.environment) body.environment = opts.environment;

  // Generation Config
  const generationConfig: any = {};

  if (opts.toolChoice) generationConfig.tool_choice = opts.toolChoice;

  if (opts.voice || opts.language) {
    const speechConfig: any = {};
    if (opts.voice) speechConfig.voice = opts.voice;
    if (opts.language) speechConfig.language = opts.language;
    generationConfig.speech_config = [speechConfig];
  }

  if (opts.aspectRatio || opts.imageSize || opts.editStrength !== undefined || opts.mask) {
    generationConfig.image_config = {};
    if (opts.aspectRatio) generationConfig.image_config.aspect_ratio = opts.aspectRatio;
    if (opts.imageSize) generationConfig.image_config.image_size = opts.imageSize;
    if (opts.editStrength !== undefined)
      generationConfig.image_config.edit_strength = opts.editStrength;
    if (opts.mask) generationConfig.image_config.mask = opts.mask;
  }

  if (Object.keys(generationConfig).length > 0) {
    body.generation_config = generationConfig;
  }

  return body;
}
