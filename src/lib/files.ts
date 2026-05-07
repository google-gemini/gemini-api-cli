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

import { readFileSync, writeFileSync, existsSync, mkdirSync } from "node:fs";
import { readdir, readFile, stat } from "node:fs/promises";
import { extname, dirname, join, relative } from "node:path";

export interface Content {
  type: string;
  data: string;
  mime_type: string;
}

export function inputToContentBlock(input: string): Content {
  const [type, pathOrUrl] = input.split(":", 2);
  
  if (type === "url") {
    return {
      type: "url",
      data: pathOrUrl,
      mime_type: "text/plain",
    };
  }
  
  try {
    const data = readFileSync(pathOrUrl);
    const base64 = data.toString("base64");
    const mimeType = detectMimeType(pathOrUrl);
    
    return {
      type,
      data: base64,
      mime_type: mimeType,
    };
  } catch (error: any) {
    if (error.code === "ENOENT") {
      throw new Error(`File not found: ${pathOrUrl}`);
    }
    throw error;
  }
}

const MIME_TYPES: Record<string, string> = {
  // Images
  ".png": "image/png",
  ".jpg": "image/jpeg",
  ".jpeg": "image/jpeg",
  ".webp": "image/webp",
  ".heic": "image/heic",
  ".heif": "image/heif",
  ".gif": "image/gif",
  ".bmp": "image/bmp",
  ".tiff": "image/tiff",
  
  // Audio
  ".wav": "audio/wav",
  ".mp3": "audio/mp3",
  ".aiff": "audio/aiff",
  ".aac": "audio/aac",
  ".ogg": "audio/ogg",
  ".flac": "audio/flac",
  ".mpeg": "audio/mpeg",
  ".m4a": "audio/m4a",
  ".l16": "audio/l16",
  ".opus": "audio/opus",
  
  // Video
  ".mp4": "video/mp4",
  ".mov": "video/mov",
  ".avi": "video/avi",
  ".flv": "video/x-flv",
  ".webm": "video/webm",
  ".wmv": "video/wmv",
  ".3gpp": "video/3gpp",
  
  // Documents
  ".pdf": "application/pdf",
};

export function detectMimeType(path: string): string {
  const ext = extname(path).toLowerCase();
  const mimeType = MIME_TYPES[ext];
  if (!mimeType) {
    throw new Error(`Unsupported file extension: ${ext}`);
  }
  return mimeType;
}

export function mimeTypeToExt(mimeType: string): string | undefined {
  for (const ext in MIME_TYPES) {
    if (MIME_TYPES[ext] === mimeType) {
      return ext.substring(1);
    }
  }
  return undefined;
}

function getWavHeader(pcmLength: number, sampleRate: number = 24000, numChannels: number = 1, bitsPerSample: number = 16): Buffer {
  const header = Buffer.alloc(44);
  
  header.write("RIFF", 0);
  header.writeUInt32LE(36 + pcmLength, 4);
  header.write("WAVE", 8);
  
  header.write("fmt ", 12);
  header.writeUInt32LE(16, 16);
  header.writeUInt16LE(1, 20);
  header.writeUInt16LE(numChannels, 22);
  header.writeUInt32LE(sampleRate, 24);
  header.writeUInt32LE(sampleRate * numChannels * (bitsPerSample / 8), 28);
  header.writeUInt16LE(numChannels * (bitsPerSample / 8), 32);
  header.writeUInt16LE(bitsPerSample, 34);
  
  header.write("data", 36);
  header.writeUInt32LE(pcmLength, 40);
  
  return header;
}

export function saveMediaOutputs(outputs: any[], interactionId: string, requestedOutput?: string) {
  for (let i = 0; i < outputs.length; i++) {
    const block = outputs[i];
    if (["image", "audio", "video", "document"].includes(block.type)) {
      const data = block.data;
      const mimeType = block.mimeType || block.mime_type;
      if (data) {
        let filename = requestedOutput;
        if (!filename) {
          const ext = mimeTypeToExt(mimeType) || "bin";
          filename = `output/${interactionId}_${i}.${ext}`;
        }
        
        const dir = dirname(filename);
        if (!existsSync(dir)) {
          mkdirSync(dir, { recursive: true });
        }
        
        const buffer = Buffer.from(data, "base64");
        if (block.type === "audio" && mimeType === "audio/l16" && filename.toLowerCase().endsWith(".wav")) {
          const header = getWavHeader(buffer.length);
          writeFileSync(filename, Buffer.concat([header, buffer]));
        } else {
          writeFileSync(filename, buffer);
        }
        
        console.log(`[${block.type}] Saved to ${filename} (${mimeType})`);
      }
    }
  }
}

export interface InlineFile {
  type: "inline";
  target: string;
  content: string;
  encoding?: "base64";
}

// Extensions that should be read as binary (base64) rather than UTF-8 text
const BINARY_EXTENSIONS = new Set([
  // From MIME_TYPES
  ...Object.keys(MIME_TYPES),
  // Archives
  ".zip", ".tar", ".gz", ".bz2", ".xz", ".7z",
  // Executables / compiled
  ".wasm", ".so", ".dylib", ".dll", ".exe",
  // Other binary
  ".bin", ".dat", ".db", ".sqlite",
]);

function isBinaryFile(filePath: string): boolean {
  const ext = extname(filePath).toLowerCase();
  return BINARY_EXTENSIONS.has(ext);
}

export async function collectInlineFiles(
  dir: string,
  basePath: string = process.env.AGENTS_WORKSPACE_PATH ?? "/.agents/",
): Promise<InlineFile[]> {
  const prefix = basePath.endsWith("/") ? basePath : basePath + "/";
  const files: InlineFile[] = [];

  async function walk(currentDir: string): Promise<void> {
    let entries;
    try {
      entries = await readdir(currentDir, { withFileTypes: true });
    } catch {
      return;
    }

    for (const entry of entries) {
      const fullPath = join(currentDir, entry.name);

      if (entry.isDirectory()) {
        if (entry.name === "node_modules" || entry.name === ".git" || entry.name === ".gemini") continue;
        await walk(fullPath);
        continue;
      }

      if (entry.isFile()) {
        const rel = relative(dir, fullPath);
        if (rel === "agent.yaml") continue;

        if (rel === ".env") {
          try {
            const content = await readFile(fullPath, "utf-8");
            files.push({ type: "inline", target: "/credentials/.env", content });
          } catch {
            // Ignore
          }
          continue;
        }

        const info = await stat(fullPath);
        if (info.size > 1_048_576) continue;

        const target = prefix + rel;

        if (isBinaryFile(fullPath)) {
          try {
            const buffer = await readFile(fullPath);
            const content = buffer.toString("base64");
            files.push({ type: "inline", target, content, encoding: "base64" });
          } catch {
            // Skip unreadable binary files
          }
        } else {
          try {
            const content = await readFile(fullPath, "utf-8");
            files.push({ type: "inline", target, content });
          } catch {
            // Skip unreadable text files
          }
        }
      }
    }
  }

  await walk(dir);
  return files;
}

export function getEnvKeys(content: string): string[] {
  const keys: string[] = [];
  const lines = content.split("\n");
  for (const line of lines) {
    const trimmed = line.trim();
    if (trimmed.startsWith("#") || trimmed === "") continue;
    const match = trimmed.match(/^([^=]+)=(.*)$/);
    if (match) {
      const key = match[1]?.trim();
      const value = match[2]?.trim();
      if (key && value && value !== "") {
        keys.push(key);
      }
    }
  }
  return keys;
}
