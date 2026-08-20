// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

/**
 * Shared framework for TypeScript SDK canaries.
 * Each scenario imports Config, Runner, and helpers from this module.
 */

import http from 'http';

// ── Config ────────────────────────────────────────────────────────────────────

export interface Config {
  apiUrl: string;
  apiKey: string;
  apiKeyUser2: string;
  email: string;
  password: string;
  llmProvider: string;
  llmApiKey: string;
  llmModel: string;
  badModel: string;
}

export function configFromEnv(): Config {
  return {
    apiUrl: process.env.LLMSAFESPACES_URL || 'http://localhost:8080',
    apiKey: process.env.LLMSAFESPACES_API_KEY || '',
    apiKeyUser2: process.env.LLMSAFESPACES_API_KEY_USER2 || '',
    email: process.env.LLMSAFESPACES_EMAIL || '',
    password: process.env.LLMSAFESPACES_PASSWORD || '',
    llmProvider: process.env.LLMSAFESPACES_LLM_PROVIDER || 'anthropic',
    llmApiKey: process.env.LLMSAFESPACES_LLM_API_KEY || '',
    llmModel: process.env.LLMSAFESPACES_LLM_MODEL || '',
    badModel: process.env.LLMSAFESPACES_BAD_MODEL || 'invalid-provider/no-such-model',
  };
}

// ── Result ────────────────────────────────────────────────────────────────────

export interface Check {
  name: string;
  passed: boolean;
  detail?: string;
}

export interface Result {
  scenario: string;
  sdk: string;
  passed: number;
  failed: number;
  duration_s: number;
  checks: Check[];
  error?: string;
}

export class Runner {
  private readonly _scenario: string;
  private readonly _sdk: string;
  private readonly _start: number;
  private _checks: Check[] = [];
  private _passed = 0;
  private _failed = 0;

  constructor(scenario: string, sdk = 'typescript-sdk') {
    this._scenario = scenario;
    this._sdk = sdk;
    this._start = Date.now();
  }

  assert(cond: boolean, name: string, detail = ''): boolean {
    this._checks.push({ name, passed: cond, detail: detail || undefined });
    if (cond) this._passed++; else this._failed++;
    return cond;
  }

  ok(name: string): void { this.assert(true, name); }
  fail(name: string, detail = ''): void { this.assert(false, name, detail); }

  async assertNoError<T>(fn: () => Promise<T>, name: string): Promise<[boolean, T | null]> {
    try {
      const result = await fn();
      this.ok(name);
      return [true, result];
    } catch (e: any) {
      this.fail(name, e?.message || String(e));
      return [false, null];
    }
  }

  async assertError(fn: () => Promise<unknown>, name: string): Promise<boolean> {
    try {
      await fn();
      this.fail(name, 'expected an error but got none');
      return false;
    } catch (e: any) {
      this.assert(true, name, e?.message || String(e));
      return true;
    }
  }

  result(): Result {
    return {
      scenario: this._scenario,
      sdk: this._sdk,
      passed: this._passed,
      failed: this._failed,
      duration_s: (Date.now() - this._start) / 1000,
      checks: this._checks,
    };
  }

  print(): Result {
    const res = this.result();
    console.log(`=== Canary: ${res.sdk} / ${res.scenario} ===`);
    for (const c of res.checks) {
      const mark = c.passed ? 'PASS' : 'FAIL';
      const detail = c.detail ? `: ${c.detail}` : '';
      console.log(`  ${mark} ${c.name}${detail}`);
    }
    console.log(`--- ${res.passed} passed, ${res.failed} failed in ${res.duration_s.toFixed(2)}s ---\n`);
    return res;
  }

  exitCode(): number { return this._failed > 0 ? 1 : 0; }
}

// ── HTTP helpers ──────────────────────────────────────────────────────────────

import https from 'https';

// resolveHttpModule returns the http or https module based on URL scheme.
function resolveHttpModule(url: URL): typeof http {
  return url.protocol === 'https:' ? (https as unknown as typeof http) : http;
}

export function nodeFetch(input: string, init?: RequestInit): Promise<Response> {
  const url = new URL(input);
  const mod = resolveHttpModule(url);
  return new Promise((resolve, reject) => {
    const agent = new (mod as any).Agent({ keepAlive: false });
    const req = mod.request({
      hostname: url.hostname,
      port: url.port || (url.protocol === 'https:' ? 443 : 80),
      path: url.pathname + url.search,
      method: (init?.method || 'GET').toUpperCase(),
      headers: { ...(init?.headers as Record<string, string> || {}) },
      agent,
    }, (res) => {
      const chunks: Buffer[] = [];
      res.on('data', (c: Buffer) => chunks.push(c));
      res.on('end', () => {
        const body = Buffer.concat(chunks).toString();
        resolve({
          ok: (res.statusCode || 500) >= 200 && (res.statusCode || 500) < 300,
          status: res.statusCode || 500,
          statusText: res.statusMessage || '',
          json: () => Promise.resolve(JSON.parse(body)),
          text: () => Promise.resolve(body),
          headers: res.headers as any,
        } as Response);
      });
      res.on('error', reject);
    });
    req.on('error', reject);
    if (init?.body) req.write(init.body as string);
    req.end();
  });
}

export async function rawDo(
  method: string, url: string, apiKey = '', body?: Buffer | string, timeoutMs = 15000
): Promise<[number, Buffer]> {
  return new Promise((resolve, reject) => {
    const u = new URL(url);
    const mod = resolveHttpModule(u);
    const req = mod.request({
      hostname: u.hostname,
      port: u.port || (u.protocol === 'https:' ? 443 : 80),
      path: u.pathname + u.search,
      method,
      headers: {
        'Content-Type': 'application/json',
        ...(apiKey ? { 'Authorization': `Bearer ${apiKey}` } : {}),
      },
    }, (res) => {
      const chunks: Buffer[] = [];
      res.on('data', (c: Buffer) => chunks.push(c));
      res.on('end', () => resolve([res.statusCode || 0, Buffer.concat(chunks)]));
      res.on('error', reject);
    });
    req.on('error', reject);
    req.setTimeout(timeoutMs, () => { req.destroy(); reject(new Error('timeout')); });
    if (body) req.write(body);
    req.end();
  });
}

export function hasErrorField(body: Buffer | string): boolean {
  try {
    const obj = JSON.parse(body.toString());
    return typeof obj?.error === 'string';
  } catch { return false; }
}

export function hasField(body: Buffer | string, field: string): boolean {
  try {
    const obj = JSON.parse(body.toString());
    return field in obj;
  } catch { return false; }
}

export function containsLeakedInternals(body: Buffer | string): boolean {
  const s = body.toString().toLowerCase();
  return ['panic:', 'runtime error:', 'goroutine ', 'stack trace'].some(m => s.includes(m));
}

export async function waitPhase(
  client: any, wsId: string, target: string, timeoutMs: number
): Promise<string> {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    try {
      const ws = await client.workspaces.get(wsId);
      if (ws.phase === target) return ws.phase;
    } catch {}
    await sleep(3000);
  }
  try { return (await client.workspaces.get(wsId)).phase; } catch { return 'unknown'; }
}

export async function waitActive(client: any, wsId: string): Promise<string> {
  return waitPhase(client, wsId, 'Active', 150000);
}

export async function ensureSessionWithRetry(client: any, wsId: string, maxTries = 5): Promise<any> {
  let lastErr: any;
  for (let i = 0; i < maxTries; i++) {
    try {
      const sess = await client.sessions.ensure(wsId);
      if (sess?.sessionId) return sess;
    } catch (e) { lastErr = e; }
    await sleep(5000);
  }
  throw new Error(`ensure session failed after ${maxTries} tries: ${lastErr}`);
}

export function sleep(ms: number): Promise<void> {
  return new Promise(r => setTimeout(r, ms));
}

// jwtLogin posts email/password and returns the JWT bearer token.
//
// Some endpoints (user-credential CRUD, user-secret CRUD) require the
// session-scoped DEK that only JWT login produces (post-Epic-56). API-key
// auth alone returns 403 "encryption key not available; re-authenticate"
// on those paths. Scenarios that exercise user-DEK-encrypted writes must
// use the JWT token returned here as `apiKey` for subsequent calls.
//
// Throws on login failure.
export async function jwtLogin(cfg: Config): Promise<string> {
  const body = JSON.stringify({ email: cfg.email, password: cfg.password });
  const [status, content] = await rawDo("POST", `${cfg.apiUrl}/api/v1/auth/login`, "", body);
  if (status !== 200) {
    throw new Error(`jwt login failed: status=${status} body=${content.slice(0, 200).toString()}`);
  }
  const obj = JSON.parse(content.toString());
  if (!obj.token) {
    throw new Error(`jwt login: no token in response: ${JSON.stringify(obj)}`);
  }
  return obj.token;
}

/**
 * Concatenated text of a contract Message (pkg/session shape) — the
 * convenience view for assertions that don't render parts. Mirrors
 * Go's Message.TextContent() and Python's message_text().
 */
export function msgText(msg: { text?: string; parts?: Array<{ type: string; text?: string }> } | null | undefined): string {
  if (!msg) return "";
  if (msg.text) return msg.text;
  return (msg.parts ?? []).filter(p => p.type === 'text').map(p => p.text ?? '').join('');
}
