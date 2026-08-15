#!/usr/bin/env python3
# Copyright (C) 2026 Michael Kao
# SPDX-License-Identifier: AGPL-3.0-or-later
"""S-AUTH canary — Python SDK"""

from __future__ import annotations

import sys
import os

sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), ".."))

from canary import Runner, Config, config_from_env
from llmsafespaces import LLMSafeSpaces, AuthError


def run(run: Runner, cfg: Config) -> None:
    c = LLMSafeSpaces(cfg.api_url, api_key=cfg.api_key, timeout=20.0)

    # P1: valid API key
    ok, me = run.assert_no_error(lambda: c.auth.me(), "valid-key: auth.me no error")
    if ok:
        run.assert_(me.get("id") is not None, "valid-key: user.id present")
        run.assert_(me.get("email") is not None, "valid-key: user.email present")
        run.assert_(me.get("role") is not None, "valid-key: user.role present")
        run.assert_(me.get("active") is True, "valid-key: user.active=true")

    # P2+P3: JWT login
    if cfg.email and cfg.password:
        jwt_c = LLMSafeSpaces(
            cfg.api_url, email=cfg.email, password=cfg.password, timeout=20.0
        )
        ok2, me2 = run.assert_no_error(
            lambda: jwt_c.auth.me(), "jwt-login: auth.me no error"
        )
        if ok2:
            run.assert_(me2.get("active") is True, "jwt-login: user.active=true")

    # N1: invalid key
    bad = LLMSafeSpaces(
        cfg.api_url, api_key="lsp_invalid_canary_key_000000000000", timeout=10.0
    )
    run.assert_error(lambda: bad.auth.me(), "invalid-key: raises AuthError")

    # N2: empty key
    noauth = LLMSafeSpaces(cfg.api_url, api_key="", timeout=10.0)
    run.assert_error(lambda: noauth.auth.me(), "empty-key: raises error")

    # N4+N5: wrong password / nonexistent email
    for name, email, pw in [
        ("wrong-password", cfg.email or "canary@example.com", "definitely-wrong-xyz"),
        ("nonexistent-email", "ghost-99@nonexistent.invalid", "wrongpass123"),
    ]:
        bad_login = LLMSafeSpaces(cfg.api_url, email=email, password=pw, timeout=10.0)
        run.assert_error(lambda c=bad_login: c.auth.me(), f"{name}: raises error")


if __name__ == "__main__":
    r = Runner("auth")
    cfg = config_from_env()
    run(r, cfg)
    res = r.print()
    sys.exit(r.exit_code())
