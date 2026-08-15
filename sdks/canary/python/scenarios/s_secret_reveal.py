#!/usr/bin/env python3
# Copyright (C) 2026 Michael Kao
# SPDX-License-Identifier: AGPL-3.0-or-later
"""S-SECRET-REVEAL canary — Python SDK"""

from __future__ import annotations
import sys, os

sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), ".."))
from canary import Runner, Config, config_from_env, raw_do, has_field
from llmsafespaces import LLMSafeSpaces

SECRET_VALUE = "canary-py-reveal-test-val-xyz"


def run(r: Runner, cfg: Config) -> None:
    if not cfg.password:
        r.ok("reveal: skipped (no LLMSAFESPACES_PASSWORD)")
        return
    c = LLMSafeSpaces(cfg.api_url, api_key=cfg.api_key, timeout=20.0)
    sid = None
    try:
        ok, s = r.assert_no_error(
            lambda: c.secrets.create(
                name="canary-py-reveal", type="env-secret", value=SECRET_VALUE
            ),
            "create: no error",
        )
        if not ok:
            return
        sid = s.id

        # P2: reveal with correct password
        ok2, val = r.assert_no_error(
            lambda: c.secrets.reveal(sid, cfg.password), "reveal-correct: no error"
        )
        if ok2:
            r.assert_(val == SECRET_VALUE, "reveal: value matches", repr(val))

        # P3: value absent from GET
        _, b = raw_do("GET", f"{cfg.api_url}/api/v1/secrets/{sid}", cfg.api_key)
        r.assert_(not has_field(b, "value"), "get: no value field")

        # N1: missing password body
        s1, _ = raw_do(
            "POST", f"{cfg.api_url}/api/v1/secrets/{sid}/reveal", cfg.api_key, b"{}"
        )
        r.assert_(s1 == 400, "reveal-no-password: 400", str(s1))

        # N2: wrong password
        import json as _json

        s2, b2 = raw_do(
            "POST",
            f"{cfg.api_url}/api/v1/secrets/{sid}/reveal",
            cfg.api_key,
            _json.dumps({"password": "definitely-wrong-xyz"}).encode(),
        )
        r.assert_(s2 == 403, "reveal-wrong-password: 403", str(s2))

    finally:
        if sid:
            try:
                c.secrets.delete(sid)
            except:
                pass


if __name__ == "__main__":
    r = Runner("secret-reveal")
    cfg = config_from_env()
    run(r, cfg)
    r.print()
    sys.exit(r.exit_code())
