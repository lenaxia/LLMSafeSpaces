#!/usr/bin/env node
// Silent-ignore capture: respond to permission request #1 with the WRONG shape (optionIds array),
// watch the stall, cancel the prompt, re-prompt, respond correctly -> proceeds. One transcript proving both.
import { spawn } from "node:child_process";
import { openSync, writeSync, closeSync, mkdirSync } from "node:fs";
const BIN = "/opencode/usr/local/bin/opencode";
const CWD = "/tmp/opencode/acpspike/pg-silentignore";
mkdirSync(CWD, { recursive: true });
writeSync(openSync(CWD + "/opencode.json", "w"), JSON.stringify({ permission: { edit: "ask", bash: { "*": "ask" } } }, null, 2));
const out = openSync("/tmp/opencode/acpspike/transcript-silentignore.ndjson", "w");
const t0 = Date.now();
const log = (dir, obj) => writeSync(out, JSON.stringify({ t: ((Date.now() - t0) / 1000).toFixed(2), dir, ...obj }) + "\n");
const proc = spawn(BIN, ["acp", "--pure", "--cwd", CWD], { stdio: ["pipe", "pipe", "ignore"] });
let nextId = 1; const pending = new Map(); let buf = ""; let permCount = 0; let stallTimer = null;
proc.stdout.on("data", (chunk) => {
  buf += chunk.toString(); let i;
  while ((i = buf.indexOf("\n")) >= 0) {
    const line = buf.slice(0, i).trim(); buf = buf.slice(i + 1);
    if (!line) continue;
    let msg; try { msg = JSON.parse(line) } catch { continue }
    if (msg.id !== undefined && (msg.result !== undefined || msg.error !== undefined)) {
      const p = pending.get(msg.id);
      log("agent->client RESPONSE", { id: msg.id, method: p?.method, result: msg.result, error: msg.error });
      if (p) { pending.delete(msg.id); p.resolve(msg); }
    } else if (msg.method && msg.id !== undefined) {
      log("agent->client REQUEST", msg);
      if (msg.method === "session/request_permission") {
        permCount++;
        if (permCount === 1) {
          // WRONG shape (public draft): optionIds array. Expect silent ignore -> stall.
          const wrong = { update: { outcome: "selected", optionIds: ["once"] } };
          log("client->agent RESPONSE", { jsonrpc: "2.0", id: msg.id, method: "session/request_permission", note: "WRONG SHAPE (draft optionIds array)", result: wrong });
          proc.stdin.write(JSON.stringify({ jsonrpc: "2.0", id: msg.id, result: wrong }) + "\n");
          const sentAt = Date.now();
          stallTimer = setInterval(() => {
            log("STALL-PROBE", { quietSeconds: ((Date.now() - sentAt) / 1000).toFixed(1), permCount });
            if (Date.now() - sentAt > 25000) { clearInterval(stallTimer); log("STALL-CONFIRMED", { quietSeconds: ((Date.now() - sentAt) / 1000).toFixed(1) }); }
          }, 5000);
        } else {
          const opt = (msg.params.options ?? []).find(o => /once/i.test(o.kind ?? "")) ?? msg.params.options?.[0];
          const right = { outcome: { outcome: "selected", optionId: opt?.optionId ?? "once" } };
          log("client->agent RESPONSE", { jsonrpc: "2.0", id: msg.id, method: "session/request_permission", note: "CORRECT SHAPE (singular optionId)", result: right });
          proc.stdin.write(JSON.stringify({ jsonrpc: "2.0", id: msg.id, result: right }) + "\n");
        }
      } else {
        proc.stdin.write(JSON.stringify({ jsonrpc: "2.0", id: msg.id, result: {} }) + "\n");
        log("client->agent RESPONSE", { jsonrpc: "2.0", id: msg.id, method: msg.method, result: {}, note: "generic ack" });
      }
    } else if (msg.method) log("agent->client NOTIFICATION", msg);
  }
});
function send(method, params) {
  const id = nextId++;
  log("client->agent REQUEST", { jsonrpc: "2.0", id, method, params });
  proc.stdin.write(JSON.stringify({ jsonrpc: "2.0", id, method, params }) + "\n");
  return new Promise(r => pending.set(id, { resolve: r, method }));
}
const main = async () => {
  await send("initialize", { protocolVersion: 1, clientCapabilities: { fs: { readTextFile: false, writeTextFile: false } }, clientInfo: { name: "acp-spike-driver", version: "0.0.1" } });
  const ns = await send("session/new", { cwd: CWD, mcpServers: [] });
  const sid = ns.result.sessionId;
  // Fire prompt, do NOT await completion (it will stall on the ignored permission response).
  send("session/prompt", { sessionId: sid, prompt: [{ type: "text", text: "Use the bash tool to run exactly: echo silent-ignore-probe. Then reply with its output." }] });
  // After 30s of stall: cancel, re-prompt, second permission request answered correctly.
  setTimeout(async () => {
    if (stallTimer) clearInterval(stallTimer);
    // session/cancel is a NOTIFICATION (request form returns -32601, observed live in the prior run).
    log("client->agent NOTIFICATION", { jsonrpc: "2.0", method: "session/cancel", params: { sessionId: sid }, note: "cancel as notification" });
    proc.stdin.write(JSON.stringify({ jsonrpc: "2.0", method: "session/cancel", params: { sessionId: sid } }) + "\n");
    await new Promise(r => setTimeout(r, 3000));
    const pr = await send("session/prompt", { sessionId: sid, prompt: [{ type: "text", text: "Use the bash tool to run exactly: echo silent-ignore-probe. Then reply with its output." }] });
    log("SECOND-PROMPT-STOP", { stopReason: pr.result?.stopReason, permCount });
    await send("session/close", { sessionId: sid });
    proc.kill(); closeSync(out); setTimeout(() => process.exit(0), 200);
  }, 30000);
};
main().catch(e => { log("DRIVER-ERROR", { message: String(e) }); proc.kill(); process.exit(1); });
setTimeout(() => { log("DRIVER-TIMEOUT", {}); proc.kill(); process.exit(2); }, 240000);
