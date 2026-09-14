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
        saw_entries = False
        deadline = time.time() + 30
        while time.time() < deadline:
            # Transport errors retry; SHAPE assertions live OUTSIDE the
            # try so no except can swallow them (r5 f1).
            try:
                s3, b3 = raw_do(
                    "GET",
                    f"{cfg.api_url}/api/v1/workspaces/{ws_id}/permission",
                    cfg.api_key,
                )
            except Exception:
                time.sleep(2)
                continue
            if s3 != 200:
                time.sleep(2)
                continue
            # A non-array body is a shape regression — fail loud.
            try:
                perms_list = json.loads(b3)
            except Exception:
                r.assert_(False, "permission list is JSON", b3[:120].decode("utf-8", "replace"))
                pending_found = True  # stop polling; already failed
                break
            if not isinstance(perms_list, list):
                r.assert_(False, "permission list is an array", type(perms_list).__name__)
                pending_found = True
                break
            if perms_list:
                saw_entries = True
                # The LIVE entry carries the contract shape (4a-2:
                # kind-discriminated, camelCase, no legacy envelope).
                first = perms_list[0]
                r.assert_(first.get("kind") == "permission",
                          "live permission: contract kind field",
                          f"keys={sorted(first.keys())}")
                r.assert_(first.get("session_id") is None,
                          "live permission: NO snake_case",
                          f"keys={sorted(first.keys())}")
                pending_found = True
                perm_id = first.get("id", "")
                # The vocabulary is once/always/reject (the pre-contract
                # "allow" was never valid).
                s4, _ = raw_do(
                    "POST",
                    f"{cfg.api_url}/api/v1/workspaces/{ws_id}/permission/{perm_id}/reply",
                    cfg.api_key,
                    json.dumps({"reply": "once"}).encode(),
                )
                r.assert_(200 <= s4 < 300, "permission-reply: success (2xx; 202 = the #1313 late-answer accept)", str(s4))
                break
            time.sleep(2)

        if not pending_found and saw_entries:
            r.assert_(False, "entries exist but none matched", "shape regression")
        elif not pending_found:
            r.ok("permission: no pending permissions (model did not trigger tool permission)")

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
