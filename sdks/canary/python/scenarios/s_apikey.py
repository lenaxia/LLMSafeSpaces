#!/usr/bin/env python3
# Copyright (C) 2026 Michael Kao
# SPDX-License-Identifier: AGPL-3.0-or-later
"""S-APIKEY canary — Python SDK"""

from __future__ import annotations
import sys, os

sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), ".."))
from canary import Runner, Config, config_from_env
from llmsafespaces import LLMSafeSpaces


def run(r: Runner, cfg: Config) -> None:
    c = LLMSafeSpaces(cfg.api_url, api_key=cfg.api_key, timeout=20.0)
    key_id = None
    try:
        ok, key = r.assert_no_error(
            lambda: c.auth.create_api_key("canary-py-key"), "create-key: no error"
        )
        if not ok:
            return
        r.assert_(key.name == "canary-py-key", "create-key: name")
        r.assert_(key.key.startswith("lsp_"), "create-key: starts with lsp_", key.key)
        r.assert_(key.active, "create-key: active=true")
        key_id = key.id

        # List — key present, full key absent
        ok2, keys = r.assert_no_error(
            lambda: c.auth.list_api_keys(), "list-keys: no error"
        )
        if ok2:
            found = next((k for k in keys if k.id == key_id), None)
            r.assert_(found is not None, "list-keys: key present")
            if found:
                r.assert_(not found.key, "list-keys: full key absent in list")

        # New key authenticates
        new_c = LLMSafeSpaces(cfg.api_url, api_key=key.key, timeout=10.0)
        r.assert_no_error(lambda: new_c.auth.me(), "new-key: authenticates")

        # Delete
        r.assert_no_error(lambda: c.auth.delete_api_key(key_id), "delete-key: no error")
        key_id = None

        # Absent after delete
        ok3, keys2 = r.assert_no_error(
            lambda: c.auth.list_api_keys(), "list-after-delete: no error"
        )
        if ok3:
            r.assert_(all(k.id != key.id for k in keys2), "list-after-delete: absent")

        # Deleted key rejected
        r.assert_error(lambda: new_c.auth.me(), "deleted-key: AuthError")

    finally:
        if key_id:
            try:
                c.auth.delete_api_key(key_id)
            except:
                pass

    # N1: delete nonexistent
    r.assert_error(
        lambda: c.auth.delete_api_key("00000000-0000-0000-0000-000000000099"),
        "delete-nonexistent: error",
    )


if __name__ == "__main__":
    r = Runner("apikey")
    cfg = config_from_env()
    run(r, cfg)
    r.print()
    sys.exit(r.exit_code())
