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

import { APIError, CLIError } from "./errors";

export interface StreamEvent {
  type:
    | "interaction.created"
    | "content.start"
    | "content.delta"
    | "content.stop"
    | "step.start"
    | "step.delta"
    | "step.stop"
    | "interaction.completed"
    | "interaction.status_update"
    | "error";
  data: any;
  raw: string; // Original SSE JSON for --json mode
}

export interface Usage {
  inputTokens?: number;
  outputTokens?: number;
  thoughtTokens?: number;
  cachedTokens?: number;
}

export interface StepInfo {
  index: number;
  type?: string;
  status?: string;
  text?: string;
}

export interface StreamResult {
  interactionId: string;
  status: string;
  outputs: ContentBlock[]; // Reassembled content blocks
  steps: StepInfo[]; // Accumulated step data
  usage?: Usage;
  created?: string;
  updated?: string;
  environmentId?: string;
  lastEventId?: string; // For deep-research stream reconnection
}

export type ContentBlock =
  | { type: "text"; text: string }
  | { type: "image"; data: string; mimeType: string }
  | { type: "audio"; data: string; mimeType: string }
  | { type: "document"; data: string; mimeType: string }
  | { type: "video"; data: string; mimeType: string }
  | { type: "function_call"; name: string; arguments: object; id: string }
  | { type: "code_execution_call"; arguments: { code: string }; id: string }
  | { type: "code_execution_result"; result: string; isError: boolean; callId: string }
  | { type: "thought_summary"; text: string }
  | { type: "thought_signature"; signature: string }
  | { type: "url_context_call"; url: string }
  | { type: "google_search_call"; query: string }
  | { type: "mcp_server_tool_call"; server: string; tool: string; arguments: object }
  | { type: "file_search_call"; query: string }
  | { type: "google_maps_call"; query: string }
  | { type: "function_result"; result: unknown; callId: string }
  | { type: "url_context_result"; result: unknown; callId: string }
  | { type: "google_search_result"; result: unknown; callId: string }
  | { type: "mcp_server_tool_result"; result: unknown; callId: string }
  | { type: "file_search_result"; result: unknown; callId: string }
  | { type: "google_maps_result"; result: unknown; callId: string }
  | { type: "text_annotation"; annotations: unknown[] };

export async function processStream(
  response: Response,
  callbacks: {
    onEvent: (event: StreamEvent, block?: ContentBlock) => void; // Called for each SSE event
    onComplete: (result: StreamResult) => void; // Called when stream ends
    onBlockComplete?: (block: ContentBlock) => void; // Called when a block is complete
  },
): Promise<StreamResult> {
  if (!response.body) {
    throw new CLIError("Response body is null");
  }

  const reader = response.body.getReader();
  const decoder = new TextDecoder();
  let buffer = "";

  const result: StreamResult = {
    interactionId: "",
    status: "",
    outputs: [],
    steps: [],
  };

  const contentBlocks: Map<number, ContentBlock> = new Map();
  const completedBlocks = new Set<number>();

  try {
    while (true) {
      const { done, value } = await reader.read();
      if (done) break;

      buffer += decoder.decode(value, { stream: true });
      const lines = buffer.split("\n");
      buffer = lines.pop() || "";

      for (const line of lines) {
        const trimmed = line.trim();
        if (!trimmed) continue;

        if (trimmed.startsWith("data:")) {
          const dataStr = trimmed.substring(5).trim();
          if (dataStr === "[DONE]") continue;

          try {
            const data = JSON.parse(dataStr);
            const event: StreamEvent = {
              type: data.event_type,
              data: data,
              raw: dataStr,
            };
            handleEvent(event, result, contentBlocks);
            const block =
              event.data && event.data.index !== undefined
                ? contentBlocks.get(event.data.index)
                : undefined;
            callbacks.onEvent(event, block);

            if (event.type === "content.stop") {
              const index = event.data.index;
              completedBlocks.add(index);
              const block = contentBlocks.get(index);
              if (block) {
                callbacks.onBlockComplete?.(block);
              }
            }
          } catch (_e) {
            // Malformed SSE lines are handled gracefully
            // console.warn("Failed to parse SSE data:", dataStr, e);
          }
        }
      }
    }

    // Handle remaining buffer
    if (buffer.trim().startsWith("data:")) {
      const dataStr = buffer.trim().substring(5).trim();
      if (dataStr !== "[DONE]") {
        try {
          const data = JSON.parse(dataStr);
          const event: StreamEvent = {
            type: data.event_type,
            data: data,
            raw: dataStr,
          };
          callbacks.onEvent(event);
          handleEvent(event, result, contentBlocks);
        } catch (_e) {
          // Ignore
        }
      }
    }
  } catch (e) {
    throw new APIError(`SSE connection error: ${(e as Error).message}`);
  }

  // Call onBlockComplete for any blocks that didn't get content.stop
  for (const [index, block] of contentBlocks.entries()) {
    if (!completedBlocks.has(index)) {
      callbacks.onBlockComplete?.(block);
    }
  }

  // Convert contentBlocks map to array
  result.outputs = Array.from(contentBlocks.values());

  // Remove holes from sparse steps array
  result.steps = result.steps.filter(Boolean);

  // Finalize any blocks if needed
  for (const block of result.outputs) {
    if (block.type === "function_call" && typeof (block as any).arguments === "string") {
      try {
        (block as any).arguments = JSON.parse((block as any).arguments);
      } catch {
        // Ignore
      }
    }
    if (block.type === "mcp_server_tool_call" && typeof (block as any).arguments === "string") {
      try {
        (block as any).arguments = JSON.parse((block as any).arguments);
      } catch {
        // Ignore
      }
    }
  }

  callbacks.onComplete(result);
  return result;
}

function handleEvent(
  event: StreamEvent,
  result: StreamResult,
  contentBlocks: Map<number, ContentBlock>,
) {
  const data = event.data;

  if (data.interaction) {
    if (data.interaction.id) result.interactionId = data.interaction.id;
    if (data.interaction.status) result.status = data.interaction.status;
    if (data.interaction.created_at) result.created = data.interaction.created_at;
    if (data.interaction.updated_at) result.updated = data.interaction.updated_at;
    if (data.interaction.environment_id) result.environmentId = data.interaction.environment_id;
  }
  if (data.interaction_id) {
    result.interactionId = data.interaction_id;
  }
  if (data.environment_id) {
    result.environmentId = data.environment_id;
  }
  if (data.event_id) {
    result.lastEventId = data.event_id;
  }

  if (event.type === "content.start") {
    const index = data.index;
    const content = data.content;
    if (content?.type) {
      const block: any = { type: content.type };
      if (content.name) block.name = content.name;
      contentBlocks.set(index, block);
    }
  } else if (event.type === "content.delta") {
    const index = data.index;
    const delta = data.delta;
    const block = contentBlocks.get(index);

    if (block && delta) {
      switch (block.type) {
        case "text":
          if (delta.text) (block as any).text = ((block as any).text || "") + delta.text;
          break;
        case "image":
        case "audio":
        case "document":
        case "video":
          if (delta.data) (block as any).data = ((block as any).data || "") + delta.data;
          if (delta.mime_type) (block as any).mimeType = delta.mime_type;
          break;
        case "function_call":
          if (delta.name) (block as any).name = delta.name;
          if (delta.arguments) {
            if (typeof delta.arguments === "string") {
              (block as any).arguments = ((block as any).arguments || "") + delta.arguments;
            } else {
              (block as any).arguments = delta.arguments;
            }
          }
          if (delta.id) (block as any).id = delta.id;
          break;
        case "code_execution_call":
          if (delta.arguments?.code) {
            (block as any).arguments = (block as any).arguments || { code: "" };
            (block as any).arguments.code += delta.arguments.code;
          }
          if (delta.id) (block as any).id = delta.id;
          break;
        case "code_execution_result":
          if (delta.result) (block as any).result = ((block as any).result || "") + delta.result;
          if (delta.is_error !== undefined) (block as any).isError = delta.is_error;
          if (delta.call_id) (block as any).callId = delta.call_id;
          break;
        case "thought_summary":
          if (delta.text) (block as any).text = ((block as any).text || "") + delta.text;
          break;
        case "thought_signature":
          if (delta.signature) (block as any).signature = delta.signature;
          break;
        case "url_context_call":
          if (delta.url) (block as any).url = delta.url;
          break;
        case "google_search_call":
          if (delta.query) (block as any).query = delta.query;
          break;
        case "mcp_server_tool_call":
          if (delta.server) (block as any).server = delta.server;
          if (delta.tool) (block as any).tool = delta.tool;
          if (delta.arguments) {
            if (typeof delta.arguments === "string") {
              (block as any).arguments = ((block as any).arguments || "") + delta.arguments;
            } else {
              (block as any).arguments = delta.arguments;
            }
          }
          break;
        case "file_search_call":
          if (delta.query) (block as any).query = delta.query;
          break;
        case "google_maps_call":
          if (delta.query) (block as any).query = delta.query;
          break;
        case "function_result":
          if (delta.result) (block as any).result = delta.result;
          if (delta.call_id) (block as any).callId = delta.call_id;
          break;
        case "url_context_result":
          if (delta.result) (block as any).result = delta.result;
          if (delta.call_id) (block as any).callId = delta.call_id;
          break;
        case "google_search_result":
          if (delta.result) (block as any).result = delta.result;
          if (delta.call_id) (block as any).callId = delta.call_id;
          break;
        case "mcp_server_tool_result":
          if (delta.result) (block as any).result = delta.result;
          if (delta.call_id) (block as any).callId = delta.call_id;
          break;
        case "file_search_result":
          if (delta.result) (block as any).result = delta.result;
          if (delta.call_id) (block as any).callId = delta.call_id;
          break;
        case "google_maps_result":
          if (delta.result) (block as any).result = delta.result;
          if (delta.call_id) (block as any).callId = delta.call_id;
          break;
        case "text_annotation":
          if (delta.annotations) {
            (block as any).annotations = (block as any).annotations || [];
            (block as any).annotations.push(...delta.annotations);
          }
          break;
      }
    }
  } else if (event.type === "step.start") {
    const index = data.index ?? data.step_index;
    if (index !== undefined) {
      const step: StepInfo = { index };
      if (data.step?.type) step.type = data.step.type;
      if (data.step?.status) step.status = data.step.status;
      result.steps[index] = step;
      if (data.step?.type === "model_output" && Array.isArray(data.step.content)) {
        for (const c of data.step.content) {
          if (["text", "image", "audio", "video", "document"].includes(c.type)) {
            const block: any = { type: c.type };
            if (c.text) block.text = c.text;
            if (c.data) block.data = c.data;
            if (c.mime_type) block.mimeType = c.mime_type;
            contentBlocks.set(index, block);
          }
        }
      }
    }
  } else if (event.type === "step.delta") {
    const index = data.index ?? data.step_index;
    if (index !== undefined) {
      const step = result.steps[index] || { index };
      const delta = data.delta;
      if (delta) {
        if (delta.text) step.text = (step.text || "") + delta.text;
        if (delta.type) step.type = delta.type;
        if (delta.status) step.status = delta.status;

        // Also append media/block data to contentBlocks if available
        let block = contentBlocks.get(index);
        if (!block && delta.type) {
          let type: string | undefined;
          if (
            [
              "text",
              "image",
              "audio",
              "video",
              "document",
              "function_call",
              "code_execution_call",
              "code_execution_result",
              "thought_summary",
              "thought_signature",
              "url_context_call",
              "google_search_call",
              "mcp_server_tool_call",
              "file_search_call",
              "google_maps_call",
              "function_result",
              "url_context_result",
              "google_search_result",
              "mcp_server_tool_result",
              "file_search_result",
              "google_maps_result",
              "text_annotation",
            ].includes(delta.type)
          ) {
            type = delta.type;
          } else if (delta.type === "thought") {
            type = "thought_summary";
          }

          if (type) {
            const newBlock = { type } as ContentBlock;
            block = newBlock;
            contentBlocks.set(index, newBlock);
          }
        }

        if (block) {
          if (delta.data) (block as any).data = ((block as any).data || "") + delta.data;
          if (delta.mime_type) (block as any).mimeType = delta.mime_type;
          if (delta.text) (block as any).text = ((block as any).text || "") + delta.text;
          if (delta.signature) (block as any).signature = delta.signature;

          // Tool call arguments
          if (delta.name) (block as any).name = delta.name;
          if (delta.arguments) {
            if (typeof delta.arguments === "string") {
              (block as any).arguments = ((block as any).arguments || "") + delta.arguments;
            } else {
              (block as any).arguments = delta.arguments;
            }
          }
          if (delta.id) (block as any).id = delta.id;

          // Results
          if (delta.result) (block as any).result = ((block as any).result || "") + delta.result;
          if (delta.is_error !== undefined) (block as any).isError = delta.is_error;
          if (delta.call_id) (block as any).callId = delta.call_id;
          if (delta.url) (block as any).url = delta.url;
          if (delta.query) (block as any).query = delta.query;
          if (delta.server) (block as any).server = delta.server;
          if (delta.tool) (block as any).tool = delta.tool;
          if (delta.annotations) {
            (block as any).annotations = (block as any).annotations || [];
            (block as any).annotations.push(...delta.annotations);
          }
        }
      }
      result.steps[index] = step;
    }
  } else if (event.type === "step.stop") {
    const index = data.index ?? data.step_index;
    if (index !== undefined) {
      const step = result.steps[index];
      if (step) {
        step.status = data.step?.status || "completed";
      }
    }
  } else if (event.type === "interaction.completed") {
    const usage = data.usage || data.interaction?.usage;
    if (usage) {
      result.usage = {
        inputTokens: usage.total_input_tokens ?? usage.input_tokens,
        outputTokens: usage.total_output_tokens ?? usage.output_tokens,
        thoughtTokens: usage.total_thought_tokens ?? usage.thought_tokens,
        cachedTokens: usage.total_cached_tokens ?? usage.cached_tokens,
      };
    }
  }
}
