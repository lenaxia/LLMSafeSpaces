#!/usr/bin/env python3
# Copyright (C) 2026 Michael Kao
# SPDX-License-Identifier: AGPL-3.0-or-later
"""D-CHANGE-PASSWORD canary — Python SDK"""

from __future__ import annotations

import sys
import os

sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), ".."))

from canary import Runner, Config, config_from_env
from llmsafespaces import LLMSafeSpaces, AuthError


def run(r: Runner, cfg: Config) -> None:
    email = os.environ.get("LLMSAFESPACES_ROTATE_EMAIL", "canary-rotate@llmsafespaces.test")
    password = os.environ.get("LLMSAFESPACES_ROTATE_PASSWORD", "canary-rotate-password!")
    new_password = "canary-new-pw-12345!"

    c = LLMSafeSpaces(cfg.api_url, email=email, password=password, timeout=30.0)
    secret_id = None
    try:
        ok_me, me = r.assert_no_error(lambda: c.auth.me(), "login: no error")
        if not ok_me:
            return

        ok, secret = r.assert_no_error(
            lambda: c.secrets.create(
                name="canary-py-chpw", type="env-secret", value="chpw-test-value", metadata={"var_name": "CANARY_PY_VAR"}
            ),
            "create-secret: no error",
        )
        if not ok:
            return
        secret_id = secret.id

        r.assert_no_error(
            lambda: c.account.change_password(password, new_password),
            "change-password: no error",
        )

        c2 = LLMSafeSpaces(cfg.api_url, email=email, password=new_password, timeout=30.0)
        ok2, me2 = r.assert_no_error(lambda: c2.auth.me(), "login-new-pw: no error")
        r.assert_(ok2, "login-new-pw: succeeds")

        r.assert_error(
            lambda: LLMSafeSpaces(
                cfg.api_url, email=email, password=password, timeout=10.0
            ).auth.me(),
            "login-old-pw: error (401)",
        )

        ok3, val = r.assert_no_error(
            lambda: c2.secrets.reveal(secret_id, new_password),
            "reveal-new-pw: no error",
        )
        if ok3:
            r.assert_(val == "chpw-test-value", "reveal-new-pw: value matches", repr(val))

        r.assert_no_error(
            lambda: c2.account.change_password(new_password, password),
            "change-back: no error",
        )

        # Negative: wrong old password
        r.assert_error(
            lambda: c.account.change_password("definitely-wrong-xyz", new_password),
            "change-wrong-old: error",
        )

        # Negative: short new password
        r.assert_error(
            lambda: c.account.change_password(password, "short"),
            "change-short-new: error",
        )

    finally:
        if secret_id:
            try:
                c.secrets.delete(secret_id)
            except Exception:
                try:
                    c2.secrets.delete(secret_id)
                except Exception:
                    pass


if __name__ == "__main__":
    r = Runner("change-password")
    cfg = config_from_env()
    run(r, cfg)
    r.print()
    sys.exit(r.exit_code())
