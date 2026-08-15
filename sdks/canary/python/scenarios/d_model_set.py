#!/usr/bin/env python3
# Copyright (C) 2026 Michael Kao
# SPDX-License-Identifier: AGPL-3.0-or-later
"""D-MODEL-SET canary — Python SDK"""

from __future__ import annotations

import json
import sys
import os

sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), ".."))

from canary import (
    jwt_login,
    Runner,
    Config,
    config_from_env,
    wait_active,
    ensure_session_with_retry,
    raw_do,
)
from llmsafespaces import LLMSafeSpaces


def run(r: Runner, cfg: Config) -> None:
    if not cfg.llm_api_key or not cfg.llm_model:
        r.ok("model-set: skipped (no LLM API key or model)")
        return

    c = LLMSafeSpaces(cfg.api_url, api_key=jwt_login(cfg), timeout=120.0)
    ws_id = None
    cred_id = None
    try:
        ok, ws = r.assert_no_error(
            lambda: c.workspaces.create(
                name="canary-py-model-set", runtime="base", storage_size="1Gi"
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

        cred_value = json.dumps(
            {
                "kind": cfg.llm_provider,
                "slug": "canary-py-model-set",
                "apiKey": cfg.llm_api_key,
            }
        )
        ok_cred, cred = r.assert_no_error(
            lambda: c.secrets.create(
                name="canary-py-model-set-cred", type="llm-provider", value=cred_value
            ),
            "create-cred: no error",
        )
        if ok_cred and cred:
            cred_id = cred.id
            r.assert_no_error(
                lambda: c.workspaces.set_bindings(ws_id, [cred_id]),
                "bind-cred: no error",
            )

        r.assert_no_error(
            lambda: c.workspaces.set_model(ws_id, cfg.llm_model),
            "set-model: no error",
        )

        try:
            sess = ensure_session_with_retry(c, ws_id, 5)
        except Exception as e:
            r.fail("ensure-session: no error", str(e))
            return
        r.ok("ensure-session: no error")
        sid = sess.sessionId

        ok2, msg = r.assert_no_error(
            lambda: c.sessions.send_message(ws_id, sid, "Reply with exactly: MODEL-OK"),
            "send-message: no error",
        )
        if ok2 and msg is not None:
            r.assert_(len(msg.content) > 0, "send-message: non-empty content")
            r.assert_(
                "MODEL-OK" in msg.content.upper(),
                "send-message: contains MODEL-OK",
                repr(msg.content[:100]),
            )

        # Negative: empty model
        s1, _ = raw_do(
            "PUT",
            f"{cfg.api_url}/api/v1/workspaces/{ws_id}/model",
            cfg.api_key,
            json.dumps({"model": ""}).encode(),
        )
        r.assert_(s1 in (400, 422), "empty-model: 400 or 422", str(s1))

        # Negative: nonexistent workspace
        s2, _ = raw_do(
            "PUT",
            f"{cfg.api_url}/api/v1/workspaces/00000000-0000-0000-0000-000000000000/model",
            cfg.api_key,
            json.dumps({"model": "test/model"}).encode(),
        )
        r.assert_(s2 == 404, "nonexistent-ws-model: 404", str(s2))

        # Negative: bad model — verify ws still Active
        ok3, ws_check = r.assert_no_error(
            lambda: c.workspaces.get(ws_id), "ws-still-exists: no error"
        )
        if ok3 and ws_check:
            r.assert_(
                ws_check.phase == "Active",
                "after-bad-model: ws still Active",
                ws_check.phase,
            )

    finally:
        if cred_id:
            try:
                c.secrets.delete(cred_id)
            except Exception:
                pass
        if ws_id:
            try:
                c.workspaces.delete(ws_id)
            except Exception:
                pass


if __name__ == "__main__":
    r = Runner("model-set")
    cfg = config_from_env()
    run(r, cfg)
    r.print()
    sys.exit(r.exit_code())
