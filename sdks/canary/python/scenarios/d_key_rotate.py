#!/usr/bin/env python3
# Copyright (C) 2026 Michael Kao
# SPDX-License-Identifier: AGPL-3.0-or-later
"""D-KEY-ROTATE canary — Python SDK"""

from __future__ import annotations

import sys
import os

sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), ".."))

from canary import Runner, Config, config_from_env
from llmsafespaces import LLMSafeSpaces, AuthError


def run(r: Runner, cfg: Config) -> None:
    email = os.environ.get("LLMSAFESPACES_ROTATE_EMAIL", "canary-rotate@llmsafespaces.test")
    password = os.environ.get("LLMSAFESPACES_ROTATE_PASSWORD", "canary-rotate-password!")

    c = LLMSafeSpaces(cfg.api_url, email=email, password=password, timeout=30.0)
    secret_id = None
    try:
        ok_me, me = r.assert_no_error(lambda: c.auth.me(), "login: no error")
        if not ok_me or me is None:
            return
        user_id = me.get("id", "")

        ok, secret = r.assert_no_error(
            lambda: c.secrets.create(
                name="canary-py-rotate", type="env-secret", value="rotate-test-value"
            ),
            "create-secret: no error",
        )
        if not ok:
            return
        secret_id = secret.id

        ok2, rotate_resp = r.assert_no_error(
            lambda: c.account.rotate_key(password), "rotate-key: no error"
        )
        if ok2 and rotate_resp is not None:
            r.assert_(
                "recoveryKey" in rotate_resp,
                "rotate-key: has recoveryKey",
                str(list(rotate_resp.keys())),
            )

        ok3, val = r.assert_no_error(
            lambda: c.secrets.reveal(secret_id, password),
            "reveal-after-rotate: no error",
        )
        if ok3:
            r.assert_(
                val == "rotate-test-value",
                "reveal-after-rotate: value matches",
                repr(val),
            )

        r.assert_error(
            lambda: c.account.rotate_key("wrong-password-xyz"),
            "rotate-wrong-password: error",
        )

        r.assert_error(
            lambda: c.account.rotate_key(""),
            "rotate-empty-password: error",
        )

    finally:
        if secret_id:
            try:
                c.secrets.delete(secret_id)
            except Exception:
                pass


if __name__ == "__main__":
    r = Runner("key-rotate")
    cfg = config_from_env()
    run(r, cfg)
    r.print()
    sys.exit(r.exit_code())
