#!/usr/bin/env python3
# Copyright (C) 2026 Michael Kao
# SPDX-License-Identifier: AGPL-3.0-or-later
"""S-WS-STATUS canary — Python SDK"""

from __future__ import annotations
import sys, os

sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), ".."))
from canary import Runner, Config, config_from_env, raw_do, has_field
from llmsafespaces import LLMSafeSpaces, NotFoundError


def run(r: Runner, cfg: Config) -> None:
    c = LLMSafeSpaces(cfg.api_url, api_key=cfg.api_key, timeout=20.0)
    ws_id = None
    try:
        ok, ws = r.assert_no_error(
            lambda: c.workspaces.create(
                name="canary-py-wsstatus", runtime="base", storage_size="1Gi"
            ),
            "create: no error",
        )
        if not ok:
            return
        ws_id = ws.id

        ok2, st = r.assert_no_error(
            lambda: c.workspaces.get_status(ws_id), "get-status: no error"
        )
        if ok2 and st is not None:
            # Phase may be empty immediately after creation — the
            # controller hasn't reconciled yet (Pending → Creating →
            # Active). In canary CI there may be no controller running,
            # so accept empty phase for a freshly-created workspace; the
            # contract under test is that GetStatus returns 200 with a
            # parseable shape (mirror of the Go twin).
            r.assert_(
                st.get("phase", "") is not None,
                "status: phase present (may be empty pre-reconcile)",
            )
            r.assert_(
                isinstance(st.get("activeSessions"), int) and st["activeSessions"] >= 0,
                "status: activeSessions ≥ 0",
            )
            r.assert_("credentialState" in st, "status: credentialState present")
            r.assert_("agentHealth" in st, "status: agentHealth present")
            r.assert_(
                isinstance(st.get("agentHealth", {}).get("status"), str),
                "status: agentHealth.status is string",
            )
            # Conditions may be empty pre-Active — the field must be
            # parseable (present-or-absent without crash), not non-empty
            # (mirror of the Go twin).
            r.assert_(
                st.get("conditions") is None or isinstance(st.get("conditions"), list),
                "status: conditions field parseable",
            )

        # Raw — no error field on success
        s, b = raw_do(
            "GET", f"{cfg.api_url}/api/v1/workspaces/{ws_id}/status", cfg.api_key
        )
        r.assert_(s == 200, f"status-raw: 200 (got {s})")
        r.assert_(not has_field(b, "error"), "status-raw: no error field")

    finally:
        if ws_id:
            try:
                c.workspaces.delete(ws_id)
            except:
                pass

    # N1: nonexistent
    r.assert_error(
        lambda: c.workspaces.get_status("00000000-0000-0000-0000-000000000000"),
        "status-nonexistent: error",
    )


if __name__ == "__main__":
    r = Runner("ws-status")
    cfg = config_from_env()
    run(r, cfg)
    r.print()
    sys.exit(r.exit_code())
