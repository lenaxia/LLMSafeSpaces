#!/usr/bin/env node
// Lifecycle enumeration: after a tiny prompt, drive session/list, set_config_option, set_model, fork, load, resume.
import { spawn } from "node:child_process";
import { openSync, writeSync, closeSync, mkdirSync } from "node:fs";

const BIN = "/opencode/usr/local/bin/opencode";
const label = "lifecycle";
const CWD = `/tmp/opencode/acpspike/pg-${label}`;
mkdirSync(CWD, { recursive: true });
const out = openSync(`/tmp/opencode/acpspike/transcript-${label}.ndjson`, "w");
const t0 = Date.now();
const log = (dir, obj) => writeSync(out, JSON.stringify({ t: ((Date.now() - t0) / 1000).toFixed(2), dir, ...obj }) + "\n");

const proc = spawn(BIN, ["acp", "--pure", "--cwd", CWD], { stdio: ["pipe", "pipe", "ignore"] });
let nextId = 1; const pending = new Map(); let buf = "";
const plans = [];
proc.stdout.on("data", (chunk) => {
  buf += chunk.toString(); let i;
  while ((i = buf.indexOf("\n")) >= 0) {
    const line = buf.slice(0, i).trim(); buf = buf.slice(i + 1);
    if (!line) continue;
    let msg; try { msg = JSON.parse(line); } catch { continue; }
    if (msg.id !== undefined && (msg.result !== undefined || msg.error !== undefined)) {
      const p = pending.get(msg.id);
      log("agent->client RESPONSE", { id: msg.id, method: p?.method, result: msg.result, error: msg.error });
      if (p) { pending.delete(msg.id); p.resolve(msg); }
    } else if (msg.method) {
      if (msg.params?.update?.sessionUpdate === "plan") plans.push(msg.params.update);
      if (msg.method === "session/request_permission") {
        const opt = (msg.params.options ?? []).find(o => /once/i.test(o.kind ?? "")) ?? msg.params.options?.[0];
        writeSync(proc.stdin, JSON.stringify({ jsonrpc: "2.0", id: msg.id, result: { outcome: { outcome: "selected", optionId: opt?.optionId ?? "once" } } }) + "\n");
        log("client->agent RESPONSE", { id: msg.id, method: "session/request_permission", result: { outcome: { outcome: "selected", optionId: opt?.optionId ?? "once" } } });
      } else log("agent->client NOTIFICATION", msg);
    }
  }
});
function send(method, params) {
  const id = nextId++;
  proc.stdin.write(JSON.stringify({ jsonrpc: "2.0", id, method, params }) + "\n");
  log("client->agent REQUEST", { jsonrpc: "2.0", id, method, params });
  return new Promise((resolve) => pending.set(id, { resolve, method }));
}
const main = async () => {
  await send("initialize", { protocolVersion: 1, clientCapabilities: { fs: { readTextFile: false, writeTextFile: false } }, clientInfo: { name: "acp-spike-driver", version: "0.0.1" } });
  const ns = await send("session/new", { cwd: CWD, mcpServers: [] });
  const sessionId = ns.result?.sessionId;
  await send("session/prompt", { sessionId, prompt: [{ type: "text", text: "Use your todo tool to create a one-item todo list: item 'spike' pending. Then reply ok." }] });
  const lst = await send("session/list", { limit: 10 });
  const opt = ns.result?.configOptions?.[0];
  if (opt && opt.options?.length) {
    const other = opt.options.find(o => o.value !== opt.currentValue) ?? opt.options[0];
    await send("session/set_config_option", { sessionId, configId: opt.id, value: other.value });
    await send("session/set_model", { sessionId, modelId: opt.currentValue });
  }
  const fk = await send("session/fork", { sessionId, title: "spike-fork", updatedAt: new Date().toISOString() });
  const forkId = fk.result?.sessionId ?? ("fork-err:" + JSON.stringify(fk.error ?? {}).slice(0, 120));
  if (fk.result?.sessionId) await send("session/load", { sessionId: fk.result.sessionId, cwd: CWD, mcpServers: [], latest: true });
  log("PLANS-CAPTURED", { count: plans.length, plans });
  log("FORK-ID", { forkId });
  await send("session/close", { sessionId });
  proc.kill(); closeSync(out); setTimeout(() => process.exit(0), 150);
};
main().catch((e) => { log("DRIVER-ERROR", { message: String(e) }); proc.kill(); process.exit(1); });
setTimeout(() => { log("DRIVER-TIMEOUT", {}); proc.kill(); process.exit(2); }, 240000);
