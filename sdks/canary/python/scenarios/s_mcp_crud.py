#!/usr/bin/env python3
# Copyright (C) 2026 Michael Kao
# SPDX-License-Identifier: AGPL-3.0-or-later
"""S-MCP-CRUD canary (#1046) — user-scope MCP server CRUD round-trip."""

from __future__ import annotations
import sys, os

sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), ".."))
from canary import jwt_login, Runner, Config, config_from_env
from llmsafespaces import LLMSafeSpaces


def run(r: Runner, cfg: Config) -> None:
    c = LLMSafeSpaces(cfg.api_url, api_key=jwt_login(cfg), timeout=20.0)
    srv_id = None
    try:
        ok, srv = r.assert_no_error(
            lambda: c.mcp_servers.create(
                {"name": "canary-py-mcp", "transport": "http", "url": "https://mcp.example.com/mcp"}
            ),
            "create-mcp: no error",
        )
        if not ok or not srv:
            return
        r.assert_(bool(srv.get("id")), "create-mcp: id non-empty", str(srv.get("id")))
        srv_id = srv["id"]

        ok, got = r.assert_no_error(lambda: c.mcp_servers.get(srv_id), "get-mcp: no error")
        if ok and got:
            r.assert_(got.get("name") == "canary-py-mcp", "get-mcp: name", str(got.get("name")))

        ok, listed = r.assert_no_error(lambda: c.mcp_servers.list(), "list-mcp: no error")
        if ok and listed is not None:
            r.assert_(any(s.get("id") == srv_id for s in listed), "list-mcp: present", "")

        ok, updated = r.assert_no_error(
            lambda: c.mcp_servers.update(srv_id, {"name": "canary-py-mcp-renamed"}),
            "update-mcp: no error",
        )
        if ok and updated:
            r.assert_(updated.get("name") == "canary-py-mcp-renamed", "update-mcp: renamed", str(updated.get("name")))

        ok, _ = r.assert_no_error(
            lambda: c.mcp_servers.create_auto_apply(srv_id, "user"), "auto-apply-create: no error"
        )
        if ok:
            ok, rules = r.assert_no_error(lambda: c.mcp_servers.list_auto_apply(srv_id), "auto-apply-list: no error")
            if ok and rules is not None:
                r.assert_(any(x.get("targetType") == "user" for x in rules), "auto-apply-list: user rule", "")

        ok, _ = r.assert_no_error(lambda: c.mcp_servers.delete(srv_id), "delete-mcp: no error")
        if ok:
            srv_id = None
            r.assert_error(lambda: c.mcp_servers.get(srv["id"]), "get-after-delete: error")
    finally:
        if srv_id:
            try:
                c.mcp_servers.delete(srv_id)
            except Exception:
                pass

    r.assert_error(
        lambda: c.mcp_servers.get("00000000-0000-0000-0000-000000000098"),
        "get-nonexistent-mcp: error",
    )
    r.assert_error(
        lambda: c.mcp_servers.create({"transport": "http"}),
        "create-malformed-mcp: error",
    )


def main() -> None:
    r = Runner("mcp-crud")
    run(r, config_from_env())
    r.print()
    sys.exit(r.exit_code())


if __name__ == "__main__":
    main()
