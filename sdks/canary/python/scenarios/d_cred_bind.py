#!/usr/bin/env python3
# Copyright (C) 2026 Michael Kao
# SPDX-License-Identifier: AGPL-3.0-or-later
"""D-CRED-BIND canary — Python SDK"""

from __future__ import annotations
import json, sys, os

sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), ".."))
from canary import jwt_login, Runner, Config, config_from_env, wait_active, wait_phase
from llmsafespaces import LLMSafeSpaces


def run(r: Runner, cfg: Config) -> None:
    c = LLMSafeSpaces(cfg.api_url, api_key=jwt_login(cfg), timeout=60.0)
    ws_id = cred_id = None
    try:
        ok, ws = r.assert_no_error(
            lambda: c.workspaces.create(
                name="canary-py-cred-bind", runtime="base", storage_size="1Gi"
            ),
            "create-ws: no error",
        )
        if not ok:
            return
        ws_id = ws.id

        phase = wait_active(c, ws_id)
        r.assert_(phase == "Active", "reach-active", f"got {phase!r}")
        if phase != "Active":
            return

        cred_value = json.dumps(
            {"kind": cfg.llm_provider, "slug": "canary-py-bind", "apiKey": "sk-canary-placeholder"}
        )
        ok2, cred = r.assert_no_error(
            lambda: c.secrets.create(
                name="canary-py-cred-bind-s", type="llm-provider", value=cred_value
            ),
            "create-cred: no error",
        )
        if not ok2:
            return
        cred_id = cred.id

        # Bind
        r.assert_no_error(
            lambda: c.workspaces.set_bindings(ws_id, [cred_id]), "bind-cred: no error"
        )

        # Get bindings
        ok3, b = r.assert_no_error(
            lambda: c.workspaces.get_bindings(ws_id), "get-bindings: no error"
        )
        if ok3 and b is not None:
            r.assert_(
                any(x.get("id") == cred_id for x in b.get("bindings", [])),
                "get-bindings: cred present",
            )

        # Reload → pod-reported outcome (US-70.3: status, not a count)
        ok4, result = r.assert_no_error(
            lambda: c.workspaces.reload_secrets(ws_id), "reload-secrets: no error"
        )
        if ok4 and result is not None:
            r.assert_(
                result.get("status") in ("applied", "not_modified"),
                "reload-secrets: status ∈ {applied, not_modified}",
                str(result.get("status")),
            )

        # Status credentialState.available = true
        ok5, st = r.assert_no_error(
            lambda: c.workspaces.get_status(ws_id), "status-after-reload: no error"
        )
        if ok5 and st is not None:
            r.assert_(
                st.get("credentialState", {}).get("available") is True,
                "status: credentialState.available=true",
            )

        # Unbind then reload-empty → still a valid pod outcome (not an error)
        r.assert_no_error(
            lambda: c.workspaces.set_bindings(ws_id, []), "unbind: no error"
        )
        ok6, er = r.assert_no_error(
            lambda: c.workspaces.reload_secrets(ws_id), "reload-after-unbind: no error"
        )
        if ok6 and er is not None:
            r.assert_(
                er.get("status") in ("applied", "not_modified"),
                "reload-after-unbind: status ∈ {applied, not_modified}",
                str(er.get("status")),
            )

        # Reload on suspended → error
        r.assert_no_error(lambda: c.workspaces.suspend(ws_id), "suspend: no error")
        wait_phase(c, ws_id, "Suspended", 60)
        r.assert_error(
            lambda: c.workspaces.reload_secrets(ws_id),
            "reload-suspended: error (no running pod)",
        )

    finally:
        if cred_id:
            try:
                c.secrets.delete(cred_id)
            except:
                pass
        if ws_id:
            try:
                c.workspaces.delete(ws_id)
            except:
                pass


if __name__ == "__main__":
    r = Runner("cred-bind")
    cfg = config_from_env()
    run(r, cfg)
    r.print()
    sys.exit(r.exit_code())
