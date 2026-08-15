#!/usr/bin/env python3
# Copyright (C) 2026 Michael Kao
# SPDX-License-Identifier: AGPL-3.0-or-later
"""D-ACCOUNT-RECOVER canary — Python SDK"""

from __future__ import annotations

import sys
import os

sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), ".."))

from canary import Runner, Config, config_from_env, raw_do
from llmsafespaces import LLMSafeSpaces


def run(r: Runner, cfg: Config) -> None:
    email = os.environ.get("LLMSAFESPACES_ROTATE_EMAIL", "canary-rotate@llmsafespaces.test")
    password = os.environ.get("LLMSAFESPACES_ROTATE_PASSWORD", "canary-rotate-password!")
    new_password = "canary-recover-pw-99!"

    c = LLMSafeSpaces(cfg.api_url, email=email, password=password, timeout=30.0)
    secret_id = None
    try:
        ok_me, me = r.assert_no_error(lambda: c.auth.me(), "login: no error")
        if not ok_me or me is None:
            return
        user_id = me.get("id", "")

        ok, secret = r.assert_no_error(
            lambda: c.secrets.create(
                name="canary-py-recover", type="env-secret", value="recover-test-value"
            ),
            "create-secret: no error",
        )
        if not ok:
            return
        secret_id = secret.id

        ok2, rotate_resp = r.assert_no_error(
            lambda: c.account.rotate_key(password), "rotate-key: no error"
        )
        if not ok2 or rotate_resp is None:
            return
        recovery_key = rotate_resp.get("recoveryKey", "")
        r.assert_(len(recovery_key) > 0, "rotate-key: recoveryKey non-empty")

        ok3, recover_resp = r.assert_no_error(
            lambda: c.account.recover(user_id, recovery_key, new_password),
            "recover: no error",
        )
        if ok3 and recover_resp is not None:
            r.assert_(
                "recoveryKey" in recover_resp,
                "recover: has new recoveryKey",
                str(list(recover_resp.keys())),
            )

        c2 = LLMSafeSpaces(cfg.api_url, email=email, password=new_password, timeout=30.0)
        ok4, me2 = r.assert_no_error(lambda: c2.auth.me(), "login-after-recover: no error")
        r.assert_(ok4, "login-after-recover: succeeds")

        ok5, val = r.assert_no_error(
            lambda: c2.secrets.reveal(secret_id, new_password),
            "reveal-after-recover: no error",
        )
        if ok5:
            r.assert_(val == "recover-test-value", "reveal-after-recover: value matches", repr(val))

        # Negative: invalid recovery key
        r.assert_error(
            lambda: c2.account.recover(user_id, "invalid-key-xyz", "another-new-pw!"),
            "recover-invalid-key: error",
        )

        # Negative: missing fields
        import json
        s, _ = raw_do(
            "POST",
            f"{cfg.api_url}/api/v1/account/recover",
            "",
            json.dumps({"userId": user_id}).encode(),
        )
        r.assert_(s in (400, 422), "recover-missing-fields: 400 or 422", str(s))

        r.assert_no_error(
            lambda: c2.account.change_password(new_password, password),
            "reset-password: no error",
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
    r = Runner("account-recover")
    cfg = config_from_env()
    run(r, cfg)
    r.print()
    sys.exit(r.exit_code())
