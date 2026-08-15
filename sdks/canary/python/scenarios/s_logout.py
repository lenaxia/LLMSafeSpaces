#!/usr/bin/env python3
# Copyright (C) 2026 Michael Kao
# SPDX-License-Identifier: AGPL-3.0-or-later
"""S-LOGOUT canary — Python SDK"""

from __future__ import annotations
import json, sys, os

sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), ".."))
from canary import Runner, Config, config_from_env, raw_do
from llmsafespaces import LLMSafeSpaces


def run(r: Runner, cfg: Config) -> None:
    if not cfg.email or not cfg.password:
        r.ok("logout: skipped (no email/password)")
        return

    # P1: Login → JWT token
    s, b = raw_do(
        "POST",
        cfg.api_url + "/api/v1/auth/login",
        "",
        json.dumps({"email": cfg.email, "password": cfg.password}).encode(),
    )
    if not r.assert_(s == 200, f"login: 200 (got {s})", b[:200].decode()):
        return
    token = json.loads(b).get("token", "")
    r.assert_(token != "", "login: token non-empty")

    # P2: JWT works pre-logout
    c = LLMSafeSpaces(cfg.api_url, api_key=token, timeout=10.0)
    ok, _ = r.assert_no_error(lambda: c.auth.me(), "pre-logout: auth.me succeeds")

    # P3: Logout
    s2, _ = raw_do("POST", cfg.api_url + "/api/v1/auth/logout", token, b"")
    r.assert_(s2 == 204, f"logout: 204 (got {s2})")

    # P4: Same JWT rejected
    r.assert_error(lambda: c.auth.me(), "post-logout: JWT rejected")

    # P5: Idempotent second logout
    s3, _ = raw_do("POST", cfg.api_url + "/api/v1/auth/logout", token, b"")
    r.assert_(s3 == 204, "logout-idempotent: 204")

    # N1+N2: API key still valid
    c_key = LLMSafeSpaces(cfg.api_url, api_key=cfg.api_key, timeout=10.0)
    r.assert_no_error(lambda: c_key.auth.me(), "api-key: still valid after JWT logout")


if __name__ == "__main__":
    r = Runner("logout")
    cfg = config_from_env()
    run(r, cfg)
    r.print()
    sys.exit(r.exit_code())
