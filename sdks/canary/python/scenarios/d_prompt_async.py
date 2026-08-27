#!/usr/bin/env python3
# Copyright (C) 2026 Michael Kao
# SPDX-License-Identifier: AGPL-3.0-or-later
"""D-PROMPT-ASYNC canary — Python SDK
Tests POST /sessions/:id/prompt + SSE session.idle.
"""

from __future__ import annotations
import json, sys, os, time, threading

sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), ".."))
from canary import (
    Runner,
    Config,
    config_from_env,
    wait_active,
    ensure_session_with_retry,
    raw_do,
)
from llmsafespaces import LLMSafeSpaces


def run(r: Runner, cfg: Config) -> None:
    if not cfg.llm_api_key:
        r.ok("prompt-async: skipped (no LLM API key)")
        return
    c = LLMSafeSpaces(cfg.api_url, api_key=cfg.api_key, timeout=120.0)
    ws_id = None
    try:
        ok, ws = r.assert_no_error(
            lambda: c.workspaces.create(
                name="canary-py-prompt-async", runtime="base", storage_size="1Gi"
            ),
            "create: no error",
        )
        if not ok:
            return
        ws_id = ws.id

        phase = wait_active(c, ws_id)
        r.assert_(phase == "Active", "reach-active", f"got {phase!r}")
        if phase != "Active":
            return

        try:
            sess = ensure_session_with_retry(c, ws_id, 5)
        except Exception as e:
            r.fail("ensure-session: no error", str(e))
            return
        r.ok("ensure-session: no error")
        sid = sess.sessionId

        # P1: prompt_async returns 202 immediately
        s, _ = raw_do(
            "POST",
            f"{cfg.api_url}/api/v1/workspaces/{ws_id}/sessions/{sid}/prompt",
            cfg.api_key,
            json.dumps({"parts": [{"type": "text", "text": "Reply with the word: ASYNC-OK"}]}).encode(),
        )
        r.assert_(s in (200, 202), f"prompt-async: 202 immediate (got {s})")

        # P2+P3: Subscribe to SSE and wait for session.idle
        idle_received = _wait_for_session_idle(cfg, ws_id, sid, timeout=90)
        r.assert_(
            idle_received, "sse: received session.idle", "no session.idle within 90s"
        )

        # P4: history contains response
        ok2, hist = r.assert_no_error(
            lambda: c.sessions.get_history(ws_id, sid), "history-after-async: no error"
        )
        if ok2:
            r.assert_(len(hist) >= 1, "history-after-async: ≥1 entry", str(len(hist)))

        # N1: malformed session ID
        s2, _ = raw_do(
            "POST",
            f"{cfg.api_url}/api/v1/workspaces/{ws_id}/sessions/..%2Fetc/prompt",
            cfg.api_key,
            json.dumps({"parts": [{"type": "text", "text": "ping"}]}).encode(),
        )
        r.assert_(s2 == 400, "malformed-session-id: 400", str(s2))

    finally:
        if ws_id:
            try:
                c.workspaces.delete(ws_id)
            except:
                pass


def _wait_for_session_idle(
    cfg: Config, ws_id: str, session_id: str, timeout: float
) -> bool:
    import httpx

    deadline = time.time() + timeout
    try:
        with httpx.stream(
            "GET",
            f"{cfg.api_url}/api/v1/workspaces/{ws_id}/events",
            headers={
                "Authorization": f"Bearer {cfg.api_key}",
                "Accept": "text/event-stream",
            },
            timeout=timeout + 5,
        ) as resp:
            for line in resp.iter_lines():
                if time.time() > deadline:
                    break
                if not line.startswith("data: "):
                    continue
                data = line[6:]
                try:
                    evt = json.loads(data)
                    if (
                        evt.get("type") == "session.status"
                        and evt.get("status") == "idle"
                        and evt.get("session_id", session_id) == session_id
                    ):
                        return True
                except Exception:
                    pass
    except Exception:
        pass
    return False


if __name__ == "__main__":
    r = Runner("prompt-async")
    cfg = config_from_env()
    run(r, cfg)
    r.print()
    sys.exit(r.exit_code())
