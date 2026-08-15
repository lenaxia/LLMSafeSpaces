#!/usr/bin/env python3
# Copyright (C) 2026 Michael Kao
# SPDX-License-Identifier: AGPL-3.0-or-later
"""S-RATE-LIMIT canary — Python SDK"""

from __future__ import annotations

import json
import sys
import os

import httpx

sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), ".."))

from canary import Runner, Config, config_from_env, raw_do, has_error_field, has_field


def run(r: Runner, cfg: Config) -> None:
    login_url = f"{cfg.api_url}/api/v1/auth/login"
    body = json.dumps({"email": "no-such-user@test.com", "password": "wrong"}).encode()

    got_429 = False
    for i in range(12):
        status, _ = raw_do("POST", login_url, "", body, timeout=5.0)
        if status == 429:
            got_429 = True
            break

    r.assert_(got_429, "rate-limit: at least one 429", f"attempted 12 rapid logins")

    s1, b1 = raw_do("GET", f"{cfg.api_url}/readyz", "", timeout=5.0)
    r.assert_(s1 == 200, "readyz: 200", str(s1))

    s2, b2 = raw_do("GET", f"{cfg.api_url}/livez", "", timeout=5.0)
    r.assert_(s2 == 200, "livez: 200", str(s2))


if __name__ == "__main__":
    r = Runner("rate-limit")
    cfg = config_from_env()
    run(r, cfg)
    r.print()
    sys.exit(r.exit_code())
