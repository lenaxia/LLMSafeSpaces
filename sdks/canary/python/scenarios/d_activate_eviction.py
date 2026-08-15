#!/usr/bin/env python3
# Copyright (C) 2026 Michael Kao
# SPDX-License-Identifier: AGPL-3.0-or-later
"""D-ACTIVATE-EVICTION canary — Python SDK"""

from __future__ import annotations

import os
import sys

sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), ".."))

from canary import Runner, Config, config_from_env, wait_active
from llmsafespaces import LLMSafeSpaces


def run(r: Runner, cfg: Config) -> None:
    limit = int(os.environ.get("LLMSAFESPACES_MAX_ACTIVE_WORKSPACES_PER_USER", "3"))
    if limit <= 0:
        r.ok("activate-eviction: skipped (unlimited)")
        return

    c = LLMSafeSpaces(cfg.api_url, api_key=cfg.api_key, timeout=120.0)
    created: list[str] = []
    try:
        for i in range(limit):
            ok, ws = r.assert_no_error(
                lambda idx=i: c.workspaces.create(
                    name=f"canary-py-evict-{idx}", runtime="base", storage_size="1Gi"
                ),
                f"create-{i}: no error",
            )
            if ok and ws:
                created.append(ws.id)
                phase = wait_active(c, ws.id)
                r.assert_(phase == "Active", f"active-{i}", f"got {phase!r}")

        if len(created) < 2:
            r.fail("need >= 2 workspaces", f"only created {len(created)}")
            return

        suspended_id = created[-1]
        r.assert_no_error(
            lambda: c.workspaces.suspend(suspended_id),
            "suspend-last: no error",
        )

        import time
        time.sleep(2)

        ok2, ws_new = r.assert_no_error(
            lambda: c.workspaces.create(
                name="canary-py-evict-extra", runtime="base", storage_size="1Gi"
            ),
            "create-extra: no error",
        )
        if ok2 and ws_new:
            created.append(ws_new.id)
            phase = wait_active(c, ws_new.id)
            r.assert_(phase == "Active", "extra-active", f"got {phase!r}")

        ok3, resp = r.assert_no_error(
            lambda: c.workspaces.activate(suspended_id),
            "activate-suspended: no error",
        )
        if ok3 and resp is not None:
            r.assert_("resumed" in resp, "activate: has resumed field")
            r.assert_("suspended" in resp, "activate: has suspended field")

    finally:
        for wid in created:
            try:
                c.workspaces.delete(wid)
            except Exception:
                pass


if __name__ == "__main__":
    r = Runner("activate-eviction")
    cfg = config_from_env()
    run(r, cfg)
    r.print()
    sys.exit(r.exit_code())
