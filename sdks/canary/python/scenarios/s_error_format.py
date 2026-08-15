#!/usr/bin/env python3
# Copyright (C) 2026 Michael Kao
# SPDX-License-Identifier: AGPL-3.0-or-later
"""S-ERROR-FORMAT canary — Python SDK"""

from __future__ import annotations

import json
import sys
import os

sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), ".."))

from canary import (
    Runner,
    Config,
    config_from_env,
    raw_do,
    has_error_field,
    has_field,
    contains_leaked_internals,
)
from llmsafespaces import LLMSafeSpaces


def run(run: Runner, cfg: Config) -> None:
    base = f"{cfg.api_url}/api/v1"

    # P1: 401 no auth
    s1, b1 = raw_do("GET", f"{base}/auth/me", "")
    run.assert_(s1 == 401, "401-no-auth: status", f"got {s1}")
    run.assert_(has_error_field(b1), "401-no-auth: error field")
    _assert_error_is_string(run, b1, "401-no-auth: error is string")

    # P2: 404 nonexistent workspace
    s2, b2 = raw_do(
        "GET", f"{base}/workspaces/00000000-0000-0000-0000-000000000000", cfg.api_key
    )
    run.assert_(s2 == 404, "404-nonexistent: status", f"got {s2}")
    run.assert_(has_error_field(b2), "404-nonexistent: error field")

    # P3: 400 empty register body
    s3, b3 = raw_do("POST", f"{base}/auth/register", "", b"{}")
    run.assert_(s3 == 400, "400-empty-register: status", f"got {s3}")
    run.assert_(has_error_field(b3), "400-empty-register: error field")

    # P4: 400 PUT workspace missing name
    c = LLMSafeSpaces(cfg.api_url, api_key=cfg.api_key, timeout=15.0)
    ws_id = None
    try:
        ws = c.workspaces.create(
            name="canary-py-errfmt", runtime="base", storage_size="1Gi"
        )
        ws_id = ws.id
        s4, b4 = raw_do("PUT", f"{base}/workspaces/{ws_id}", cfg.api_key, b"{}")
        run.assert_(s4 == 400, "400-rename-empty: status", f"got {s4}")
        run.assert_(has_error_field(b4), "400-rename-empty: error field")
        _assert_error_is_string(run, b4, "400-rename-empty: error is string")

        # P7: proxy 503 workspace-not-ready shape
        s7, b7 = raw_do(
            "POST",
            f"{base}/workspaces/{ws_id}/sessions/canary-sess-id/message",
            cfg.api_key,
            b'{"content":"ping","parts":[{"type":"text","text":"ping"}]}',
        )
        if s7 == 503:
            run.assert_(has_field(b7, "phase"), "503-not-ready: phase field")
            run.assert_(has_field(b7, "retryAfter"), "503-not-ready: retryAfter field")
            run.assert_(has_error_field(b7), "503-not-ready: error field")
        else:
            run.assert_(s7 >= 400, "proxy-error: 4xx/5xx", f"got {s7}")

    finally:
        if ws_id:
            try:
                c.workspaces.delete(ws_id)
            except Exception:
                pass

    # P8: path traversal blocked
    s8, b8 = raw_do(
        "GET",
        f"{base}/workspaces/test-ws/sessions/..%2F..%2Fetc%2Fpasswd/message",
        cfg.api_key,
    )
    run.assert_(s8 in (400, 404), "path-traversal: 400 or 404", f"got {s8}")

    # P5+P6: No leaked internals
    for body in [b1, b2, b3]:
        run.assert_(not contains_leaked_internals(body), "no-leaked-internals", "")

    # P9: Success has no error field
    s9, b9 = raw_do("GET", f"{cfg.api_url}/livez", "")
    run.assert_(s9 == 200, "success-no-error: livez 200")
    run.assert_(not has_field(b9, "error"), "success-no-error: no error field")


def _assert_error_is_string(run: Runner, body: bytes, label: str) -> None:
    try:
        obj = json.loads(body)
        v = obj.get("error")
        run.assert_(isinstance(v, str), label, f"error field type: {type(v)!r}")
    except json.JSONDecodeError:
        run.fail(label, "not valid JSON")


if __name__ == "__main__":
    r = Runner("error-format")
    cfg = config_from_env()
    run(r, cfg)
    r.print()
    sys.exit(r.exit_code())
