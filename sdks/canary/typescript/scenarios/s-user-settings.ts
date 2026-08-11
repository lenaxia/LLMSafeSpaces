// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later
// S-USER-SETTINGS canary — TypeScript SDK

import { LLMSafeSpaces } from '../../src/index.js';
import { Runner, Config, configFromEnv, nodeFetch, rawDo } from '../canary.js';

const EXPECTED_SCHEMA_VERSION = 10;

async function run(r: Runner, cfg: Config): Promise<void> {
  const c = new LLMSafeSpaces({ baseUrl: cfg.apiUrl, apiKey: cfg.apiKey, timeout: 15000, fetch: nodeFetch as any });

  // P1: GET settings
  const [ok, settings] = await r.assertNoError(() => c.userSettings.get(), 'get-settings: no error');
  if (ok && settings) {
    r.assert(settings.settings !== null, 'get-settings: settings object present');
    r.assert(settings.schemaVersion > 0, 'get-settings: schemaVersion > 0', String(settings.schemaVersion));
  }

  // P2+P3: GET schema + drift detection
  const [ok2, schema] = await r.assertNoError(() => c.userSettings.getSchema(), 'get-schema: no error');
  if (ok2 && schema) {
    r.assert(schema.schemaVersion === EXPECTED_SCHEMA_VERSION,
      `schema-version: equals ${EXPECTED_SCHEMA_VERSION}`,
      `got ${schema.schemaVersion} — SCHEMA DRIFT DETECTED`);
    r.assert(Array.isArray(schema.settings), 'get-schema: settings array');
  }

  // P5–P7: SET and verify round-trip
  const [ok3, res] = await r.assertNoError(() => c.userSettings.set('theme', 'dark'), 'set-theme: no error');
  if (ok3 && res) {
    r.assert(res.key === 'theme', 'set-theme: key field');
    r.assert(res.value === 'dark', 'set-theme: value field');
  }
  const [ok4, settings2] = await r.assertNoError(() => c.userSettings.get(), 'get-after-set: no error');
  if (ok4 && settings2) r.assert(settings2.settings['theme'] === 'dark', 'get-after-set: theme=dark');
  await c.userSettings.set('theme', 'system').catch(() => {});

  // N1: no auth → 401
  const [s] = await rawDo('GET', cfg.apiUrl + '/api/v1/users/me/settings');
  r.assert(s === 401, `no-auth: 401 (got ${s})`);

  // N2: missing value → 400
  const [s2] = await rawDo('PUT', cfg.apiUrl + '/api/v1/users/me/settings/theme', cfg.apiKey, Buffer.from('{}'));
  r.assert(s2 === 400, `missing-value: 400 (got ${s2})`);

  // N3: unknown key → 400
  const [s3] = await rawDo('PUT', cfg.apiUrl + '/api/v1/users/me/settings/nonexistent.key.xyz',
    cfg.apiKey, Buffer.from('{"value":"test"}'));
  r.assert(s3 === 400, `unknown-key: 400 (got ${s3})`);
}

async function main() {
  const r = new Runner('user-settings');
  await run(r, configFromEnv());
  r.print();
  process.exit(r.exitCode());
}
main().catch(e => { console.error(e); process.exit(1); });
