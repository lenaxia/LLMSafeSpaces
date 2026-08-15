#!/usr/bin/env python3
# Copyright (C) 2026 Michael Kao
# SPDX-License-Identifier: AGPL-3.0-or-later
"""D-TERMINAL canary — Python SDK"""

from __future__ import annotations

import sys
import os

sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), ".."))

from canary import Runner, Config, config_from_env, wait_active
from llmsafespaces import LLMSafeSpaces


def run(r: Runner, cfg: Config) -> None:
    c = LLMSafeSpaces(cfg.api_url, api_key=cfg.api_key, timeout=60.0)
    ws_id = None
    try:
        ok, ws = r.assert_no_error(
            lambda: c.workspaces.create(
                name="canary-py-terminal", runtime="base", storage_size="1Gi"
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

        ok2, ticket = r.assert_no_error(
            lambda: c.terminal.get_ticket(ws_id), "get-ticket: no error"
        )
        if ok2 and ticket is not None:
            r.assert_(
                ticket.ticket.startswith("tkt_"),
                "ticket: starts with tkt_",
                repr(ticket.ticket[:10]),
            )
            r.assert_(
                len(ticket.ticket) > 10,
                "ticket: length > 10",
                str(len(ticket.ticket)),
            )
            r.assert_(
                ticket.expiresAt is not None and len(ticket.expiresAt) > 0,
                "ticket: has expiresAt",
            )

        ok3, ticket2 = r.assert_no_error(
            lambda: c.terminal.get_ticket(ws_id), "get-ticket-2: no error"
        )
        if ok3 and ok2 and ticket2 is not None and ticket is not None:
            r.assert_(
                ticket.ticket != ticket2.ticket,
                "tickets-differ: two tickets are unique",
            )

        r.assert_error(
            lambda: c.terminal.get_ticket("00000000-0000-0000-0000-000000000000"),
            "nonexistent-ws: error",
        )

    finally:
        if ws_id:
            try:
                c.workspaces.delete(ws_id)
            except Exception:
                pass


if __name__ == "__main__":
    r = Runner("terminal")
    cfg = config_from_env()
    run(r, cfg)
    r.print()
    sys.exit(r.exit_code())
