#!/usr/bin/env python3
# Copyright (C) 2026 Michael Kao
# SPDX-License-Identifier: AGPL-3.0-or-later
"""D-AGENT-INPUT canary — Python SDK"""

from __future__ import annotations

import json
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
    raw_do,
)
from llmsafespaces import LLMSafeSpaces
from llmsafespaces.client import message_text


def run(r: Runner, cfg: Config) -> None:
    if not cfg.llm_api_key:
        r.ok("agent-input: skipped (no LLM API key)")
        return

    c = LLMSafeSpaces(cfg.api_url, api_key=cfg.api_key, timeout=120.0)
    ws_id = None
    try:
        ok, ws = r.assert_no_error(
            lambda: c.workspaces.create(
                name="canary-py-agent-input", runtime="base", storage_size="1Gi"
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

        s1, b1 = raw_do(
            "GET",
            f"{cfg.api_url}/api/v1/workspaces/{ws_id}/question",
            cfg.api_key,
        )
        r.assert_(s1 == 200, "question: 200", str(s1))
        if s1 == 200:
            try:
                questions = json.loads(b1)
                r.assert_(isinstance(questions, list), "question: returns array")
            except Exception:
                r.fail("question: valid JSON", "parse error")

        s2, b2 = raw_do(
            "GET",
            f"{cfg.api_url}/api/v1/workspaces/{ws_id}/permission",
            cfg.api_key,
        )
        r.assert_(s2 == 200, "permission: 200", str(s2))
        if s2 == 200:
            try:
                perms = json.loads(b2)
                r.assert_(isinstance(perms, list), "permission: returns array")
            except Exception:
                r.fail("permission: valid JSON", "parse error")

        ok2, msg = r.assert_no_error(
            lambda: c.sessions.send_message(
                ws_id,
                sid,
                "Create a file called /tmp/canary-test.txt with hello world",
            ),
            "send-message: no error",
        )
        if ok2 and msg is not None:
            r.assert_(len(message_text(msg)) > 0, "send-message: non-empty text")

        pending_found = False
        deadline = time.time() + 30
        while time.time() < deadline:
            try:
                s3, b3 = raw_do(
                    "GET",
                    f"{cfg.api_url}/api/v1/workspaces/{ws_id}/permission",
                    cfg.api_key,
                )
                if s3 == 200:
                    perms_list = json.loads(b3)
                    for p in perms_list:
                        if p.get("status") == "pending":
                            pending_found = True
                            perm_id = p.get("id", "")
                            reply_body = json.dumps(
                                {"reply": "allow", "reason": "canary test"}
                            ).encode()
                            s4, _ = raw_do(
                                "POST",
                                f"{cfg.api_url}/api/v1/workspaces/{ws_id}/permission/{perm_id}/reply",
                                cfg.api_key,
                                reply_body,
                            )
                            r.assert_(s4 in (200, 204), "permission-reply: success", str(s4))
                            break
            except Exception:
                pass
            if pending_found:
                break
            time.sleep(2)

        if not pending_found:
            r.ok("permission: no pending permissions (auto-approved)")

    finally:
        if ws_id:
            try:
                c.workspaces.delete(ws_id)
            except Exception:
                pass


if __name__ == "__main__":
    r = Runner("agent-input")
    cfg = config_from_env()
    run(r, cfg)
    r.print()
    sys.exit(r.exit_code())
