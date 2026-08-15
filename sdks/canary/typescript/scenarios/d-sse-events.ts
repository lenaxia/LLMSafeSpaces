// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later
// D-SSE-EVENTS canary — TypeScript SDK

import http from 'http';
import { LLMSafeSpaces } from '../../../typescript/src/index.js';
import { Runner, Config, configFromEnv, nodeFetch, rawDo, waitActive, sleep } from '../canary.js';

async function run(r: Runner, cfg: Config): Promise<void> {
  const c = new LLMSafeSpaces({ baseUrl: cfg.apiUrl, apiKey: cfg.apiKey, timeout: 60000, fetch: nodeFetch as any });
  let wsId: string | null = null;
  try {
    const [ok, ws] = await r.assertNoError(
      () => c.workspaces.create({ name: 'canary-ts-sse', runtime: 'base', storageSize: '1Gi' }),
      'create: no error');
    if (!ok || !ws) return;
    wsId = ws.id;

    const phase = await waitActive(c, wsId);
    r.assert(phase === 'Active', 'reach-active', `got "${phase}"`);
    if (phase !== 'Active') return;

    // P1: SSE responds with correct headers
    const [sseStatus, sseCT] = await sseHead(cfg, wsId!);
    r.assert(sseStatus === 200, `sse-connect: 200 (got ${sseStatus})`);
    r.assert(sseCT.includes('text/event-stream'), 'sse-connect: content-type', sseCT);

    // Start collecting events in background
    const events: any[] = [];
    const stopCollecting = collectSSEEvents(cfg, wsId!, events);

    await sleep(500); // let connection settle

    // P2+P3: Suspend triggers workspace.phase event
    await r.assertNoError(() => c.workspaces.suspend(wsId!), 'suspend: no error');
    const phaseReceived = await waitForEventMatching(events, e =>
      e.type === 'workspace.phase' && (e.phase === 'Suspending' || e.phase === 'Suspended'),
      30000);
    r.assert(phaseReceived, 'sse: workspace.phase event on suspend');

    // P4: Activate triggers another phase event
    const prevCount = events.filter(e => e.type === 'workspace.phase').length;
    await r.assertNoError(() => c.workspaces.activate(wsId!), 'activate: no error');
    const resumeReceived = await waitForEventMatching(events, e =>
      e.type === 'workspace.phase' && events.filter(x => x.type === 'workspace.phase').length > prevCount,
      60000);
    r.assert(resumeReceived, 'sse: workspace.phase event on resume');

    stopCollecting();

    // N1: SSE on nonexistent workspace → 404
    const [sn] = await rawDo('GET', `${cfg.apiUrl}/api/v1/workspaces/00000000-0000-0000-0000-000000000000/events`,
      cfg.apiKey);
    r.assert(sn === 404, `sse-nonexistent: 404 (got ${sn})`);

  } finally {
    if (wsId) { try { await c.workspaces.delete(wsId); } catch {} }
  }
}

function sseHead(cfg: Config, wsId: string): Promise<[number, string]> {
  return new Promise(resolve => {
    const u = new URL(`${cfg.apiUrl}/api/v1/workspaces/${wsId}/events`);
    const req = http.get({
      hostname: u.hostname, port: u.port || 80, path: u.pathname,
      headers: { Authorization: `Bearer ${cfg.apiKey}`, Accept: 'text/event-stream' },
    }, res => {
      const ct = res.headers['content-type'] ?? '';
      resolve([res.statusCode ?? 0, ct]);
      req.destroy();
    });
    req.on('error', () => resolve([0, '']));
    setTimeout(() => { req.destroy(); resolve([0, '']); }, 5000);
  });
}

function collectSSEEvents(cfg: Config, wsId: string, events: any[]): () => void {
  const u = new URL(`${cfg.apiUrl}/api/v1/workspaces/${wsId}/events`);
  const req = http.get({
    hostname: u.hostname, port: u.port || 80, path: u.pathname,
    headers: { Authorization: `Bearer ${cfg.apiKey}`, Accept: 'text/event-stream' },
  }, res => {
    let buf = '';
    res.on('data', (chunk: Buffer) => {
      buf += chunk.toString();
      const lines = buf.split('\n');
      buf = lines.pop() ?? '';
      for (const line of lines) {
        if (!line.startsWith('data: ')) continue;
        try { events.push(JSON.parse(line.slice(6))); } catch {}
      }
    });
  });
  req.on('error', () => {});
  return () => req.destroy();
}

function waitForEventMatching(events: any[], test: (e: any) => boolean, timeoutMs: number): Promise<boolean> {
  return new Promise(resolve => {
    const deadline = Date.now() + timeoutMs;
    const check = () => {
      if (events.some(test)) { resolve(true); return; }
      if (Date.now() > deadline) { resolve(false); return; }
      setTimeout(check, 500);
    };
    check();
  });
}

async function main() {
  const r = new Runner('sse-events');
  await run(r, configFromEnv());
  r.print();
  process.exit(r.exitCode());
}
main().catch(e => { console.error(e); process.exit(1); });
