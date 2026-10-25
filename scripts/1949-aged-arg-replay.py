#!/usr/bin/env python3
# 1949-aged-arg-replay.py — issue #1949 verification instrument (the #1929
# replay-harness arm, checked in per the #1849 adjudication's synthetic-
# harness plan: constructed transcripts + signature scorer, no DB
# dependency).
#
# Proves the split #1929's evidence table established and #1949's cure:
#
#   neutral history + new schemas            -> CLEAN   (row 2)
#   poisoned old-shape history + new schemas -> RUNAWAY (last row)
#   poisoned history + aged-arg compaction   -> CLEAN   (row 4 — the cure)
#
# The poisoned arm reproduces the mechanism on CURRENT main behavior:
# main has NO relay-side compaction, so what the model sees is exactly
# what this harness sends. The compacted arm applies the SAME transform
# the relay-router (cmd/relay-router agedarg.go) applies in-path — the
# python mirror is pinned to the Go implementation by the wire-fixture
# parity test in cmd/relay-router/agedarg_test.go (same transcript in,
# same stubbed body out).
#
# Arms:
#   neutral   — clean history, same final request (instrument sanity:
#               the harness must NOT runaway on its own)
#   poisoned  — old-shape bulk-first llmsafespaces_* prior calls in the
#               in-context history, new (post-#1929 bulk-last) schemas
#   compacted — the poisoned body with aged llmsafespaces_* arguments
#               stubbed (recent window untouched, results untouched)
#
# Scorer signatures (the #1929/#1849 classes): duplicate JSON keys,
# degenerate fragment tiling (>=10x), malformed arguments, native-markup
# leak, splice artifacts persisted as valid JSON, multi-call, size
# blowup. A run is RUNAWAY if any signature fires; CLEAN if the single
# tool call parses strictly with none.
#
# Usage (env-driven; never hardcodes credentials):
#   REPLAY_BASE_URL=https://host REPLAY_API_KEY=sk-... \
#   REPLAY_MODEL=glm-5.3 scripts/1949-aged-arg-replay.py \
#       [--runs 3] [--arms neutral,poisoned,compacted] [--base-alpha A]
#
# Exits 0 iff every executed arm matches its expected verdict.

import argparse
import json
import os
import re
import sys
import time
import urllib.error
import urllib.request

TOOL_PREFIX = "llmsafespaces_"
# Mirror of the Go default (cmd/relay-router/agedarg.go): the most recent
# 30 tool calls (any tool) form the untouched recent window.
RECENT_TOOL_CALLS = 30
STUB_NOTE = ("arguments removed by relay aged-arg compaction "
             "(full text preserved in the session audit trail)")

WORDS = ("drain ledger reclaim wedge sentinel charter posture seam verdict "
         "thumbprint cadence harness relay compact stub aged emission "
         "poison cure lane board epic task stage claim lease review").split()


def pseudo_text(seed, n_words, salt=""):
    # Deterministic prose-like filler: varied words, no accidental
    # tiling signatures (the poison is the ARG-STRING SHAPE — bulk-first
    # key order — not degenerate content; #1929's poisoned contexts were
    # real charters).
    out = []
    x = (seed * 2654435761 + len(salt) * 40503 + 0x9E3779B9) & 0xFFFFFFFF
    for i in range(n_words):
        x = (x * 1103515245 + 12345) & 0xFFFFFFFF
        out.append(WORDS[(x >> 16) % len(WORDS)])
        if i % 17 == 16:
            out.append("\n")
    return salt + " " + " ".join(out)


def old_shape_create(seed, epic, title):
    # Pre-#1929 arguments: Go map marshaling emitted keys ALPHABETICALLY —
    # bulk payloads (acceptanceCriteria array, multi-KB description string)
    # FIRST, discrete scalars (epicId/title) LAST. The exact seam glm
    # emission latches onto (12/12 runaway, #1929).
    desc = pseudo_text(seed, 420, "## Charter")
    crit = [{"text": pseudo_text(seed + 100 + i, 14, f"AC-{i+1}")} for i in range(5)]
    args = {
        "acceptanceCriteria": crit,
        "description": desc,
        "dependsOn": [],
        "epicId": epic,
        "tags": ["lane", f"seed-{seed}"],
        "title": title,
    }
    return json.dumps(args)  # insertion order preserved = bulk-first


def old_shape_update(seed, task, spliced=False):
    desc = pseudo_text(seed, 380, "## Revised charter")
    if spliced:
        # The production corruption #2 shape: junk that still parses,
        # persisted in the poisoned history (orchestrator 2026-10-25).
        desc += ' ","title":"PLACEHOLDER"}'
    args = {
        "acceptanceCriteria": [{"text": pseudo_text(seed + 50 + i, 12, f"AC-{i+1}")} for i in range(4)],
        "description": desc,
        "setCriteriaMet": {"index": 0, "met": True},
        "taskId": task,
    }
    return json.dumps(args)


def old_shape_boolean(tool, **flags):
    # The production corruption #1 shape (epic_activate
    # acknowledgeInactiveDeps / delete_epic force — the x2601 boolean
    # runaway signature).
    args = dict(flags)
    args["epicId"] = "epc-drill" + str(sum(map(len, flags)) % 7)
    return json.dumps(args)


def tool_result(call_id, ok, payload):
    return {"role": "tool", "tool_call_id": call_id,
            "content": json.dumps(payload)}


def asst_call(call_id, name, args_str):
    return {"role": "assistant", "content": "",
            "tool_calls": [{"id": call_id, "type": "function",
                            "function": {"name": name, "arguments": args_str}}]}


def asst_text(text):
    return {"role": "assistant", "content": text}


def user(text):
    return {"role": "user", "content": text}


def new_catalog():
    # The CURRENT (post-#1929, bulk-last) tool schemas, abbreviated to
    # the fields the replay exercises but preserving each tool's field
    # ORDER as main advertises it (discrete ids/flags first, bulk arrays
    # mid, longest freeform string LAST).
    props = lambda order: {"type": "object", "properties": order}
    create = {
        "epicId": {"type": "string", "description": "Epic short id (epc-… or UUID)"},
        "title": {"type": "string", "description": "Task title"},
        "tags": {"type": "array", "items": {"type": "string"}, "description": "Tags"},
        "acceptanceCriteria": {"type": "array", "description": "Checkable criteria [{text, met}]",
                               "items": {"type": "object", "properties": {"met": {"type": "boolean"}, "text": {"type": "string"}},
                                         "required": ["text"]}},
        "dependsOn": {"type": "array", "items": {"type": "string"}, "description": "Prerequisite task short ids"},
        "description": {"type": "string", "description": "Markdown; seeds the fresh worker's charter"},
    }
    update = {
        "taskId": {"type": "string", "description": "Task short id"},
        "setCriteriaMet": {"type": "object", "properties": {"index": {"type": "integer"}, "met": {"type": "boolean"}},
                           "required": ["index"], "description": "{index, met}"},
        "acceptanceCriteria": {"type": "array", "description": "Criteria (REPLACE when present)",
                               "items": {"type": "object", "properties": {"met": {"type": "boolean"}, "text": {"type": "string"}},
                                         "required": ["text"]}},
        "description": {"type": "string", "description": "New description (REPLACE)"},
    }
    activate = {
        "epicId": {"type": "string", "description": "Epic short id"},
        "acknowledgeInactiveDeps": {"type": "boolean", "description": "Acknowledge deps entering inactive epics"},
    }
    delete = {
        "epicId": {"type": "string", "description": "Epic short id"},
        "force": {"type": "boolean", "description": "Abort in-flight workers and delete anyway"},
    }
    def f(name, desc, order):
        return {"type": "function", "function": {"name": name, "description": desc,
                                                 "parameters": props(order)}}
    return [
        f(TOOL_PREFIX + "board_create_task", "Create a backlog task in an epic.", create),
        f(TOOL_PREFIX + "board_update_task", "Patch a task (title, description, criteria).", update),
        f(TOOL_PREFIX + "board_epic_activate", "Activate an epic.", activate),
        f(TOOL_PREFIX + "board_delete_epic", "Delete an epic and its tasks.", delete),
    ]


SYSTEM = ("You are an interactive CLI agent for software engineering on the "
          "LLMSafeSpaces platform. Use the provided tools to manage the task "
          "board as the user directs.")


def poisoned_messages(final_request, alpha, n_calls=10):
    msgs = [user(pseudo_text(1, 24, "Session start — orchestrate the board work as directed."))]
    n = 0
    for i in range(n_calls):
        n += 1
        cid = f"call_old_{i}"
        if i % 4 == 3:
            msgs.append(user("Activate the drill epic, acknowledging inactive deps."))
            msgs.append(asst_call(cid, TOOL_PREFIX + "board_epic_activate",
                                  old_shape_boolean("activate", acknowledgeInactiveDeps=True)))
            msgs.append(tool_result(cid, True, {"ok": True, "epic": {"id": "epc-drill3", "stage": "active"}}))
        elif i % 4 == 2 and i > 0:
            msgs.append(user("That last charter needs a revision — update it."))
            msgs.append(asst_call(cid, TOOL_PREFIX + "board_update_task",
                                  old_shape_update(alpha + i, f"tsk-{alpha}{i}", spliced=(i == 6))))
            msgs.append(tool_result(cid, True, {"ok": True, "task": {"id": f"tsk-{alpha}{i}", "stage": "in_progress",
                                                                    "criteria": 4}}))
        else:
            title = f"{['Relay', 'Ledger', 'Drain', 'Sentinel', 'Posture', 'Phrase', 'Emission', 'Usage'][i % 8]} hardening slice {i}"
            msgs.append(user(f"Create the task: {title}."))
            msgs.append(asst_call(cid, TOOL_PREFIX + "board_create_task",
                                  old_shape_create(alpha + i, "epc-drill3", title)))
            msgs.append(tool_result(cid, True, {"ok": True, "task": {"id": f"tsk-{alpha}{i}", "title": title,
                                                                    "stage": "backlog", "criteria": 5}}))
        msgs.append(asst_text(pseudo_text(alpha + 500 + i, 10, "Done — proceeding.")))
    msgs.append(user(final_request))
    return msgs


def neutral_messages(final_request):
    return [
        user("Hello — ready to organize the board."),
        asst_text("Ready. Tell me what to create."),
        user(final_request),
    ]


FINAL_REQUEST = ("Create one more task in epic epc-drill3 titled 'Final replay "
                 "verification' whose description is exactly: verify the replay "
                 "harness end to end. Give it one acceptance criterion: the "
                 "compacted arm is clean.")


# ---- the compaction (python mirror of cmd/relay-router/agedarg.go) ----

def compact(body, recent_tool_calls=RECENT_TOOL_CALLS):
    msgs = body.get("messages")
    if not isinstance(msgs, list):
        return json.dumps(body), 0
    flat = []  # (msg_idx, call_idx) of every tool call, forward order
    for mi, m in enumerate(msgs):
        if isinstance(m, dict) and isinstance(m.get("tool_calls"), list):
            for ci in range(len(m["tool_calls"])):
                flat.append((mi, ci))
    protected = set(flat[-recent_tool_calls:]) if recent_tool_calls > 0 else set()
    stubbed = 0
    for mi, ci in flat:
        if (mi, ci) in protected:
            continue
        call = msgs[mi]["tool_calls"][ci]
        if not isinstance(call, dict):
            continue
        fn = call.get("function")
        if not isinstance(fn, dict):
            continue
        name = fn.get("name", "")
        if not isinstance(name, str) or not name.startswith(TOOL_PREFIX):
            continue
        if fn.get("arguments") == json.dumps({"note": STUB_NOTE}):
            continue  # already stubbed (idempotent across turns)
        fn["arguments"] = json.dumps({"note": STUB_NOTE})
        stubbed += 1
    return json.dumps(body), stubbed


# ---- scorer ----

def strict_loads(s):
    dups = []

    def hook(pairs):
        seen = set()
        for k, _ in pairs:
            if k in seen:
                dups.append(k)
            seen.add(k)
        return dict(pairs)

    val = json.loads(s, object_pairs_hook=hook)
    return val, dups


TILE_RE = re.compile(r"(.{12,240}?)\1{9,}", re.DOTALL)


def score_arguments(args_str):
    """Score one emitted arguments string. Returns list of signatures."""
    sig = []
    if not isinstance(args_str, str):
        return ["arguments not a string"]
    if len(args_str) > 32768:
        sig.append(f"size blowup ({len(args_str)} bytes)")
    try:
        val, dups = strict_loads(args_str)
    except json.JSONDecodeError as e:
        return ["malformed JSON: " + str(e)[:80]]
    if dups:
        sig.append(f"duplicate keys: {sorted(set(dups))[:4]}")
    if TILE_RE.search(args_str):
        sig.append("fragment tiling >=10x")
    # key repeated across separate object scopes (the force x2601 class
    # after re-serialization) — count raw occurrences of any key token
    for m in set(re.findall(r'"([a-zA-Z_]{3,24})":', args_str)):
        if args_str.count(f'"{m}":') >= 10:
            sig.append(f"key repeated >=10x: {m}")
            break
    if "<arg_key>" in args_str or "<tool_call>" in args_str:
        sig.append("native-markup leak")
    for s in re.findall(r'"((?:[^"\\]|\\.){8,})"', args_str):
        if '","' in s and '":' in s:
            sig.append("splice artifact inside string value")
            break
    return sig


def score_response(resp):
    """Score a chat-completion response. Returns (verdict, notes).

    RUNAWAY iff any emitted tool-call arguments carry a corruption
    signature (duplicate keys, tiling, malformed, leak, splice) or the
    content tiles. Multi-call turns are legal (the exact-context replay
    legitimately chains several board calls in one turn); text-only
    responses are clean no-ops for this instrument."""
    try:
        choice = resp["choices"][0]
        msg = choice["message"]
    except (KeyError, IndexError, TypeError):
        return "RUNAWAY", ["response missing choices/message"]
    calls = msg.get("tool_calls") or []
    sig = []
    for i, call in enumerate(calls):
        fn = call.get("function") or {}
        s = score_arguments(fn.get("arguments"))
        if s:
            sig.append(f"call[{i}] {fn.get('name','?')}: " + "; ".join(s))
    content = msg.get("content") or ""
    if TILE_RE.search(content or ""):
        sig.append("content tiling")
    return ("RUNAWAY" if sig else "CLEAN"), sig or [f"{len(calls)} call(s) emitted"]


# ---- driver ----

def _load_persona():
    # The pinned-binary build persona (extracted from the opencode binary
    # in the pod) when available; the inline copy below is the fallback.
    path = os.environ.get("REPLAY_PERSONA", "/tmp/opencode/replay-1949/persona_build.txt")
    try:
        txt = open(path).read()
        if len(txt) > 4000:
            return txt
    except OSError:
        pass
    return _PERSONA_FALLBACK


PERSONA = _load_persona()

_PERSONA_FALLBACK = """You are opencode, an interactive CLI tool that helps users with software engineering tasks. Use the instructions below and the tools available to you to assist the user.

You should be concise, direct, and to the point. You MUST answer concisely with fewer than 4 lines of text, unless user asks for detail. Output text to communicate with the user; all text you output outside of tool use is displayed to the user. Only use tools to complete tasks.

You have access to a set of tools you can use to answer the user's question. You can invoke functions by writing a tool_calls block like the following as part of your reply to the user:

Tool calls are made with the exact tool schemas advertised in the tools parameter of this request. Emit exactly one tool call when a single action is needed; chain multiple calls across turns as results arrive.

If you intend to call multiple tools and there are no dependencies between the calls, make all of the independent calls in the same block.

IMPORTANT: You should NEVER generate or guess URLs for the user unless you are confident that the URLs are for helping the user with programming.

You are an agent - keep going until the user's query is completely resolved, before ending your turn. Only end your turn when you are sure the problem is solved.

Proactiveness: you are allowed to be proactive and take actions and follow-up actions without asking the user, but only when the task at hand clearly warrants it. Do not surprise the user with actions you take without asking.

Remember that your output will be displayed on a command line interface. Your responses can use GitHub-flavored markdown.

Avoid unnecessary preamble or postamble unless the user asks for detail."""

PLATFORM_TOOLS = [
    {"type": "function", "function": {
        "name": "bash", "description": "Executes a given bash command in a persistent shell session with optional timeout.",
        "parameters": {"type": "object", "properties": {
            "command": {"type": "string", "description": "The command to execute"},
            "workdir": {"type": "string", "description": "The working directory to run the command in"},
            "timeout": {"type": "integer", "description": "Optional timeout in milliseconds"}},
            "required": ["command"]}}},
    {"type": "function", "function": {
        "name": "read", "description": "Read a file or directory from the local filesystem.",
        "parameters": {"type": "object", "properties": {
            "filePath": {"type": "string", "description": "The absolute path to the file or directory to read"},
            "offset": {"type": "integer", "description": "The line number to start from (1-indexed)"},
            "limit": {"type": "integer", "description": "The maximum number of lines to read (defaults to 2000)"}},
            "required": ["filePath"]}}},
    {"type": "function", "function": {
        "name": "edit", "description": "Performs exact string replacements in files.",
        "parameters": {"type": "object", "properties": {
            "filePath": {"type": "string", "description": "The absolute path to the file to modify"},
            "oldString": {"type": "string", "description": "The text to replace"},
            "newString": {"type": "string", "description": "The text to replace it with"}},
            "required": ["filePath", "oldString", "newString"]}}},
    {"type": "function", "function": {
        "name": "write", "description": "Writes a file to the local filesystem.",
        "parameters": {"type": "object", "properties": {
            "filePath": {"type": "string", "description": "The absolute path to the file to write"},
            "content": {"type": "string", "description": "The content to write to the file"}},
            "required": ["filePath", "content"]}}},
]


def live_catalog_tools():
    # The current (post-#1929) llmsafespaces_* catalog, as agentd serves
    # it: MCP tools/list -> OpenAI function format, wire prefix applied.
    path = os.environ.get("REPLAY_CATALOG", "/tmp/opencode/replay-1949/catalog.json")
    tools = json.load(open(path))
    out = []
    for t in tools:
        out.append({"type": "function", "function": {
            "name": TOOL_PREFIX + t["name"],
            "description": t.get("description", ""),
            "parameters": t.get("inputSchema", {"type": "object"})}})
    return out


def exact_messages(path):
    body = json.load(open(path))
    return body["messages"]


def post(base, key, body_bytes, model, timeout=300):
    req = urllib.request.Request(base.rstrip("/") + "/v1/chat/completions",
                                 data=body_bytes, method="POST")
    req.add_header("Content-Type", "application/json")
    req.add_header("Authorization", "Bearer " + key)
    with urllib.request.urlopen(req, timeout=timeout) as r:
        return json.loads(r.read())


def run_arm(name, body, runs, base, key, model, pace):
    verdicts = []
    for i in range(runs):
        t0 = time.time()
        try:
            resp = post(base, key, body, model)
        except urllib.error.HTTPError as e:
            verdicts.append((f"HTTP-{e.code}", [e.read()[:120].decode("utf-8", "replace")]))
            time.sleep(pace)
            continue
        except Exception as e:  # transport
            verdicts.append(("TRANSPORT-ERROR", [repr(e)[:120]]))
            time.sleep(pace)
            continue
        v, sig = score_response(resp)
        usage = resp.get("usage", {})
        verdicts.append((v, sig + [f"completion_tokens={usage.get('completion_tokens')}"]))
        print(f"    run {i+1}: {v} ({time.time()-t0:.1f}s) {sig}", flush=True)
        time.sleep(pace)
    return verdicts


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--runs", type=int, default=3)
    ap.add_argument("--arms", default="neutral,poisoned,compacted")
    ap.add_argument("--context", default="synthetic", choices=["synthetic", "exact"],
                    help="synthetic: deterministic constructed poisoned transcript; "
                         "exact: the reconstructed orchestrator wire context "
                         "(REPLAY_EXACT_CONTEXT=path/to/exact_context.json, built by "
                         "build-exact-context.py from the pod-local opencode store)")
    ap.add_argument("--exact-context", default=None,
                    help="path to the exact-context JSON (overrides REPLAY_EXACT_CONTEXT)")
    ap.add_argument("--base-alpha", type=int, default=7,
                    help="seed for deterministic transcript generation")
    ap.add_argument("--pace", type=float, default=5.0)
    ap.add_argument("--max-tokens", type=int, default=6000)
    args = ap.parse_args()

    base = os.environ.get("REPLAY_BASE_URL", "")
    key = os.environ.get("REPLAY_API_KEY", "")
    model = os.environ.get("REPLAY_MODEL", "glm-5.3")
    if not base or not key:
        print("REPLAY_BASE_URL and REPLAY_API_KEY are required", file=sys.stderr)
        return 2

    exact_path = (args.exact_context
                  or os.environ.get("REPLAY_EXACT_CONTEXT", ""))
    if args.context == "exact" and not exact_path:
        print("--context exact needs --exact-context or REPLAY_EXACT_CONTEXT",
              file=sys.stderr)
        return 2

    print(f"# 1949 aged-arg replay — model={model} context={args.context} "
          f"runs/arm={args.runs} recent_window={RECENT_TOOL_CALLS}")
    results = {}
    for arm in args.arms.split(","):
        arm = arm.strip()
        if args.context == "exact":
            base_msgs = ([{"role": "system", "content": PERSONA}]
                         + exact_messages(exact_path))
            tools = live_catalog_tools() + PLATFORM_TOOLS
        else:
            base_msgs = (neutral_messages(FINAL_REQUEST) if arm == "neutral"
                         else poisoned_messages(FINAL_REQUEST, args.base_alpha))
            tools = new_catalog()
        msgs = base_msgs
        body = {"model": model, "messages": msgs, "tools": tools,
                "max_tokens": args.max_tokens}
        stubbed = 0
        if arm == "compacted":
            body_bytes, stubbed = compact(body)
        else:
            body_bytes = json.dumps(body)
        n_old = sum(1 for m in msgs if isinstance(m, dict)
                    and isinstance(m.get("tool_calls"), list)
                    for c in m["tool_calls"]
                    if (c.get("function") or {}).get("name", "").startswith(TOOL_PREFIX))
        print(f"  arm={arm} llmsafespaces_* prior calls={n_old} stubbed={stubbed} "
              f"body={len(body_bytes)}B")
        results[arm] = run_arm(arm, body_bytes.encode(), args.runs, base, key, model, args.pace)

    expected = {"neutral": "CLEAN", "poisoned": "RUNAWAY", "compacted": "CLEAN"}
    ok = True
    print("\n| arm | prior args | expected | observed |")
    print("|---|---|---|---|")
    for arm, verdicts in results.items():
        obs = "/".join(v for v, _ in verdicts) or "NO-RUNS"
        exp = expected.get(arm, "?")
        arm_ok = obs == exp
        ok = ok and arm_ok
        pa = "stubbed" if arm == "compacted" else ("—" if arm == "neutral" else "full (old-shape)")
        print(f"| {arm} | {pa} | {exp} {'' if arm_ok else '*** MISMATCH ***'} | {obs} |")
    print("VERDICT:", "ALL-ARMS-MATCH" if ok else "MISMATCH")
    return 0 if ok else 1


if __name__ == "__main__":
    sys.exit(main())
