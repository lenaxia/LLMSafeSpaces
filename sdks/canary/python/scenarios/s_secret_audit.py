#!/usr/bin/env python3
# Copyright (C) 2026 Michael Kao
# SPDX-License-Identifier: AGPL-3.0-or-later
"""S-SECRET-AUDIT canary — Python SDK"""

from __future__ import annotations
import sys, os

sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), ".."))
from canary import Runner, Config, config_from_env, jwt_login, raw_do
from llmsafespaces import LLMSafeSpaces


def run(r: Runner, cfg: Config) -> None:
    c = LLMSafeSpaces(cfg.api_url, api_key=jwt_login(cfg), timeout=20.0)
    sid = None
    try:
        ok, s = r.assert_no_error(
            lambda: c.secrets.create(
                name="canary-py-audit", type="env-secret", value="v", metadata={"var_name": "CANARY_PY_VAR"}
            ),
            "create-for-audit: no error",
        )
        if ok:
            sid = s.id

        ok2, entries = r.assert_no_error(
            lambda: c.secrets.get_audit_log(), "get-audit: no error"
        )
        if ok2:
            r.assert_(entries is not None, "get-audit: entries non-nil")
            if sid:
                found = next((e for e in entries if e.get("secretId") == sid), None)
                r.assert_(
                    found is not None, "get-audit: entry for created secret present"
                )
                if found:
                    r.assert_(
                        found.get("action") not in (None, ""),
                        "audit-entry: action field",
                    )
                    r.assert_(
                        found.get("userId") not in (None, ""),
                        "audit-entry: userId field",
                    )

    finally:
        if sid:
            try:
                c.secrets.delete(sid)
            except:
                pass

    # N1: no auth
    s, _ = raw_do("GET", cfg.api_url + "/api/v1/secrets/audit", "")
    r.assert_(s == 401, "audit-no-auth: 401", str(s))


if __name__ == "__main__":
    r = Runner("secret-audit")
    cfg = config_from_env()
    run(r, cfg)
    r.print()
    sys.exit(r.exit_code())
