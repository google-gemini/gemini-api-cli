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

/**
 * Cloudflare Worker + Container launcher for Gemini API Self-Hosted Environments.
 *
 * Routes each `environment_id` deterministically to a dedicated stateful
 * Durable Object and isolated container microVM (`env.SELF_HOSTED_WORKER.getByName`),
 * then runs `gemini-api environments connect`.
 */

import {DurableObject} from 'cloudflare:workers';

const DEFAULT_SERVER_URL = 'https://generativelanguage.googleapis.com';
const INACTIVITY_TIMEOUT_MS = 10 * 60 * 1_000;
const SAFE_ENV_ID_RE = /^[A-Za-z0-9][A-Za-z0-9._/-]{0,127}$/;

/**
 * Cloudflare Worker bindings and secrets for the self-hosted connector.
 */
export interface Env {
  /** Durable Object namespace bound to the `SelfHostedWorker` Container class. */
  SELF_HOSTED_WORKER: DurableObjectNamespace<SelfHostedWorker>;
  /** Shared bearer secret required on all `POST /connect` requests. */
  CLOUDFLARE_DISPATCH_SECRET: string;
  /** Scoped environment key for `gemini-api environments connect`. */
  GEMINI_ENVIRONMENT_KEY?: string;
  /** Optional custom Gemini API server URL allowed with Worker-level credentials. */
  GEMINI_SERVER_URL?: string;
}

/**
 * Request body sent to `POST /connect` to start a persistent 1:1 worker.
 */
export interface ConnectRequest {
  environment_id: string;
  environment_key?: string;
  server_url?: string;
}

/**
 * Durable Object managing an isolated Cloudflare Container per `environment_id`.
 */
export class SelfHostedWorker extends DurableObject<Env> {
  private connected = false;

  constructor(ctx: DurableObjectState, env: Env) {
    super(ctx, env);
    const container = this.ctx.container;
    if (container && container.running) {
      void ctx.blockConcurrencyWhile(() =>
        container.setInactivityTimeout(INACTIVITY_TIMEOUT_MS),
      );
    }
  }

  async startConnect(
    environmentKey: string,
    serverUrl?: string,
  ): Promise<{alreadyRunning: boolean}> {
    const container = this.ctx.container;
    if (!container) {
      throw new Error('Container attachment is unavailable');
    }
    if (this.connected && container.running) {
      return {alreadyRunning: true};
    }

    // Reserve connected state before awaiting container startup/timeout calls
    // so concurrent POST /connect requests cannot spawn duplicate processes.
    this.connected = true;
    try {
      if (!container.running) {
        container.start({
          image: container.images.self_hosted,
          instance: 'standard-1',
          enableInternet: true,
        });
        await container.setInactivityTimeout(INACTIVITY_TIMEOUT_MS);
      }
    } catch (err) {
      this.connected = false;
      throw err;
    }

    const argv = [
      'gemini-api',
      'environments',
      'connect',
      '--root-dir=/workspace',
      '--workdir=/workspace',
    ];
    if (serverUrl) {
      argv.push(`--server-url=${serverUrl}`);
    }

    this.ctx.waitUntil(
      (async () => {
        try {
          const proc = await container.exec(argv, {
            cwd: '/workspace',
            env: {GEMINI_ENVIRONMENT_KEY: environmentKey},
          });
          await proc.exitCode;
        } finally {
          this.connected = false;
        }
      })(),
    );

    return {alreadyRunning: false};
  }
}

/**
 * Compares two bearer tokens in constant time over fixed-length SHA-256 digests
 * so neither token content nor secret length leaks via timing side-channels.
 */
async function verifyBearerAuth(
  authHeader: string | null,
  expectedSecret: string,
): Promise<boolean> {
  if (!authHeader || !expectedSecret) {
    return false;
  }
  const match = /^Bearer\s+(.+)$/i.exec(authHeader.trim());
  if (!match) {
    return false;
  }
  const provided = match[1].trim();
  const expected = expectedSecret.trim();
  if (!provided || !expected) {
    return false;
  }

  const encoder = new TextEncoder();
  const [providedDigest, expectedDigest] = await Promise.all([
    crypto.subtle.digest('SHA-256', encoder.encode(provided)),
    crypto.subtle.digest('SHA-256', encoder.encode(expected)),
  ]);

  const subtleWithTimingSafe = crypto.subtle as SubtleCrypto & {
    timingSafeEqual?: (a: ArrayBuffer, b: ArrayBuffer) => boolean;
  };
  if (typeof subtleWithTimingSafe.timingSafeEqual === 'function') {
    return subtleWithTimingSafe.timingSafeEqual(providedDigest, expectedDigest);
  }

  const a = new Uint8Array(providedDigest);
  const b = new Uint8Array(expectedDigest);
  let diff = 0;
  for (let i = 0; i < a.byteLength; i++) {
    diff |= a[i] ^ b[i];
  }
  return diff === 0;
}

function validateEnvironmentId(environmentId: string): boolean {
  return (
    SAFE_ENV_ID_RE.test(environmentId) && !environmentId.includes('..')
  );
}

function validateHttpsServerUrl(rawUrl: string): string | null {
  if (
    !rawUrl ||
    rawUrl.startsWith('-') ||
    /[\s\u0000-\u001f]/.test(rawUrl)
  ) {
    return null;
  }
  let parsed: URL;
  try {
    parsed = new URL(rawUrl);
  } catch {
    return null;
  }
  if (
    parsed.protocol !== 'https:' ||
    !parsed.hostname ||
    parsed.username ||
    parsed.password ||
    parsed.search ||
    parsed.hash
  ) {
    return null;
  }
  return rawUrl;
}

function jsonResponse(body: Record<string, unknown>, status = 200): Response {
  return new Response(JSON.stringify(body, null, 2), {
    status,
    headers: {'content-type': 'application/json; charset=utf-8'},
  });
}

async function handleConnect(request: Request, env: Env): Promise<Response> {
  let body: ConnectRequest;
  try {
    body = (await request.json()) as ConnectRequest;
  } catch {
    return jsonResponse({error: 'Invalid JSON request body'}, 400);
  }

  const environmentId =
    typeof body.environment_id === 'string' ? body.environment_id.trim() : '';
  const callerKey =
    typeof body.environment_key === 'string' ? body.environment_key.trim() : '';
  const workerKey = (env.GEMINI_ENVIRONMENT_KEY ?? '').trim();
  const environmentKey = callerKey || workerKey;
  const rawServerUrl =
    typeof body.server_url === 'string' ? body.server_url.trim() : '';
  const configuredServerUrl = (env.GEMINI_SERVER_URL ?? '').trim();

  if (!environmentId || !validateEnvironmentId(environmentId)) {
    return jsonResponse({error: 'A valid environment_id is required'}, 400);
  }
  if (!environmentKey || /[\s\u0000-\u001f]/.test(environmentKey)) {
    return jsonResponse(
      {
        error:
          'environment_key (or GEMINI_ENVIRONMENT_KEY) is required for connect',
      },
      400,
    );
  }

  let effectiveServerUrl: string | undefined;
  if (rawServerUrl) {
    const validated = validateHttpsServerUrl(rawServerUrl);
    if (!validated) {
      return jsonResponse({error: 'Invalid https server_url format'}, 400);
    }
    const allowedWorkerServerUrl = configuredServerUrl || DEFAULT_SERVER_URL;
    if (!callerKey && validated !== allowedWorkerServerUrl) {
      return jsonResponse(
        {
          error:
            'server_url cannot be overridden when using Worker-scoped GEMINI_ENVIRONMENT_KEY',
        },
        400,
      );
    }
    effectiveServerUrl = validated;
  } else if (configuredServerUrl) {
    const validated = validateHttpsServerUrl(configuredServerUrl);
    if (!validated) {
      return jsonResponse({error: 'Invalid configured GEMINI_SERVER_URL'}, 500);
    }
    effectiveServerUrl = validated;
  }

  const workerStub = env.SELF_HOSTED_WORKER.getByName(environmentId);
  const {alreadyRunning} = await workerStub.startConnect(
    environmentKey,
    effectiveServerUrl,
  );

  return jsonResponse(
    {
      status: alreadyRunning ? 'already_connected' : 'connecting',
      environment_id: environmentId,
    },
    202,
  );
}

// tslint:disable-next-line:no-default-export
export default {
  async fetch(request: Request, env: Env): Promise<Response> {
    const url = new URL(request.url);

    if (request.method === 'GET' && url.pathname === '/healthz') {
      return jsonResponse({status: 'ok'}, 200);
    }

    if (
      !(await verifyBearerAuth(
        request.headers.get('authorization'),
        env.CLOUDFLARE_DISPATCH_SECRET,
      ))
    ) {
      return jsonResponse({error: 'Unauthorized'}, 401);
    }

    if (request.method === 'POST' && url.pathname === '/connect') {
      return handleConnect(request, env);
    }

    return jsonResponse({error: 'Not found'}, 404);
  },
} satisfies ExportedHandler<Env>;
