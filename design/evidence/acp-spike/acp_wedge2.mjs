#!/usr/bin/env node
// Wedge-queued-prompt capture: wrong-shape consent -> stall -> send a SECOND prompt DURING the stall
// (the r2 unevidenced sub-claim) -> observe evaluation or silence -> notification-cancel -> observe resolution.
import { spawn } from "node:child_process";
import { openSync, writeSync, closeSync, mkdirSync } from "node:fs";
const BIN = "/opencode/usr/local/bin/opencode";
const CWD = "/tmp/opencode/acpspike/pg-wedge2";
mkdirSync(CWD, { recursive: true });
writeSync(openSync(CWD + "/opencode.json", "w"), JSON.stringify({ permission: { edit: "ask", bash: { "*": "ask" } } }, null, 2));
const out = openSync("/tmp/opencode/acpspike/transcript-wedge2.ndjson", "w");
const t0 = Date.now();
const log = (dir, obj) => writeSync(out, JSON.stringify({ t: ((Date.now() - t0) / 1000).toFixed(2), dir, ...obj }) + "\n");
const proc = spawn(BIN, ["acp", "--pure", "--cwd", CWD], { stdio: ["pipe", "pipe", "ignore"] });
let nextId = 1; const pending = new Map(); let buf = ""; let permCount = 0;
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
          const wrong = { update: { outcome: "selected", optionIds: ["once"] } }; // draft-spec shape: WRONG
          log("client->agent RESPONSE", { jsonrpc: "2.0", id: msg.id, method: "session/request_permission", note: "WRONG SHAPE (draft optionIds array)", result: wrong });
          proc.stdin.write(JSON.stringify({ jsonrpc: "2.0", id: msg.id, result: wrong }) + "\n");
        } else {
          const opt = (msg.params.options ?? []).find(o => /once/i.test(o.kind ?? "")) ?? msg.params.options?.[0];
          const right = { outcome: { outcome: "selected", optionId: opt?.optionId ?? "once" } };
          log("client->agent RESPONSE", { jsonrpc: "2.0", id: msg.id, method: "session/request_permission", note: "CORRECT SHAPE", result: right });
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
  const P1 = "Use the bash tool to run exactly: echo wedge2-first. Then reply with its output.";
  const P2 = "Reply with the single word: second";
  send("session/prompt", { sessionId: sid, prompt: [{ type: "text", text: P1 }] }); // id N: will wedge
  // 12s in (stall confirmed): queue the second prompt DURING the wedge.
  setTimeout(() => { log("WEDGE-PROBE", { note: "12s into stall; queueing second prompt now" }); send("session/prompt", { sessionId: sid, prompt: [{ type: "text", text: P2 }] }); }, 12000);
  // 30s in: notification cancel; then watch what resolves.
  setTimeout(() => {
    log("client->agent NOTIFICATION", { jsonrpc: "2.0", method: "session/cancel", params: { sessionId: sid }, note: "cancel as notification" });
    proc.stdin.write(JSON.stringify({ jsonrpc: "2.0", method: "session/cancel", params: { sessionId: sid } }) + "\n");
  }, 30000);
  // 60s in: third prompt (post-cancel) to test session health.
  setTimeout(async () => {
    const pr = await send("session/prompt", { sessionId: sid, prompt: [{ type: "text", text: "Reply with the single word: third" }] });
    log("THIRD-PROMPT-STOP", { stopReason: pr.result?.stopReason, permCount });
    await send("session/close", { sessionId: sid });
    proc.kill(); closeSync(out); setTimeout(() => process.exit(0), 200);
  }, 60000);
};
main().catch(e => { log("DRIVER-ERROR", { message: String(e) }); proc.kill(); process.exit(1); });
setTimeout(() => { log("DRIVER-TIMEOUT", {}); proc.kill(); process.exit(2); }, 240000);
