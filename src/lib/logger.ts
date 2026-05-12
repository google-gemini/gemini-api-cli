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

import { appendFileSync, mkdirSync } from "node:fs";
import { join } from "node:path";
import type { ContentBlock, StreamResult } from "./stream";

const LOG_DIR = join(process.cwd(), ".gemini", "logs");

export function logRequest(interactionId: string, request: object): void {
  try {
    mkdirSync(LOG_DIR, { recursive: true });
    const line = JSON.stringify({
      type: "request",
      timestamp: new Date().toISOString(),
      data: request,
    });
    appendFileSync(join(LOG_DIR, `${interactionId}.jsonl`), `${line}\n`);
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
    appendFileSync(join(LOG_DIR, `${interactionId}.jsonl`), `${line}\n`);
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
