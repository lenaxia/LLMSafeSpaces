#!/usr/bin/env node
// ACP spike driver: newline-delimited JSON-RPC over stdio against the pinned opencode 1.18.15.
// Usage: node acp_drive.mjs <label> <prompt-text> [timeout-ms]
// Writes: transcript-<label>.ndjson (every message both directions, with dir/ts tags)
import { spawn } from "node:child_process";
import { openSync, writeSync, closeSync, mkdirSync } from "node:fs";

const BIN = "/opencode/usr/local/bin/opencode";
const label = process.argv[2] ?? "run";
const promptText = process.argv[3] ?? "Reply with the single word: pong";
const timeoutMs = Number(process.argv[4] ?? 120000);
const CWD = `/tmp/opencode/acpspike/pg-${label}`;
mkdirSync(CWD, { recursive: true });

const out = openSync(`/tmp/opencode/acpspike/transcript-${label}.ndjson`, "w");
const t0 = Date.now();
let log = (dir, obj) => {
  const line = JSON.stringify({ t: ((Date.now() - t0) / 1000).toFixed(2), dir, ...obj });
  writeSync(out, line + "\n");
  if (process.env.SPIKE_VERBOSE) console.error(line.slice(0, 400));
};

const proc = spawn(BIN, ["acp", "--pure", "--cwd", CWD], { stdio: ["pipe", "pipe", "pipe"] });
proc.stderr.on("data", (d) => { /* stderr kept out of transcript; peek with SPIKE_PEEK */ if (process.env.SPIKE_PEEK) process.stderr.write(d); });

let nextId = 1;
const pending = new Map(); // id -> {resolve, method}
const seen = { notifications: {}, requestsFromAgent: {} };
let lastMessageAt = Date.now();
let outcomeAttempt = 0;
const stallTimers = [];

proc.stdout.on("data", (chunk) => {
  lastMessageAt = Date.now();
  buf += chunk.toString();
  let i;
  while ((i = buf.indexOf("\n")) >= 0) {
    const line = buf.slice(0, i).trim();
    buf = buf.slice(i + 1);
    if (!line) continue;
    let msg;
    try { msg = JSON.parse(line); } catch (e) { log({ dir: "PARSE_ERROR", line }); continue; }
    if (msg.id !== undefined && (msg.result !== undefined || msg.error !== undefined)) {
      const p = pending.get(msg.id);
      log("agent->client RESPONSE", { id: msg.id, method: p?.method, result: msg.result, error: msg.error });
      if (p) { pending.delete(msg.id); p.resolve(msg); }
    } else if (msg.method && msg.id !== undefined) {
      // request from agent (e.g. session/request_permission)
      seen.requestsFromAgent[msg.method] = (seen.requestsFromAgent[msg.method] ?? 0) + 1;
      log("agent->client REQUEST", msg);
      handleAgentRequest(msg);
    } else if (msg.method) {
      const key = msg.method + ":" + (msg.params?.update?.sessionUpdate ?? msg.params?.update?.type ?? "");
      seen.notifications[msg.method] = (seen.notifications[msg.method] ?? 0) + 1;
      log("agent->client NOTIFICATION", msg);
    } else {
      log("agent->client ???", msg);
    }
  }
});

let buf = "";
function send(method, params) {
  const id = nextId++;
  const msg = { jsonrpc: "2.0", id, method, params };
  log("client->agent REQUEST", msg);
  proc.stdin.write(JSON.stringify(msg) + "\n");
  return new Promise((resolve) => pending.set(id, { resolve, method }));
}
function notify(method, params) {
  const msg = { jsonrpc: "2.0", method, params };
  log("client->agent NOTIFICATION", msg);
  proc.stdin.write(JSON.stringify(msg) + "\n");
}
function respond(id, result) {
  const msg = { jsonrpc: "2.0", id, result };
  log("client->agent RESPONSE", msg);
  proc.stdin.write(JSON.stringify(msg) + "\n");
}

function handleAgentRequest(msg) {
  if (msg.method === "session/request_permission" || msg.method === "permission/request" || msg.method === "sessionRequest") {
    // Auto-allow: pick the first option whose id suggests once/allow; prefer 'once' to avoid sticky state.
    const opts = msg.params?.options ?? [];
    const titles = opts.map(o => ({ id: o.id ?? o.optionId, title: o.title ?? o.kind }));
    log("PERMISSION-REQUEST-SHAPE", { method: msg.method, options: titles, update: msg.params });
    const once = opts.find(o => /once/i.test(JSON.stringify(o)));
    const chosen = once ?? opts[0];
    const optionId = chosen?.id ?? chosen?.optionId ?? "once";
    // Verified from the bundled zod (L$/C$/j$): result = {outcome:{outcome:"selected",optionId:<singular string>}}; "cancelled" is double-l.
    respond(msg.id, { outcome: { outcome: "selected", optionId } });
    return;
  }
  if (msg.method === "session/consent_request") { respond(msg.id, { consent: true }); return; }
  // fs mediation declined by client capability: shouldn't be asked, but answer read-only-safe if it happens
  if (msg.method === "fs/read_text_file") { respond(msg.id, { content: "" }); return; }
  if (msg.method === "fs/write_text_file") { respond(msg.id, {}); return; }
  log("UNHANDLED-AGENT-REQUEST", msg);
  respond(msg.id, {}); // generic ack; logged loudly above
}

const PROTOCOL_VERSIONS = Number(process.env.SPIKE_PROTO ?? 1);
async function main() {
  const init = await send("initialize", {
    protocolVersion: PROTOCOL_VERSIONS,
    clientCapabilities: { fs: { readTextFile: false, writeTextFile: false } },
    clientInfo: { name: "acp-spike-driver", version: "0.0.1" },
  });
  const ns = await send("session/new", { cwd: CWD, mcpServers: [] });
  const sessionId = ns.result?.sessionId;
  log("SESSION-ID", { sessionId, firstUpdate: ns.result?.update });
  const pr = await send("session/prompt", { sessionId, prompt: [{ type: "text", text: promptText }] });
  log("PROMPT-STOP", { stopReason: pr.result?.stopReason, lastUpdate: pr.result?.update });
  console.error("DONE", JSON.stringify({ sessionId, stopReason: pr.result?.stopReason }));
  try { await send("session/close", { sessionId }); } catch {}
  proc.kill();
  writeSync(out, JSON.stringify({ SUMMARY: seen }) + "\n");
  closeSync(out);
  setTimeout(() => process.exit(0), 200);
}
main().catch((e) => { log("DRIVER-ERROR", { message: String(e) }); proc.kill(); process.exit(1); });
setTimeout(() => { log("DRIVER-TIMEOUT", {}); writeSync(out, JSON.stringify({ SUMMARY: seen }) + "\n"); proc.kill(); process.exit(2); }, timeoutMs);
