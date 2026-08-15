#!/usr/bin/env python3
# Copyright (C) 2026 Michael Kao
# SPDX-License-Identifier: AGPL-3.0-or-later
"""S-OWNERSHIP canary — Python SDK"""

from __future__ import annotations
import sys, os

sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), ".."))
from canary import Runner, Config, config_from_env, jwt_login, raw_do
from llmsafespaces import LLMSafeSpaces


def run(r: Runner, cfg: Config) -> None:
    if not cfg.api_key_user2:
        r.ok("ownership: skipped (no LLMSAFESPACES_API_KEY_USER2)")
        return

    c1 = LLMSafeSpaces(cfg.api_url, api_key=jwt_login(cfg), timeout=20.0)
    c2 = LLMSafeSpaces(cfg.api_url, api_key=cfg.api_key_user2, timeout=20.0)
    ws1_id = ws2_id = s1_id = None
    try:
        ok, ws1 = r.assert_no_error(
            lambda: c1.workspaces.create(
                name="canary-py-own-u1", runtime="base", storage_size="1Gi"
            ),
            "user1-create-ws",
        )
        if not ok:
            return
        ws1_id = ws1.id

        ok2, s1 = r.assert_no_error(
            lambda: c1.secrets.create(
                name="canary-py-own-s1", type="env-secret", value="v", metadata={"var_name": "CANARY_PY_VAR"}
            ),
            "user1-create-secret",
        )
        if ok2:
            s1_id = s1.id

        ok3, ws2 = r.assert_no_error(
            lambda: c2.workspaces.create(
                name="canary-py-own-u2", runtime="base", storage_size="1Gi"
            ),
            "user2-create-ws",
        )
        if not ok3:
            return
        ws2_id = ws2.id

        # P1+P2: each user sees their own workspace
        r.assert_no_error(lambda: c1.workspaces.get(ws1_id), "user1-get-own")
        r.assert_no_error(lambda: c2.workspaces.get(ws2_id), "user2-get-own")

        # P3+P4: list isolation
        ok4, l1 = r.assert_no_error(lambda: c1.workspaces.list(), "user1-list")
        if ok4 and l1 is not None:
            r.assert_(any(i.id == ws1_id for i in l1.items), "user1-list: W1 present")
            r.assert_(
                not any(i.id == ws2_id for i in l1.items), "user1-list: W2 absent"
            )

        ok5, l2 = r.assert_no_error(lambda: c2.workspaces.list(), "user2-list")
        if ok5 and l2 is not None:
            r.assert_(
                not any(i.id == ws1_id for i in l2.items), "user2-list: W1 absent"
            )
            r.assert_(any(i.id == ws2_id for i in l2.items), "user2-list: W2 present")

        # N1: user2 GET user1's workspace → 403 (ForbiddenError, not 404)
        # Validated: verifyOwner returns NewForbiddenError → HTTP 403.
        r.assert_error(
            lambda: c2.workspaces.get(ws1_id), "user2-get-user1-ws: 403 Forbidden"
        )

        # N2: user2 DELETE user1's workspace → error (403)
        r.assert_error(
            lambda: c2.workspaces.delete(ws1_id), "user2-delete-user1-ws: error"
        )

        # N3: user2 GET status of user1's workspace → 403
        r.assert_error(
            lambda: c2.workspaces.get_status(ws1_id), "user2-status-user1-ws: 403"
        )

        # N4: user2 GET user1's secret
        if s1_id:
            r.assert_error(
                lambda: c2.secrets.get(s1_id), "user2-get-user1-secret: error"
            )

        # N5: user2 ensure session on user1's workspace
        r.assert_error(
            lambda: c2.sessions.ensure(ws1_id), "user2-ensure-session-user1: error"
        )

        s, _ = raw_do(
            "GET",
            f"{cfg.api_url}/api/v1/workspaces/{ws1_id}/bindings",
            cfg.api_key_user2,
        )
        # N6: 403 (workspace ownership middleware denies before the secrets
        # handler) or 404 — the Go twin's access-denied contract.
        r.assert_(s in (403, 404), "user2-bindings-user1: 403/404 (access denied)", str(s))

    finally:
        for ws_id, client in [(ws1_id, c1), (ws2_id, c2)]:
            if ws_id:
                try:
                    client.workspaces.delete(ws_id)
                except:
                    pass
        if s1_id:
            try:
                c1.secrets.delete(s1_id)
            except:
                pass


if __name__ == "__main__":
    r = Runner("ownership")
    cfg = config_from_env()
    run(r, cfg)
    r.print()
    sys.exit(r.exit_code())
