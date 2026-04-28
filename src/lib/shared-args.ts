export const globalFlags = {
  "api-key": {
    type: "string" as const,
    description: "API key for authentication (or set GEMINI_API_KEY)",
  },
  "base-url": {
    type: "string" as const,
    description: "Override API base URL (or set GEMINI_API_BASE_URL)",
  },
  json: {
    type: "boolean" as const,
    description: "JSON output mode",
    default: false,
  },
  "dry-run": {
    type: "boolean" as const,
    description: "Print curl and exit",
    default: false,
  },
  verbose: {
    type: "boolean" as const,
    description: "Debug logging",
    default: false,
  },
};
