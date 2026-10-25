#!/usr/bin/env python3
# 1949-build-exact-context.py — one-shot lab builder: reconstructs the
# wire-format chat-completions body the orchestrator session
# (novel-nutmeg-verifies) presented to the model at the moment of the
# 2026-10-25 02:43 epic_activate duplicate-key runaway (#1949's
# production confirmation), from a pod-local opencode store. Emits the
# body the replay harness replays
# (scripts/1949-aged-arg-replay.py --context exact).
#
# NEEDS THE POD-LOCAL STORE: this reads /workspace/.local/opencode/
# opencode.db (read-only) and hard-codes that session's message ids —
# it is evidence-reproduction tooling for the lane that observed the
# corruption, NOT runnable from a fresh clone. The synthetic arm of the
# harness (the default context) carries the checked-in discriminator.
#
# Windows: "1025" = post- the 2026-10-24 11:35 compaction (summary +
# tail) through the 10-25 02:38:58 trigger, inclusive; "1024" = the
# poison-heavier post-10-23-06:07 window through the 10-24 10:36
# trigger (109 llmsafespaces_* calls). Tool-call arguments are
# re-serialized in their STORED key order (the emission order the model
# saw: old-shape bulk-first). Secrets scrubbed.

import json
import re
import sqlite3
import sys

SES = "ses_e9a78179affeLA8s7YV2bRqRaj"
# Window presets: (label, TAIL, END, summary-prefix)
WINDOWS = {
    # The 2026-10-25 02:43 epic_activate runaway (#1949 prod confirmation):
    "1025": ("msg_16d83447b001GMTB5vk1tHUGxz",
             "msg_ob_1792895935866508269_5012c23f_4dda_4387_809e_940209bb",
             "## Objective"),
    # The 2026-10-24 10:44 send_message runaway (52KB duplicate-key): the
    # poison-heaviest window — 108 llmsafespaces_* calls, post-10-23-06:07
    # compaction tail.
    "1024": ("msg_ob_1792735443005626030_c946cfe4_9dff_4e22_99fb_32fcc529",
             "msg_16d7c0deb001ySP5OgxErVEcSO",
             None),
}
DB = "/workspace/.local/opencode/opencode.db"

SECRET_RES = [
    (re.compile(r"sk-[A-Za-z0-9_\-]{8,}"), "sk-SCRUBBED"),
    (re.compile(r"ghp_[A-Za-z0-9_]{20,}"), "ghp_SCRUBBED"),
    (re.compile(r"github_pat_[A-Za-z0-9_]{20,}"), "github_pat_SCRUBBED"),
    (re.compile(r"(Authorization: Bearer )[A-Za-z0-9._\-]{8,}"), r"\1SCRUBBED"),
    (re.compile(r"(Authorization: Basic )[A-Za-z0-9+/=]{8,}"), r"\1SCRUBBED"),
]


def scrub(s):
    for rx, repl in SECRET_RES:
        s = rx.sub(repl, s)
    return s


def main():
    label = sys.argv[1] if len(sys.argv) > 1 else "1025"
    out_path = sys.argv[2] if len(sys.argv) > 2 else "/tmp/opencode/replay-1949/exact_context.json"
    TAIL, END, SUMM_PREFIX = WINDOWS[label]
    db = sqlite3.connect(f"file:{DB}?mode=ro", uri=True)
    cur = db.cursor()
    msgs = cur.execute(
        "SELECT id, time_created, data FROM message WHERE session_id=? ORDER BY time_created",
        (SES,)).fetchall()
    parts = cur.execute(
        "SELECT id, message_id, time_created, data FROM part WHERE session_id=? ORDER BY time_created",
        (SES,)).fetchall()
    by_msg = {}
    for pid, mid, tc, pd in parts:
        by_msg.setdefault(mid, []).append((tc, json.loads(pd)))

    i0 = next(i for i, (m, tc, d) in enumerate(msgs) if m == TAIL)
    i1 = next(i for i, (m, tc, d) in enumerate(msgs) if m == END)
    tail = msgs[i0:i1 + 1]

    # Compaction summary: the assistant text part right before tail start
    # (the compaction summary the model wrote). 1024 window: locate the
    # longest text part within 3 min before tail start.
    summary = None
    if SUMM_PREFIX is not None:
        for pid, mid, tc, pd in parts:
            p = json.loads(pd)
            if p.get("type") == "text" and p.get("text", "").startswith(SUMM_PREFIX) and tc < tail[0][1]:
                summary = p["text"]
    else:
        cand = [(tc, p.get("text", "")) for pid, mid, tc, pd in parts
                for p in [json.loads(pd)]
                if p.get("type") == "text" and tail[0][1] - 180000 < tc < tail[0][1]]
        if cand:
            summary = max(cand, key=lambda x: len(x[1]))[1]
    if summary is None:
        summary = "(compaction summary unavailable)"

    wire = [{"role": "user", "content":
             "The earlier conversation was compacted. Summary of the session so far:\n\n" + scrub(summary)}]

    synth = 0
    for mid, tc, d in tail:
        role = json.loads(d).get("role")
        plist = sorted(by_msg.get(mid, []), key=lambda x: x[0])
        if role == "user":
            texts = [p.get("text", "") for _, p in plist if p.get("type") == "text"]
            if texts:
                wire.append({"role": "user", "content": scrub("\n".join(texts))})
            continue
        if role != "assistant":
            continue
        content = "\n".join(p.get("text", "") for _, p in plist if p.get("type") == "text")
        calls = []
        results = []
        for _, p in plist:
            if p.get("type") != "tool":
                continue
            state = p.get("state", {}) or {}
            tool = p.get("tool", "")
            if tool == "invalid":
                # A recorded runaway: the emitted text lives inside the
                # error ("Invalid input for tool X: JSON parsing failed:
                # Text: <raw>"). Re-inline the RAW emitted arguments —
                # the exact poison the next request re-sends.
                named = (state.get("input") or {}).get("tool", "llmsafespaces_unknown")
                err = (state.get("input") or {}).get("error", "")
                raw = err.split("Text: ", 1)[1] if "Text: " in err else err
                cid = p.get("callID") or f"call_invalid_{synth}"
                synth += 1
                calls.append({"id": cid, "type": "function",
                              "function": {"name": named, "arguments": scrub(raw)}})
                results.append({"role": "tool", "tool_call_id": cid,
                                "content": "Error: invalid arguments (JSON parsing failed)"})
                continue
            inp = state.get("input")
            args = json.dumps(inp) if inp is not None else "{}"  # stored key order preserved
            cid = p.get("callID") or f"call_synth_{synth}"
            synth += 1
            calls.append({"id": cid, "type": "function",
                          "function": {"name": tool, "arguments": scrub(args)}})
            out = state.get("output")
            if out is None:
                out = state.get("error") or ""
            results.append({"role": "tool", "tool_call_id": cid,
                            "content": scrub(str(out)[:8000])})
        m = {"role": "assistant", "content": scrub(content)}
        if calls:
            m["tool_calls"] = calls
        wire.append(m)
        wire.extend(results)

    body = {"messages": wire}
    with open(out_path, "w") as f:
        json.dump(body, f)
    n_llss = sum(1 for m in wire if m.get("tool_calls")
                 for c in m["tool_calls"] if c["function"]["name"].startswith("llmsafespaces_"))
    n_all = sum(1 for m in wire if m.get("tool_calls") for c in m["tool_calls"])
    print(f"wire messages: {len(wire)} | tool_calls: {n_all} | llmsafespaces: {n_llss} | "
          f"bytes: {len(json.dumps(body))} -> {out_path}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
