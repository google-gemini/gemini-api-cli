import { describe, test, expect } from "bun:test";
import { writeFileSync, unlinkSync, existsSync, statSync } from "node:fs";
import { execSync } from "node:child_process";

describe("multimodal input (live API)", () => {
  const runCli = (args: string, input?: string) => {
    const cmd = `bun run src/cli.ts ${args} 2>&1`;
    const options: any = { encoding: "utf-8" };
    if (input) {
      options.input = input;
    }
    try {
      return execSync(cmd, options);
    } catch (e: any) {
      return e.stdout;
    }
  };

  function createTestPng() {
    const base64 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==";
    return Buffer.from(base64, "base64");
  }

  test("image input is processed", () => {
    if (!process.env.GEMINI_API_KEY) {
        console.warn("Skipping live API test because GEMINI_API_KEY is not set");
        return;
    }
    const png = createTestPng();
    writeFileSync("test_input.png", png);
    
    const result = runCli('run "What color is this image?" --input image:test_input.png');
    expect(result.toLowerCase()).toMatch(/red|salmon|coral/);
    unlinkSync("test_input.png");
  }, 60000);

  test("missing input file errors clearly", () => {
    const result = runCli('run "Hello" --input image:nonexistent.png');
    expect(result).toContain("File not found");
  });
});

describe("image generation (live API)", () => {
  const runCli = (args: string) => {
    const cmd = `bun run src/cli.ts ${args} 2>&1`;
    try {
      return execSync(cmd, { encoding: "utf-8" });
    } catch (e: any) {
      return e.stdout;
    }
  };

  test("image output is saved to file", () => {
    if (!process.env.GEMINI_API_KEY) {
        console.warn("Skipping live API test because GEMINI_API_KEY is not set");
        return;
    }
    runCli('run "Generate a simple blue square" --model gemini-3-pro-image-preview --output test_output.png');
    expect(existsSync("test_output.png")).toBe(true);
    expect(statSync("test_output.png").size).toBeGreaterThan(100);
    unlinkSync("test_output.png");
  }, 60000);
});

describe("TTS (live API)", () => {
  const runCli = (args: string) => {
    const cmd = `bun run src/cli.ts ${args} 2>&1`;
    try {
      return execSync(cmd, { encoding: "utf-8" });
    } catch (e: any) {
      return e.stdout;
    }
  };

  test("audio output is saved to file", () => {
    if (!process.env.GEMINI_API_KEY) {
        console.warn("Skipping live API test because GEMINI_API_KEY is not set");
        return;
    }
    const result = runCli('run "Hello world" --model gemini-3.1-flash-tts-preview --voice Kore --output test_tts.wav');
    if (!existsSync("test_tts.wav")) {
      console.error("TTS failed. Output:", result);
    }
    expect(existsSync("test_tts.wav")).toBe(true);
    expect(statSync("test_tts.wav").size).toBeGreaterThan(100);
    unlinkSync("test_tts.wav");
  }, 60000);
});

describe("image editing (live API)", () => {
  const runCli = (args: string) => {
    const cmd = `bun run src/cli.ts ${args} 2>&1`;
    try {
      return execSync(cmd, { encoding: "utf-8" });
    } catch (e: any) {
      return e.stdout;
    }
  };

  test("image editing produces output file", () => {
    if (!process.env.GEMINI_API_KEY) {
        console.warn("Skipping live API test because GEMINI_API_KEY is not set");
        return;
    }
    
    // Create a test image
    const base64 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==";
    const png = Buffer.from(base64, "base64");
    writeFileSync("test_edit_input.png", png);
    
    try {
      runCli('run "Make this image green" --input image:test_edit_input.png --response-modality image --output test_edit_output.png --model gemini-3-pro-image-preview');
      
      expect(existsSync("test_edit_output.png")).toBe(true);
      expect(statSync("test_edit_output.png").size).toBeGreaterThan(100);
    } finally {
      if (existsSync("test_edit_input.png")) unlinkSync("test_edit_input.png");
      if (existsSync("test_edit_output.png")) unlinkSync("test_edit_output.png");
    }
  }, 60000);
});

describe("image editing (dry-run)", () => {
  const runCli = (args: string) => {
    const cmd = `bun run src/cli.ts ${args} 2>&1`;
    try {
      return execSync(cmd, { encoding: "utf-8" });
    } catch (e: any) {
      return e.stdout;
    }
  };

  test("image editing flags are mapped to generation_config", () => {
    // Create dummy files
    writeFileSync("tmp_input.png", "dummy content");
    writeFileSync("tmp_mask.png", "dummy content");
    
    try {
      const result = runCli(
        'run "Edit this image" --input image:tmp_input.png --response-modality image --edit-strength 0.5 --mask tmp_mask.png --dry-run'
      );
      
      expect(result).toContain('"edit_strength": 0.5');
      expect(result).toContain('"mask":');
    } finally {
      if (existsSync("tmp_input.png")) unlinkSync("tmp_input.png");
      if (existsSync("tmp_mask.png")) unlinkSync("tmp_mask.png");
    }
  });
});
