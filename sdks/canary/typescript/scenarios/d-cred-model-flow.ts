// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later
// D-CRED-MODEL-FLOW canary — TypeScript SDK (flagship end-to-end scenario)

import { LLMSafeSpaces } from '../../../typescript/src/index.js';
import { Runner, Config, configFromEnv, nodeFetch, waitActive, ensureSessionWithRetry } from '../canary.js';

async function run(run: Runner, cfg: Config): Promise<void> {
  if (!cfg.llmApiKey || !cfg.llmModel) {
    run.ok('cred-model-flow: skipped (no LLM API key or model)');
    return;
  }

  const jwtAvailable = cfg.email !== '' && cfg.password !== '';
  if (!jwtAvailable) {
    run.ok('cred-model-flow: JWT credentials not set — agent tests will be skipped (only API surface tested)');
  }

  const clientOpts: any = { baseUrl: cfg.apiUrl, timeout: 120000, fetch: nodeFetch as any };
  if (jwtAvailable) {
    clientOpts.credentials = { email: cfg.email, password: cfg.password };
  } else {
    clientOpts.apiKey = cfg.apiKey;
  }
  const c = new LLMSafeSpaces(clientOpts);
  let wsId: string | null = null;
  let credId: string | null = null;

  try {
    // Step 1: Create workspace, wait Active
    const [ok, ws] = await run.assertNoError(
      () => c.workspaces.create({ name: 'canary-ts-flow', runtime: 'base', storageSize: '1Gi' }),
      'create-ws: no error',
    );
    if (!ok || !ws) return;
    wsId = ws.id;

    const phase = await waitActive(c, wsId);
    run.assert(phase === 'Active', 'reach-active', `got "${phase}"`);
    if (phase !== 'Active') return;

    // Step 2: Create LLM credential
    const credValue = JSON.stringify({ kind: cfg.llmProvider, slug: "canary-ts-model-flow", apiKey: cfg.llmApiKey });
    const [ok2, cred] = await run.assertNoError(
      () => c.secrets.create({ name: 'canary-ts-flow-cred', type: 'llm-provider', value: credValue }),
      'create-cred: no error',
    );
    if (!ok2 || !cred) return;
    run.assert(cred.type === 'llm-provider', 'create-cred: type=llm-provider', cred.type);
    credId = cred.id;

    await run.assertNoError(() => c.workspaces.setBindings(wsId!, [credId!]), 'bind-cred: no error');

    await run.assertNoError(() => c.workspaces.setModel(wsId!, cfg.llmModel), 'set-model: no error');

    if (!jwtAvailable) {
      run.ok('agent-tests: skipped (JWT required for DEK-based secret injection)');
      return;
    }

    // Step 5: Ensure session
    let sess: any;
    try {
      sess = await ensureSessionWithRetry(c, wsId, 5);
      run.ok('ensure-session: no error');
    } catch (e: any) {
      run.fail('ensure-session: no error', e.message);
      return;
    }

    // Step 6: Send message
    const [ok3, msg] = await run.assertNoError(
      () => c.sessions.sendMessage(wsId!, sess.sessionId, 'Reply with exactly: CRED-FLOW-OK'),
      'send-message: no error',
    );
    if (ok3 && msg) {
      run.assert(msg.content.length > 0, 'send-message: non-empty content');
      run.assert(msg.content.toUpperCase().includes('CRED-FLOW-OK'), 'send-message: contains expected text',
        msg.content.substring(0, 100));
    }

    // Step 7: History
    const [ok4, hist] = await run.assertNoError(
      () => c.sessions.getHistory(wsId!, sess.sessionId),
      'history: no error',
    );
    if (ok4 && hist) run.assert(hist.length >= 1, 'history: ≥1 entry', String(hist.length));

    // Step 8: Second session
    let sess2: any;
    try {
      sess2 = await c.sessions.ensure(wsId);
      run.ok('ensure-session-2: no error');
    } catch (e: any) {
      run.fail('ensure-session-2: no error', e.message);
      return;
    }

    // Step 9: Send to second session
    const [ok5, msg2] = await run.assertNoError(
      () => c.sessions.sendMessage(wsId!, sess2.sessionId, 'Reply with exactly: AFTER-RELOAD'),
      'send-message-2: no error',
    );
    if (ok5 && msg2) {
      run.assert(msg2.content.length > 0, 'send-message-2: non-empty');
      run.assert(msg2.content.toUpperCase().includes('AFTER-RELOAD'), 'send-message-2: contains expected text',
        msg2.content.substring(0, 100));
    }

    // Step 10: Delete credential
    await run.assertNoError(() => c.secrets.delete(credId!), 'delete-cred: no error');
    credId = null;

  } finally {
    if (credId) { try { await c.secrets.delete(credId); } catch {} }
    if (wsId) { try { await c.workspaces.delete(wsId); } catch {} }
  }
}

async function main() {
  const r = new Runner('cred-model-flow');
  const cfg = configFromEnv();
  await run(r, cfg);
  r.print();
  process.exit(r.exitCode());
}

main().catch(e => { console.error(e); process.exit(1); });
