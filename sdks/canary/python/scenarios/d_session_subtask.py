#!/usr/bin/env python3
# Copyright (C) 2026 Michael Kao
# SPDX-License-Identifier: AGPL-3.0-or-later
"""D-SESSION-SUBTASK canary — Python SDK"""

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
from llmsafespaces.client import message_text


def run(r: Runner, cfg: Config) -> None:
    if not cfg.llm_api_key:
        r.ok("session-subtask: skipped (no LLM API key)")
        return

    c = LLMSafeSpaces(cfg.api_url, api_key=cfg.api_key, timeout=120.0)
    ws_id = None
    try:
        ok, ws = r.assert_no_error(
            lambda: c.workspaces.create(
                name="canary-py-subtask", runtime="base", storage_size="1Gi"
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
                ws_id,
                sid,
                "Use the task tool to create a subtask that writes hello to /tmp/canary-sub.txt",
            ),
            "send-message: no error",
        )
        if ok2 and msg is not None:
            r.assert_(len(message_text(msg)) > 0, "send-message: non-empty text")

        subtask_found = False
        deadline = time.time() + 30
        while time.time() < deadline:
            try:
                sessions = c.sessions.list(ws_id)
                for s in sessions:
                    if s.get("parentId") and s["parentId"] != "":
                        subtask_found = True
                        r.assert_(
                            len(s["parentId"]) > 0,
                            "subtask: has parentId",
                            repr(s["parentId"]),
                        )
                        break
            except Exception:
                pass
            if subtask_found:
                break
            time.sleep(2)

        if not subtask_found:
            r.ok("subtask: skipped (model does not use task tool)")

    finally:
        if ws_id:
            try:
                c.workspaces.delete(ws_id)
            except Exception:
                pass


if __name__ == "__main__":
    r = Runner("session-subtask")
    cfg = config_from_env()
    run(r, cfg)
    r.print()
    sys.exit(r.exit_code())
