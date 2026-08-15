#!/usr/bin/env python3
# Copyright (C) 2026 Michael Kao
# SPDX-License-Identifier: AGPL-3.0-or-later
"""S-WS-CRUD canary — Python SDK"""

from __future__ import annotations

import sys
import os
import time

sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), ".."))

from canary import Runner, Config, config_from_env, raw_do
from llmsafespaces import LLMSafeSpaces, NotFoundError


def run(run: Runner, cfg: Config) -> None:
    c = LLMSafeSpaces(cfg.api_url, api_key=cfg.api_key, timeout=20.0)
    ws_id = None

    try:
        # P1: Create
        ok, ws = run.assert_no_error(
            lambda: c.workspaces.create(
                name="canary-py-crud", runtime="base", storage_size="1Gi"
            ),
            "create: no error",
        )
        if not ok:
            return
        ws_id = ws.id
        run.assert_(ws.id != "", "create: id non-empty")
        run.assert_(ws.name == "canary-py-crud", "create: name", ws.name)
        run.assert_(ws.runtime == "base", "create: runtime", ws.runtime)
        run.assert_(ws.storageSize == "1Gi", "create: storageSize", ws.storageSize)

        # P2: Get
        ok2, got = run.assert_no_error(lambda: c.workspaces.get(ws_id), "get: no error")
        if ok2:
            run.assert_(got.id == ws_id, "get: id matches", got.id)

        # P3+P4: List
        ok3, result = run.assert_no_error(
            lambda: c.workspaces.list(limit=50), "list: no error"
        )
        if ok3:
            found = any(i.id == ws_id for i in result.items)
            run.assert_(found, "list: workspace present")
            run.assert_(result.pagination is not None, "list: pagination present")

        # P5: Pagination
        ok4, page = run.assert_no_error(
            lambda: c.workspaces.list(limit=1, offset=0), "list-limit1: no error"
        )
        if ok4:
            run.assert_(
                len(page.items) <= 1, "list-limit1: ≤1 item", str(len(page.items))
            )

        # P6: Rename
        ok5, _ = run.assert_no_error(
            lambda: c.workspaces.rename(ws_id, "canary-py-renamed"), "rename: no error"
        )
        if ok5:
            renamed = c.workspaces.get(ws_id)
            run.assert_(
                renamed.name == "canary-py-renamed",
                "rename: name updated",
                renamed.name,
            )

        # P7: Delete
        run.assert_no_error(lambda: c.workspaces.delete(ws_id), "delete: no error")
        ws_id = None  # mark as deleted

        # P8: After delete
        time.sleep(1)
        try:
            deleted = c.workspaces.get(ws.id)
            run.assert_(
                deleted.phase in ("Deleted", "Terminating"),
                "after-delete: terminal phase",
                deleted.phase,
            )
        except NotFoundError:
            run.ok("after-delete: 404")
        except Exception as e:
            run.fail("after-delete: unexpected error", str(e))

    finally:
        if ws_id:
            try:
                c.workspaces.delete(ws_id)
            except Exception:
                pass

    # N1: Get nonexistent
    run.assert_error(
        lambda: c.workspaces.get("00000000-0000-0000-0000-000000000000"),
        "get-nonexistent: NotFoundError",
    )

    # N3: Empty runtime — the API resolves the default-runtime hierarchy
    # (user preference → org policy → workspace.defaultImage → "base"),
    # so it should succeed (not fail). Mirror of the Go twin.
    ok_flag, ws_default = run.assert_no_error(
        lambda: c.workspaces.create(name="canary-default-runtime", runtime="", storage_size="1Gi"),
        "create-empty-runtime: defaults (no error)",
    )
    if ok_flag:
        detail = ws_default.runtime if ws_default is not None else ""
        run.assert_(
            ws_default is not None and ws_default.runtime != "",
            "create-empty-runtime: runtime defaulted",
            detail,
        )
        if ws_default is not None:
            _ = c.workspaces.delete(ws_default.id)

    # N5: Storage too large
    run.assert_error(
        lambda: c.workspaces.create(
            name="oversized", runtime="base", storage_size="9999Gi"
        ),
        "create-oversized-storage: error",
    )

    # N6: Invalid storage format
    run.assert_error(
        lambda: c.workspaces.create(
            name="badsize", runtime="base", storage_size="invalid"
        ),
        "create-invalid-storage: error",
    )


if __name__ == "__main__":
    r = Runner("ws-crud")
    cfg = config_from_env()
    run(r, cfg)
    r.print()
    sys.exit(r.exit_code())
