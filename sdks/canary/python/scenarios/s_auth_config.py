#!/usr/bin/env python3
# Copyright (C) 2026 Michael Kao
# SPDX-License-Identifier: AGPL-3.0-or-later
"""S-AUTH-CONFIG canary — Python SDK"""

from __future__ import annotations
import json, sys, os

sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), ".."))
from canary import Runner, Config, config_from_env, raw_do, has_field


def run(r: Runner, cfg: Config) -> None:
    s, b = raw_do("GET", cfg.api_url + "/api/v1/auth/config", "")
    r.assert_(s == 200, f"auth/config: 200 (got {s})")
    try:
        obj = json.loads(b)
        r.assert_(
            isinstance(obj.get("registrationEnabled"), bool),
            "registrationEnabled: bool",
        )
        r.assert_(isinstance(obj.get("oidcEnabled"), bool), "oidcEnabled: bool")
        r.assert_(
            isinstance(obj.get("instanceName"), str) and obj["instanceName"] != "",
            "instanceName: non-empty string",
            obj.get("instanceName"),
        )
        r.assert_("motd" in obj, "motd: field present")
        r.assert_("error" not in obj, "no error field on success")
    except json.JSONDecodeError:
        r.fail("auth/config: valid JSON", b[:100].decode())


if __name__ == "__main__":
    r = Runner("auth-config")
    cfg = config_from_env()
    run(r, cfg)
    r.print()
    sys.exit(r.exit_code())
