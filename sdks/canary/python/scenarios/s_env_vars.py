#!/usr/bin/env python3
# Copyright (C) 2026 Michael Kao
# SPDX-License-Identifier: AGPL-3.0-or-later
"""S-ENV-VARS canary — Python SDK"""

from __future__ import annotations
import sys, os

sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), ".."))
from canary import Runner, Config, config_from_env, jwt_login, raw_do
from llmsafespaces import LLMSafeSpaces


def run(r: Runner, cfg: Config) -> None:
    c = LLMSafeSpaces(cfg.api_url, api_key=jwt_login(cfg), timeout=20.0)
    ws_id = None
    try:
        ok, ws = r.assert_no_error(
            lambda: c.workspaces.create(
                name="canary-py-envvars", runtime="base", storage_size="1Gi"
            ),
            "create-ws: no error",
        )
        if not ok:
            return
        ws_id = ws.id

        # P1: Set
        r.assert_no_error(
            lambda: c.workspaces.set_env(ws_id, {"CANARY_VAR": "hello"}),
            "set-env: no error",
        )

        # P2: Get — contains CANARY_VAR
        ok2, env = r.assert_no_error(
            lambda: c.workspaces.get_env(ws_id), "get-env: no error"
        )
        if ok2 and env is not None:
            r.assert_(
                "CANARY_VAR" in env.get("vars", []), "get-env: CANARY_VAR present"
            )

        # P3: Upsert
        r.assert_no_error(
            lambda: c.workspaces.set_env(ws_id, {"CANARY_VAR": "updated"}),
            "upsert-env: no error",
        )

        # P4: Delete
        r.assert_no_error(
            lambda: c.workspaces.delete_env(ws_id, "CANARY_VAR"), "delete-env: no error"
        )

        # P5: Absent after delete
        ok3, env2 = r.assert_no_error(
            lambda: c.workspaces.get_env(ws_id), "get-after-delete: no error"
        )
        if ok3 and env2 is not None:
            r.assert_(
                "CANARY_VAR" not in env2.get("vars", []),
                "get-after-delete: CANARY_VAR absent",
            )

        # N2: missing vars body
        s, _ = raw_do(
            "PUT", f"{cfg.api_url}/api/v1/workspaces/{ws_id}/env", cfg.api_key, b"{}"
        )
        r.assert_(s == 400, "set-env-no-vars: 400", str(s))

        # N3: delete nonexistent var
        s2, _ = raw_do(
            "DELETE",
            f"{cfg.api_url}/api/v1/workspaces/{ws_id}/env/NONEXISTENT_XYZ",
            cfg.api_key,
        )
        r.assert_(s2 == 404, "delete-nonexistent-var: 404", str(s2))

    finally:
        if ws_id:
            try:
                c.workspaces.delete(ws_id)
            except:
                pass


if __name__ == "__main__":
    r = Runner("env-vars")
    cfg = config_from_env()
    run(r, cfg)
    r.print()
    sys.exit(r.exit_code())
