import { CLIError, APIError } from "./errors";

export interface StreamEvent {
  type: "interaction.start" | "content.start" | "content.delta" | "content.stop" | "interaction.complete" | "interaction.status_update" | "error";
  data: any;
  raw: string;  // Original SSE JSON for --json mode
}

export interface Usage {
  inputTokens?: number;
  outputTokens?: number;
  thoughtTokens?: number;
}

export interface StreamResult {
  interactionId: string;
  status: string;
  outputs: ContentBlock[];  // Reassembled content blocks
  usage?: Usage;
  created?: string;
  updated?: string;
  environmentId?: string;
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
  | { type: "text_annotation"; annotations: unknown[] }
;

export async function processStream(
  response: Response,
  callbacks: {
    onEvent: (event: StreamEvent, block?: ContentBlock) => void;       // Called for each SSE event
    onComplete: (result: StreamResult) => void;   // Called when stream ends
    onBlockComplete?: (block: ContentBlock) => void; // Called when a block is complete
  }
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
            const block = event.data && event.data.index !== undefined ? contentBlocks.get(event.data.index) : undefined;
            callbacks.onEvent(event, block);

            if (event.type === "content.stop") {
              const index = event.data.index;
              completedBlocks.add(index);
              const block = contentBlocks.get(index);
              if (block) {
                callbacks.onBlockComplete?.(block);
              }
            }

          } catch (e) {
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
        } catch (e) {
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

function handleEvent(event: StreamEvent, result: StreamResult, contentBlocks: Map<number, ContentBlock>) {
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

  if (event.type === "content.start") {
    const index = data.index;
    const content = data.content;
    if (content && content.type) {
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
  } else if (event.type === "interaction.complete") {
    if (data.usage) {
      result.usage = {
        inputTokens: data.usage.input_tokens,
        outputTokens: data.usage.output_tokens,
        thoughtTokens: data.usage.thought_tokens,
      };
    }
  }
}
