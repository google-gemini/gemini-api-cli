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
  const apiKey = flags.apiKey ?? process.env.GEMINI_AUTOPUSH_API_KEY ?? process.env.GEMINI_API_KEY;
  if (!apiKey) {
    throw new CLIError(
      "No API key found.\n\n  Try:\n    export GEMINI_API_KEY=\"your-api-key\"\n    gemini-api run \"Hello\" --api-key \"your-api-key\""
    );
  }

  const baseUrl = flags.baseUrl 
    ?? process.env.GEMINI_API_BASE_URL 
    ?? DEFAULT_BASE_URL;

  return { apiKey, baseUrl };
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
  };

  const response = await fetch(url, {
    method,
    headers,
    body: body ? JSON.stringify(body) : undefined,
  });

  if (!response.ok) {
    let errorMsg = `API error (${response.status})`;
    try {
      const errorData = await response.json();
      if (errorData.error?.message) {
        errorMsg += `: ${errorData.error.message}`;
      }
    } catch {
      // Ignore JSON parse error
    }

    if (response.status === 401) {
      throw new CLIError(`${errorMsg}\n\n  Try:\n    export GEMINI_API_KEY="your-api-key"`);
    }
    
    if (response.status === 400) {
       throw new CLIError(`${errorMsg}\n\n  Try:\n    gemini-api run "Hello" --model gemini-3-flash-preview`);
    }

    throw new CLIError(errorMsg);
  }

  return response.json() as Promise<T>;
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
  };

  const response = await fetch(url, {
    method: "POST",
    headers,
    body: JSON.stringify(body),
  });

  if (!response.ok) {
     let errorMsg = `API error (${response.status})`;
    try {
      const errorData = await response.json();
      if (errorData.error?.message) {
        errorMsg += `: ${errorData.error.message}`;
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
  if (["code_execution", "google_search", "url_context", "computer_use", 
       "file_search", "google_maps", "retrieval"].includes(value)) {
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
  
  throw new CLIError(`Unknown tool: '${value}'\n\n  Available: code_execution, google_search, url_context, computer_use, mcp_server, file_search, google_maps, retrieval, function`);
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
}

export function buildInteractionRequest(opts: RunOptions): object {
  const body: any = {
    input: opts.input,
  };

  if (opts.agent === "waverunner") {
    body.environment = { enabled: true };
  }

  if (opts.model) body.model = opts.model;
  if (opts.agent) body.agent = opts.agent;
  if (opts.systemInstruction) body.system_instruction = opts.systemInstruction;
  if (opts.tools) body.tools = opts.tools;
  if (opts.responseModalities) body.response_modalities = opts.responseModalities;
  if (opts.responseFormat) body.responseFormat = opts.responseFormat;
  if (opts.responseMimeType) body.responseMimeType = opts.responseMimeType;
  if (opts.serviceTier) body.serviceTier = opts.serviceTier;
  if (opts.previousInteractionId) body.previous_interaction_id = opts.previousInteractionId;
  if (opts.stream !== undefined) body.stream = opts.stream;


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
    if (opts.aspectRatio) generationConfig.image_config.aspectRatio = opts.aspectRatio;
    if (opts.imageSize) generationConfig.image_config.imageSize = opts.imageSize;
    if (opts.editStrength !== undefined) generationConfig.image_config.editStrength = opts.editStrength;
    if (opts.mask) generationConfig.image_config.mask = opts.mask;
  }

  if (Object.keys(generationConfig).length > 0) {
    body.generation_config = generationConfig;
  }

  return body;
}
