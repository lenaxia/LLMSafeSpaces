#!/usr/bin/env python3
# Copyright (C) 2026 Michael Kao
# SPDX-License-Identifier: AGPL-3.0-or-later
"""D-ENV-INJECTION canary — Python SDK"""

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
)
from llmsafespaces import LLMSafeSpaces
from llmsafespaces.client import message_text


def run(r: Runner, cfg: Config) -> None:
    if not cfg.llm_api_key:
        r.ok("env-injection: skipped (no LLM API key)")
        return

    c = LLMSafeSpaces(cfg.api_url, api_key=cfg.api_key, timeout=120.0)
    ws_id = None
    try:
        ok, ws = r.assert_no_error(
            lambda: c.workspaces.create(
                name="canary-py-env-inject", runtime="base", storage_size="1Gi"
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

        r.assert_no_error(
            lambda: c.workspaces.set_env(ws_id, {"CANARY_INJECT": "canary-xyz"}),
            "set-env: no error",
        )

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
                'Run: python3 -c \'import os; print(os.environ.get("CANARY_INJECT", "NOTFOUND"))\'',
            ),
            "send-message: no error",
        )
        if ok2 and msg is not None:
            r.assert_(len(message_text(msg)) > 0, "send-message: non-empty text")
            r.assert_(
                "canary-xyz" in message_text(msg),
                "env-injected: response contains canary-xyz",
                repr(message_text(msg)[:200]),
            )

        r.assert_no_error(
            lambda: c.workspaces.delete_env(ws_id, "CANARY_INJECT"),
            "delete-env: no error",
        )

        r.assert_no_error(
            lambda: c.workspaces.reload_secrets(ws_id),
            "reload-secrets: no error",
        )

        import time
        time.sleep(2)

        ok3, msg2 = r.assert_no_error(
            lambda: c.sessions.send_message(
                ws_id,
                sid,
                'Run: python3 -c \'import os; print(os.environ.get("CANARY_INJECT", "NOTFOUND"))\'',
            ),
            "send-message-after-delete: no error",
        )
        if ok3 and msg2 is not None:
            r.assert_(
                "NOTFOUND" in message_text(msg2),
                "env-removed: response contains NOTFOUND",
                repr(message_text(msg2)[:200]),
            )

    finally:
        if ws_id:
            try:
                c.workspaces.delete(ws_id)
            except Exception:
                pass


if __name__ == "__main__":
    r = Runner("env-injection")
    cfg = config_from_env()
    run(r, cfg)
    r.print()
    sys.exit(r.exit_code())
