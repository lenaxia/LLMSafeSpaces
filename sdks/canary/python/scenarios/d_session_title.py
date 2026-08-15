#!/usr/bin/env python3
# Copyright (C) 2026 Michael Kao
# SPDX-License-Identifier: AGPL-3.0-or-later
"""D-SESSION-TITLE canary — Python SDK"""

from __future__ import annotations

import sys
import os
import time

sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), ".."))

from canary import (
    Runner,
    Config,
    config_from_env,
    wait_active,
    ensure_session_with_retry,
)
from llmsafespaces import LLMSafeSpaces


def run(r: Runner, cfg: Config) -> None:
    if not cfg.llm_api_key:
        r.ok("session-title: skipped (no LLM API key)")
        return

    c = LLMSafeSpaces(cfg.api_url, api_key=cfg.api_key, timeout=120.0)
    ws_id = None
    try:
        ok, ws = r.assert_no_error(
            lambda: c.workspaces.create(
                name="canary-py-sess-title", runtime="base", storage_size="1Gi"
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

        ok2, msg = r.assert_no_error(
            lambda: c.sessions.send_message(
                ws_id, sid, "What are the first 5 prime numbers? List them."
            ),
            "send-message: no error",
        )
        if ok2 and msg is not None:
            r.assert_(len(msg.content) > 0, "send-message: non-empty content")

        title_found = False
        deadline = time.time() + 20
        while time.time() < deadline:
            try:
                sessions = c.sessions.list(ws_id)
                for s in sessions:
                    if s.get("id") == sid and s.get("title"):
                        title_found = True
                        r.assert_(
                            len(s["title"]) > 0,
                            "session-title: non-empty title",
                            repr(s["title"]),
                        )
                        break
            except Exception:
                pass
            if title_found:
                break
            time.sleep(2)

        if not title_found:
            r.fail("session-title: title found within 20s", "no title found")

    finally:
        if ws_id:
            try:
                c.workspaces.delete(ws_id)
            except Exception:
                pass


if __name__ == "__main__":
    r = Runner("session-title")
    cfg = config_from_env()
    run(r, cfg)
    r.print()
    sys.exit(r.exit_code())
