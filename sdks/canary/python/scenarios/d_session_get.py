#!/usr/bin/env python3
# Copyright (C) 2026 Michael Kao
# SPDX-License-Identifier: AGPL-3.0-or-later
"""D-SESSION-GET canary — Python SDK"""

from __future__ import annotations

import sys
import os

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
    c = LLMSafeSpaces(cfg.api_url, api_key=cfg.api_key, timeout=60.0)
    ws_id = None
    try:
        ok, ws = r.assert_no_error(
            lambda: c.workspaces.create(
                name="canary-py-sess-get", runtime="base", storage_size="1Gi"
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

        ok2, sess_obj = r.assert_no_error(
            lambda: c.sessions.get(ws_id, sid), "get-session: no error"
        )
        if ok2 and sess_obj is not None:
            r.assert_("id" in sess_obj, "get-session: has id")
            r.assert_("title" in sess_obj, "get-session: has title")

        r.assert_no_error(
            lambda: c.sessions.rename(ws_id, sid, "canary-py-renamed"),
            "rename: no error",
        )

        ok3, sess_after = r.assert_no_error(
            lambda: c.sessions.get(ws_id, sid), "get-after-rename: no error"
        )
        if ok3 and sess_after is not None:
            r.assert_(
                sess_after.get("title") == "canary-py-renamed",
                "get-after-rename: title updated",
                repr(sess_after.get("title")),
            )

        r.assert_error(
            lambda: c.sessions.get(ws_id, "ses_nonexistent000000"),
            "get-nonexistent-session: error",
        )

        s, _ = raw_do(
            "GET",
            f"{cfg.api_url}/api/v1/workspaces/{ws_id}/sessions/..%2Fetc",
            cfg.api_key,
        )
        r.assert_(s in (400, 404), "path-traversal-session: 400 or 404", str(s))

    finally:
        if ws_id:
            try:
                c.workspaces.delete(ws_id)
            except Exception:
                pass


if __name__ == "__main__":
    r = Runner("session-get")
    cfg = config_from_env()
    run(r, cfg)
    r.print()
    sys.exit(r.exit_code())
