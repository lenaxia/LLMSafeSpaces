#!/usr/bin/env python3
# Copyright (C) 2026 Michael Kao
# SPDX-License-Identifier: AGPL-3.0-or-later
"""S-CRED-CRUD canary — Python SDK"""

from __future__ import annotations
import json, sys, os

sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), ".."))
from canary import jwt_login, Runner, Config, config_from_env
from llmsafespaces import LLMSafeSpaces


def run(r: Runner, cfg: Config) -> None:
    c = LLMSafeSpaces(cfg.api_url, api_key=jwt_login(cfg), timeout=20.0)
    cred_id = None
    cred_value = json.dumps(
        {"kind": cfg.llm_provider, "slug": "canary-py-cred", "apiKey": "sk-canary-placeholder-00000000"}
    )
    try:
        ok, cred = r.assert_no_error(
            lambda: c.secrets.create(
                name="canary-py-llm-cred", type="llm-provider", value=cred_value
            ),
            "create-cred: no error",
        )
        if not ok:
            return
        r.assert_(
            cred.type == "llm-provider", "create-cred: type=llm-provider", cred.type
        )
        cred_id = cred.id

        ok2, lst = r.assert_no_error(lambda: c.secrets.list(), "list-creds: no error")
        if ok2:
            r.assert_(
                any(s.id == cred_id for s in lst), "list-creds: credential present"
            )

        r.assert_no_error(lambda: c.secrets.delete(cred_id), "delete-cred: no error")
        cred_id = None

        ok3, lst2 = r.assert_no_error(
            lambda: c.secrets.list(), "list-after-delete: no error"
        )
        if ok3:
            r.assert_(all(s.id != cred.id for s in lst2), "list-after-delete: absent")

    finally:
        if cred_id:
            try:
                c.secrets.delete(cred_id)
            except:
                pass

    r.assert_error(
        lambda: c.secrets.delete("00000000-0000-0000-0000-000000000097"),
        "delete-nonexistent: error",
    )
    r.assert_error(
        lambda: c.secrets.create(
            name="canary-py-bad-cred", type="llm-provider", value="not-valid-json"
        ),
        "create-malformed-cred: error",
    )


if __name__ == "__main__":
    r = Runner("cred-crud")
    cfg = config_from_env()
    run(r, cfg)
    r.print()
    sys.exit(r.exit_code())
