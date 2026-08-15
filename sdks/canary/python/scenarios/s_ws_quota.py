#!/usr/bin/env python3
# Copyright (C) 2026 Michael Kao
# SPDX-License-Identifier: AGPL-3.0-or-later
"""S-WS-QUOTA canary — Python SDK"""

from __future__ import annotations

import os
import sys

sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), ".."))

from canary import Runner, Config, config_from_env
from llmsafespaces import LLMSafeSpaces, RateLimitError


def run(r: Runner, cfg: Config) -> None:
    # Match the Go/TypeScript canary behavior: skip if the quota env var
    # is not explicitly set. The server enforces per-user quotas only when
    # LLMSAFESPACES_MAX_WORKSPACES_PER_USER is configured (router.go); CI
    # never sets it, so creating 11 workspaces against an unlimited
    # cluster and expecting a 429 is a guaranteed false failure.
    env_limit = os.environ.get("LLMSAFESPACES_MAX_WORKSPACES_PER_USER", "")
    if not env_limit:
        r.ok("ws-quota: skipped (LLMSAFESPACES_MAX_WORKSPACES_PER_USER not set)")
        return
    limit = int(env_limit)
    if limit <= 0:
        r.ok("ws-quota: skipped (unlimited)")
        return

    c = LLMSafeSpaces(cfg.api_url, api_key=cfg.api_key, timeout=30.0)
    created: list[str] = []
    try:
        existing = c.workspaces.list(limit=100)
        existing_count = len(existing.items)
        slots = max(limit - existing_count, 0)
        if slots == 0:
            r.ok("ws-quota: skipped (already at limit)")
            return

        for i in range(slots):
            ok, ws = r.assert_no_error(
                lambda idx=i: c.workspaces.create(
                    name=f"canary-py-quota-{idx}", runtime="base", storage_size="1Gi"
                ),
                f"create-{i}: no error",
            )
            if ok and ws:
                created.append(ws.id)

        hit_429 = False
        try:
            ws_extra = c.workspaces.create(
                name="canary-py-quota-over", runtime="base", storage_size="1Gi"
            )
            created.append(ws_extra.id)
        except RateLimitError:
            hit_429 = True

        r.assert_(hit_429, "over-limit: 429 returned", "expected RateLimitError")

    finally:
        for wid in created:
            try:
                c.workspaces.delete(wid)
            except Exception:
                pass


if __name__ == "__main__":
    r = Runner("ws-quota")
    cfg = config_from_env()
    run(r, cfg)
    r.print()
    sys.exit(r.exit_code())
