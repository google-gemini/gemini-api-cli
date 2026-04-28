import { mkdirSync, appendFileSync } from "node:fs";
import { join } from "node:path";
import type { StreamResult, ContentBlock } from "./stream";

const LOG_DIR = join(process.cwd(), ".gemini", "logs");

export function logRequest(interactionId: string, request: object): void {
  try {
    mkdirSync(LOG_DIR, { recursive: true });
    const line = JSON.stringify({
      type: "request",
      timestamp: new Date().toISOString(),
      data: request,
    });
    appendFileSync(join(LOG_DIR, `${interactionId}.jsonl`), line + "\n");
  } catch (e) {
    console.error(`Failed to log request: ${(e as Error).message}`);
  }
}

export function logResponse(interactionId: string, result: StreamResult): void {
  try {
    mkdirSync(LOG_DIR, { recursive: true });
    
    // Strip binary data from outputs
    const outputs = result.outputs.map(stripBinaryData);
    
    const line = JSON.stringify({
      type: "response",
      timestamp: new Date().toISOString(),
      data: {
        id: result.interactionId,
        status: result.status,
        outputs,
        usage: result.usage,
        created: result.created,
        updated: result.updated,
      },
    });
    appendFileSync(join(LOG_DIR, `${interactionId}.jsonl`), line + "\n");
  } catch (e) {
    console.error(`Failed to log response: ${(e as Error).message}`);
  }
}

function stripBinaryData(block: ContentBlock): ContentBlock {
  if (["image", "audio", "document", "video"].includes(block.type)) {
    const stripped = { ...block };
    delete (stripped as any).data;
    return stripped as ContentBlock;
  }
  return block;
}
