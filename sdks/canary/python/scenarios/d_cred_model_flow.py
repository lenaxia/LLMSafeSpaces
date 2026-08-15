#!/usr/bin/env python3
# Copyright (C) 2026 Michael Kao
# SPDX-License-Identifier: AGPL-3.0-or-later
"""D-CRED-MODEL-FLOW canary — Python SDK (flagship end-to-end scenario)"""

from __future__ import annotations

import json
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


def run(run: Runner, cfg: Config) -> None:
    if not cfg.llm_api_key or not cfg.llm_model:
        run.ok("cred-model-flow: skipped (no LLM API key or model)")
        return

    # IMPORTANT: secret injection requires a DEK unlocked by a JWT jti.
    # API key auth has no jti, so SetBindings' pushSecretsToAgent silently
    # fails and the agent never receives the credential.
    # Use JWT login when available; fall back to API key for API-surface-only checks.
    jwt_available = bool(cfg.email and cfg.password)
    if not jwt_available:
        run.ok("cred-model-flow: JWT not configured — agent tests will be skipped")

    if jwt_available:
        c = LLMSafeSpaces(
            cfg.api_url, email=cfg.email, password=cfg.password, timeout=120.0
        )
    else:
        c = LLMSafeSpaces(cfg.api_url, api_key=cfg.api_key, timeout=120.0)
    ws_id = None
    cred_id = None

    try:
        # Step 1: Create workspace, wait Active
        ok, ws = run.assert_no_error(
            lambda: c.workspaces.create(
                name="canary-py-flow", runtime="base", storage_size="1Gi"
            ),
            "create-ws: no error",
        )
        if not ok:
            return
        ws_id = ws.id

        phase = wait_active(c, ws_id)
        run.assert_(phase == "Active", "reach-active", f"got {phase!r}")
        if phase != "Active":
            return

        # Step 2: Create LLM credential
        cred_value = json.dumps(
            {"kind": cfg.llm_provider, "slug": "canary-py-model-flow", "apiKey": cfg.llm_api_key}
        )
        ok2, cred = run.assert_no_error(
            lambda: c.secrets.create(
                name="canary-py-flow-cred", type="llm-provider", value=cred_value
            ),
            "create-cred: no error",
        )
        if not ok2:
            return
        run.assert_(
            cred.type == "llm-provider", "create-cred: type=llm-provider", cred.type
        )
        cred_id = cred.id

        # Step 3: Bind
        run.assert_no_error(
            lambda: c.workspaces.set_bindings(ws_id, [cred_id]),
            "bind-cred: no error",
        )

        # Step 4: Set model via SDK
        run.assert_no_error(
            lambda: c.workspaces.set_model(ws_id, cfg.llm_model),
            "set-model: no error",
        )

        # Steps 5–9 require JWT auth so the DEK is available for secret injection.
        if not jwt_available:
            run.ok("agent-tests: skipped (JWT required for DEK-based secret injection)")
            return

        # Step 5: Ensure session
        try:
            sess = ensure_session_with_retry(c, ws_id, 5)
        except Exception as e:
            run.fail("ensure-session: no error", str(e))
            return
        run.ok("ensure-session: no error")
        session_id = sess.sessionId

        # Step 6: Send message (first session)
        ok3, msg = run.assert_no_error(
            lambda: c.sessions.send_message(
                ws_id, session_id, "Reply with exactly: CRED-FLOW-OK"
            ),
            "send-message: no error",
        )
        if ok3:
            run.assert_(len(msg.content) > 0, "send-message: non-empty content")
            run.assert_(
                "CRED-FLOW-OK" in msg.content.upper(),
                "send-message: contains expected text",
                repr(msg.content[:100]),
            )

        # Step 7: History
        ok4, hist = run.assert_no_error(
            lambda: c.sessions.get_history(ws_id, session_id),
            "history: no error",
        )
        if ok4:
            run.assert_(len(hist) >= 1, "history: ≥1 entry", str(len(hist)))

        # Step 8: Second session (reload simulation)
        try:
            sess2 = c.sessions.ensure(ws_id)
        except Exception as e:
            run.fail("ensure-session-2: no error", str(e))
        else:
            run.ok("ensure-session-2: no error")
            session2_id = sess2.sessionId

            # Step 9: Send to second session
            ok5, msg2 = run.assert_no_error(
                lambda: c.sessions.send_message(
                    ws_id, session2_id, "Reply with exactly: AFTER-RELOAD"
                ),
                "send-message-2: no error",
            )
            if ok5:
                run.assert_(len(msg2.content) > 0, "send-message-2: non-empty")
                run.assert_(
                    "AFTER-RELOAD" in msg2.content.upper(),
                    "send-message-2: contains expected text",
                    repr(msg2.content[:100]),
                )

        # Step 10: Delete credential
        run.assert_no_error(lambda: c.secrets.delete(cred_id), "delete-cred: no error")
        cred_id = None

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
    r = Runner("cred-model-flow")
    cfg = config_from_env()
    run(r, cfg)
    r.print()
    sys.exit(r.exit_code())
