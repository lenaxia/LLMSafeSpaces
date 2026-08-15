#!/usr/bin/env python3
# Copyright (C) 2026 Michael Kao
# SPDX-License-Identifier: AGPL-3.0-or-later
"""D-SESSION-ENSURE canary — Python SDK"""

from __future__ import annotations
import sys, os

sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), ".."))
from canary import (
    Runner,
    Config,
    config_from_env,
    wait_active,
    wait_phase,
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
                name="canary-py-sess-ensure", runtime="base", storage_size="1Gi"
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

        # Ensure on Active → resumed=False
        try:
            sess = ensure_session_with_retry(c, ws_id, 5)
            r.ok("ensure-active: no error")
        except Exception as e:
            r.fail("ensure-active: no error", str(e))
            return
        r.assert_(sess.sessionId != "", "ensure-active: sessionId present")
        r.assert_(not sess.resumed, "ensure-active: resumed=false")
        sid = sess.sessionId

        # Suspend then ensure → auto-resume
        r.assert_no_error(lambda: c.workspaces.suspend(ws_id), "suspend: no error")
        wait_phase(c, ws_id, "Suspended", 60)
        try:
            sess2 = ensure_session_with_retry(c, ws_id, 10)
            r.ok("ensure-suspended: no error (auto-resume)")
            r.assert_(sess2.resumed, "ensure-suspended: resumed=true")
            r.assert_(
                sess2.workspacePhase == "Active",
                "ensure-suspended: workspacePhase=Active",
                sess2.workspacePhase,
            )
        except Exception as e:
            r.fail("ensure-suspended: no error", str(e))

        # List sessions
        ok2, lst = r.assert_no_error(
            lambda: c.sessions.list(ws_id), "list-sessions: no error"
        )
        if ok2:
            r.assert_(lst is not None, "list-sessions: array")

        # Rename
        r.assert_no_error(
            lambda: c.sessions.rename(ws_id, sid, "canary-py-title"),
            "rename-session: no error",
        )

        # GET individual session
        ok3, sess_obj = r.assert_no_error(
            lambda: c.sessions.get(ws_id, sid), "get-session: no error"
        )
        if ok3 and sess_obj is not None:
            r.assert_(sess_obj.get("id") is not None, "get-session: id present")

        # Abort idle
        r.assert_no_error(lambda: c.sessions.abort(ws_id, sid), "abort: no error")

        # N1: nonexistent workspace
        r.assert_error(
            lambda: c.sessions.ensure("00000000-0000-0000-0000-000000000000"),
            "ensure-nonexistent-ws: error",
        )

        # Path traversal
        s, _ = raw_do(
            "GET",
            f"{cfg.api_url}/api/v1/workspaces/{ws_id}/sessions/..%2Fetc/message",
            cfg.api_key,
        )
        r.assert_(s in (400, 404), "path-traversal: 400 or 404", str(s))

    finally:
        if ws_id:
            try:
                c.workspaces.delete(ws_id)
            except:
                pass


if __name__ == "__main__":
    r = Runner("session-ensure")
    cfg = config_from_env()
    run(r, cfg)
    r.print()
    sys.exit(r.exit_code())
