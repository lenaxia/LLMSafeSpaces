// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later
// D-TERMINAL canary — TypeScript SDK

import { LLMSafeSpaces } from '../../../typescript/src/index.js';
import { Runner, Config, configFromEnv, nodeFetch, waitActive } from '../canary.js';

async function run(r: Runner, cfg: Config): Promise<void> {
  const c = new LLMSafeSpaces({ baseUrl: cfg.apiUrl, apiKey: cfg.apiKey, timeout: 60000, fetch: nodeFetch as any });
  let wsId: string | null = null;
  try {
    const [ok, ws] = await r.assertNoError(
      () => c.workspaces.create({ name: 'canary-ts-terminal', runtime: 'base', storageSize: '1Gi' }),
      'create: no error');
    if (!ok || !ws) return;
    wsId = ws.id;

    const phase = await waitActive(c, wsId);
    r.assert(phase === 'Active', 'reach-active', `got "${phase}"`);
    if (phase !== 'Active') return;

    const [ok2, ticket1] = await r.assertNoError(
      () => c.terminal.getTicket(wsId!), 'get-ticket-1: no error');
    if (ok2 && ticket1) {
      r.assert(ticket1.ticket.startsWith('tkt_'), 'ticket-1: starts with tkt_', ticket1.ticket);
      r.assert(ticket1.ticket.length > 10, 'ticket-1: length > 10', String(ticket1.ticket.length));
      r.assert(ticket1.expiresAt.length > 0, 'ticket-1: expiresAt non-empty');
    }

    const [ok3, ticket2] = await r.assertNoError(
      () => c.terminal.getTicket(wsId!), 'get-ticket-2: no error');
    if (ok3 && ticket2 && ok2 && ticket1) {
      r.assert(ticket2.ticket !== ticket1.ticket, 'ticket-uniqueness: different tickets');
      r.assert(ticket2.ticket.startsWith('tkt_'), 'ticket-2: starts with tkt_');
    }

    await r.assertError(
      () => c.terminal.getTicket('00000000-0000-0000-0000-000000000000'),
      'ticket-nonexistent-ws: error');

  } finally {
    if (wsId) { try { await c.workspaces.delete(wsId); } catch {} }
  }
}

async function main() {
  const r = new Runner('terminal');
  await run(r, configFromEnv());
  r.print();
  process.exit(r.exitCode());
}
main().catch(e => { console.error(e); process.exit(1); });
