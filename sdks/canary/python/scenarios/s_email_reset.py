#!/usr/bin/env python3
# Copyright (C) 2026 Michael Kao
# SPDX-License-Identifier: AGPL-3.0-or-later
"""S-EMAIL-RESET canary — tests email endpoints through the real HTTP boundary."""

from __future__ import annotations

import sys
import os
import time
import json

sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), ".."))

from canary import Runner, Config, config_from_env, raw_do, contains_leaked_internals


def run(run: Runner, cfg: Config) -> None:
    base = cfg.api_url.rstrip("/") + "/api/v1"
    unique = int(time.time())
    email = f"canary-email-{unique}@llmsafespaces.test"
    password = "canary-email-pwd-123456"

    # P1: Register
    reg_body = json.dumps({"username": f"canaryemail{unique}", "email": email, "password": password}).encode()
    status, _ = raw_do("POST", base + "/auth/register", "", reg_body)
    run.assert_(status in (201, 409), f"register: 201 or 409 (got {status})")

    # P2: Login. Retry on 429: sibling scenarios' JWT logins share the
    # per-IP login bucket (10/min); this scenario's contract is the
    # 200-vs-403 email-verification semantics, not rate limiting.
    login_body = json.dumps({"email": email, "password": password}).encode()
    login_status = 0
    for attempt in range(7):
        login_status, _ = raw_do("POST", base + "/auth/login", "", login_body)
        if login_status != 429:
            break
        time.sleep(10)
    if login_status == 200:
        run.ok("login: 200 (noop mode — auto-verified)")
    elif login_status == 403:
        run.ok("login: 403 (SES mode — unverified)")
    else:
        run.fail("login: unexpected status", f"got {login_status}")

    # P3: Password-reset request → 202
    reset_body = json.dumps({"email": email}).encode()
    status, _ = raw_do("POST", base + "/auth/password-reset/request", "", reset_body)
    run.assert_(status == 202, f"password-reset-request: 202 (got {status})")

    # P4: Password-reset request unknown → 202
    status, _ = raw_do("POST", base + "/auth/password-reset/request", "",
                       json.dumps({"email": "nonexistent-canary@llmsafespaces.test"}).encode())
    run.assert_(status == 202, f"password-reset-request-unknown: 202 (got {status})")

    # P5: Password-reset confirm bogus → 404
    status, _ = raw_do("POST", base + "/auth/password-reset/confirm", "",
                       json.dumps({"token": "canary-bogus", "newPassword": "canary-new-pwd-123456"}).encode())
    run.assert_(status == 404, f"password-reset-confirm-bogus: 404 (got {status})")

    # P6: Verify-email bogus → 404
    status, _ = raw_do("POST", base + "/auth/verify-email", "",
                       json.dumps({"token": "canary-bogus"}).encode())
    run.assert_(status == 404, f"verify-email-bogus: 404 (got {status})")

    # P7: Verify-email resend → 202
    status, _ = raw_do("POST", base + "/auth/verify-email/resend", "",
                       json.dumps({"email": email}).encode())
    run.assert_(status == 202, f"verify-email-resend: 202 (got {status})")

    # P8: Resend unknown → 202
    status, _ = raw_do("POST", base + "/auth/verify-email/resend", "",
                       json.dumps({"email": "ghost-canary@nonexistent.invalid"}).encode())
    run.assert_(status == 202, f"verify-email-resend-unknown: 202 (got {status})")

    # P9: No leaked internals in error responses
    _, leak_resp = raw_do("POST", base + "/auth/password-reset/confirm", "",
                          json.dumps({"token": "x", "newPassword": "canary-valid-pwd"}).encode())
    run.assert_(not contains_leaked_internals(leak_resp), "password-reset-confirm: no leaked internals")


if __name__ == "__main__":
    run_inst = Runner("email-reset", "python-sdk")
    cfg = config_from_env()
    run(run_inst, cfg)
    res = run_inst.print()
    if res.failed > 0:
        sys.exit(1)
