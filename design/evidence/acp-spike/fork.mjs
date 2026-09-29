import { spawn } from "node:child_process";
import { openSync, writeSync, mkdirSync } from "node:fs";
const BIN = "/opencode/usr/local/bin/opencode";
const CWD = "/tmp/opencode/acpspike/pg-fork"; mkdirSync(CWD, { recursive: true });
const out = openSync("/tmp/opencode/acpspike/transcript-fork.ndjson", "w");
const t0 = Date.now();
const log = (dir, obj) => { const l = JSON.stringify({ t: ((Date.now() - t0) / 1000).toFixed(2), dir, ...obj }); writeSync(out, l + "\n"); console.error(l.slice(0, 240)); };
const proc = spawn(BIN, ["acp", "--pure", "--cwd", CWD], { stdio: ["pipe", "pipe", "ignore"] });
let nextId = 1; const pending = new Map(); let buf = "";
proc.stdout.on("data", (c) => { buf += c.toString(); let i; while ((i = buf.indexOf("\n")) >= 0) { const line = buf.slice(0, i).trim(); buf = buf.slice(i + 1); if (!line) continue; let m; try { m = JSON.parse(line) } catch { continue }
  if (m.id !== undefined && (m.result !== undefined || m.error !== undefined)) { const p = pending.get(m.id); log("RESPONSE", { id: m.id, method: p?.method, result: m.result, error: m.error }); if (p) { pending.delete(m.id); p.resolve(m) } }
  else if (m.method && m.id !== undefined) { log("AGENT-REQUEST", { method: m.method, params: m.params }); proc.stdin.write(JSON.stringify({ jsonrpc: "2.0", id: m.id, result: {} }) + "\n") }
  else if (m.method) { log("NTF", { method: m.method, params: m.params }) } } });
function send(method, params) { const id = nextId++; proc.stdin.write(JSON.stringify({ jsonrpc: "2.0", id, method, params }) + "\n"); log("REQUEST", { id, method, params }); return new Promise(r => pending.set(id, { resolve: r, method })) }
const main = async () => {
  await send("initialize", { protocolVersion: 1, clientCapabilities: { fs: { readTextFile: false, writeTextFile: false } }, clientInfo: { name: "spike", version: "0" } });
  const ns = await send("session/new", { cwd: CWD, mcpServers: [] }); const sid = ns.result?.sessionId;
  await send("session/prompt", { sessionId: sid, prompt: [{ type: "text", text: "Reply ok" }] });
  const fk = await send("session/fork", { sessionId: sid, cwd: CWD, title: "fork-spike", updatedAt: new Date().toISOString() });
  const fid = fk.result?.sessionId;
  log("FORK-RESULT", { fid, error: fk.error });
  if (fid) { const ld = await send("session/load", { sessionId: fid, cwd: CWD, mcpServers: [], latest: true }); log("LOAD-DONE", { ok: !ld.error, result: ld.result ? JSON.stringify(ld.result).slice(0, 200) : null }) }
  proc.kill(); process.exit(0) };
main().catch(e => { log("ERR", { e: String(e) }); proc.kill(); process.exit(1) });
setTimeout(() => { log("TIMEOUT", {}); proc.kill(); process.exit(2) }, 180000);
