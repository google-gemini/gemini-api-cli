export class CLIError extends Error {
  constructor(message: string) {
    super(message);
    this.name = "CLIError";
  }
}

export class APIError extends Error {
  constructor(message: string) {
    super(message);
    this.name = "APIError";
  }
}

export class ConfigError extends Error {
  constructor(message: string) {
    super(message);
    this.name = "ConfigError";
  }
}
