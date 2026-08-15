#!/usr/bin/env python3
# Copyright (C) 2026 Michael Kao
# SPDX-License-Identifier: AGPL-3.0-or-later
"""S-SECRET-BINDINGS canary — Python SDK"""

from __future__ import annotations
import sys, os

sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), ".."))
from canary import Runner, Config, config_from_env, jwt_login
from llmsafespaces import LLMSafeSpaces


def run(r: Runner, cfg: Config) -> None:
    c = LLMSafeSpaces(cfg.api_url, api_key=jwt_login(cfg), timeout=20.0)
    ws_id = sid = None
    try:
        ok, ws = r.assert_no_error(
            lambda: c.workspaces.create(
                name="canary-py-bindings", runtime="base", storage_size="1Gi"
            ),
            "create-ws: no error",
        )
        if not ok:
            return
        ws_id = ws.id

        ok2, s = r.assert_no_error(
            lambda: c.secrets.create(
                name="canary-py-bind-secret", type="env-secret", value="v", metadata={"var_name": "CANARY_PY_VAR"}
            ),
            "create-secret: no error",
        )
        if not ok2:
            return
        sid = s.id

        # P1: Bind
        r.assert_no_error(
            lambda: c.workspaces.set_bindings(ws_id, [sid]), "set-bindings: no error"
        )

        # P2: Get — contains secret
        ok3, b = r.assert_no_error(
            lambda: c.workspaces.get_bindings(ws_id), "get-bindings: no error"
        )
        if ok3 and b is not None:
            bindings = b.get("bindings", [])
            r.assert_(
                any(x.get("id") == sid for x in bindings),
                "get-bindings: secret present",
            )

        # P3: Idempotent re-bind
        r.assert_no_error(
            lambda: c.workspaces.set_bindings(ws_id, [sid]), "rebind-same: idempotent"
        )
        ok4, b2 = r.assert_no_error(
            lambda: c.workspaces.get_bindings(ws_id),
            "get-bindings-after-rebind: no error",
        )
        if ok4 and b2 is not None:
            count = sum(1 for x in b2.get("bindings", []) if x.get("id") == sid)
            r.assert_(count == 1, "rebind-same: exactly 1 entry", str(count))

        # P4+P5: Clear
        r.assert_no_error(
            lambda: c.workspaces.set_bindings(ws_id, []), "clear-bindings: no error"
        )
        ok5, empty = r.assert_no_error(
            lambda: c.workspaces.get_bindings(ws_id), "get-empty-bindings: no error"
        )
        if ok5 and empty is not None:
            r.assert_(len(empty.get("bindings", [])) == 0, "clear-bindings: empty")

        # P6: get secret bindings
        r.assert_no_error(
            lambda: c.secrets.get_bindings_for_secret(sid),
            "get-secret-bindings: no error",
        )

    finally:
        if sid:
            try:
                c.secrets.delete(sid)
            except:
                pass
        if ws_id:
            try:
                c.workspaces.delete(ws_id)
            except:
                pass

    r.assert_error(
        lambda: c.workspaces.set_bindings("00000000-0000-0000-0000-000000000000", []),
        "bind-nonexistent-ws: error",
    )


if __name__ == "__main__":
    r = Runner("secret-bindings")
    cfg = config_from_env()
    run(r, cfg)
    r.print()
    sys.exit(r.exit_code())
