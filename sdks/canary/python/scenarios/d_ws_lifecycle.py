#!/usr/bin/env python3
# Copyright (C) 2026 Michael Kao
# SPDX-License-Identifier: AGPL-3.0-or-later
"""D-WS-LIFECYCLE canary — Python SDK"""

from __future__ import annotations
import sys, os

sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), ".."))
from canary import Runner, Config, config_from_env, wait_active, wait_phase
from llmsafespaces import LLMSafeSpaces


def run(r: Runner, cfg: Config) -> None:
    c = LLMSafeSpaces(cfg.api_url, api_key=cfg.api_key, timeout=60.0)
    ws_id = None
    try:
        ok, ws = r.assert_no_error(
            lambda: c.workspaces.create(
                name="canary-py-lifecycle", runtime="base", storage_size="1Gi"
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

        # Status fields on Active
        ok2, st = r.assert_no_error(
            lambda: c.workspaces.get_status(ws_id), "get-status-active: no error"
        )
        if ok2 and st is not None:
            r.assert_(
                st.get("imageTag", "") != "",
                "status-active: imageTag non-empty",
                st.get("imageTag"),
            )
            r.assert_(
                st.get("agentHealth", {}).get("agentVersion", "") != "",
                "status-active: agentVersion non-empty",
            )
            r.assert_(
                len(st.get("conditions", [])) > 0, "status-active: conditions non-empty"
            )
            r.assert_(
                st.get("agentHealth", {}).get("status") == "Healthy",
                "status-active: agentHealth=Healthy",
                st.get("agentHealth", {}).get("status"),
            )
            r.assert_(
                st.get("diskTotalBytes", 0) > 0, "status-active: diskTotalBytes > 0"
            )

        # Suspend
        r.assert_no_error(lambda: c.workspaces.suspend(ws_id), "suspend: no error")
        sp = wait_phase(c, ws_id, "Suspended", 60)
        r.assert_(sp == "Suspended", "suspend: phase=Suspended", f"got {sp!r}")

        # Double-suspend → error (409)
        r.assert_error(
            lambda: c.workspaces.suspend(ws_id), "double-suspend: 409 Conflict"
        )

        # Activate
        r.assert_no_error(lambda: c.workspaces.activate(ws_id), "activate: no error")
        rp = wait_active(c, ws_id, 120)
        r.assert_(rp == "Active", "activate: phase=Active", f"got {rp!r}")

        # Activate already-Active → idempotent
        r.assert_no_error(
            lambda: c.workspaces.activate(ws_id), "activate-already-active: no error"
        )

        # Restart
        r.assert_no_error(lambda: c.workspaces.restart(ws_id), "restart: no error")
        rp2 = wait_active(c, ws_id, 150)
        r.assert_(rp2 == "Active", "restart: returns to Active", f"got {rp2!r}")

    finally:
        if ws_id:
            try:
                c.workspaces.delete(ws_id)
            except:
                pass


if __name__ == "__main__":
    r = Runner("ws-lifecycle")
    cfg = config_from_env()
    run(r, cfg)
    r.print()
    sys.exit(r.exit_code())
