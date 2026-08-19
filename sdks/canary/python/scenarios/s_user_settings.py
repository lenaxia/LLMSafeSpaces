#!/usr/bin/env python3
# Copyright (C) 2026 Michael Kao
# SPDX-License-Identifier: AGPL-3.0-or-later
"""S-USER-SETTINGS canary — Python SDK"""

from __future__ import annotations
import sys, os

sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), ".."))
from canary import Runner, Config, config_from_env, raw_do

EXPECTED_SCHEMA_VERSION = 13


def run(r: Runner, cfg: Config) -> None:
    import httpx

    hdrs = {
        "Authorization": f"Bearer {cfg.api_key}",
        "Content-Type": "application/json",
    }
    base = cfg.api_url + "/api/v1"

    # P1: GET settings
    resp = httpx.get(f"{base}/users/me/settings", headers=hdrs, timeout=15)
    r.assert_(resp.status_code == 200, f"get-settings: 200 (got {resp.status_code})")
    if resp.status_code == 200:
        obj = resp.json()
        r.assert_("settings" in obj, "get-settings: settings object present")
        sv = obj.get("schemaVersion", 0)
        r.assert_(sv > 0, "get-settings: schemaVersion > 0", str(sv))

    # P2+P3: GET schema
    resp2 = httpx.get(f"{base}/users/me/settings/schema", headers=hdrs, timeout=15)
    r.assert_(resp2.status_code == 200, f"get-schema: 200 (got {resp2.status_code})")
    if resp2.status_code == 200:
        obj2 = resp2.json()
        sv2 = obj2.get("schemaVersion", 0)
        r.assert_(
            sv2 == EXPECTED_SCHEMA_VERSION,
            f"schema-version: equals {EXPECTED_SCHEMA_VERSION}",
            f"got {sv2} — SCHEMA DRIFT DETECTED",
        )
        r.assert_("settings" in obj2, "get-schema: settings array present")

    # P5-P7: SET and verify round-trip
    resp3 = httpx.put(
        f"{base}/users/me/settings/theme",
        headers=hdrs,
        json={"value": "dark"},
        timeout=15,
    )
    r.assert_(resp3.status_code == 200, f"set-theme: 200 (got {resp3.status_code})")
    if resp3.status_code == 200:
        obj3 = resp3.json()
        r.assert_(obj3.get("key") == "theme", "set-theme: key field")
        r.assert_(obj3.get("value") == "dark", "set-theme: value field")

    resp4 = httpx.get(f"{base}/users/me/settings", headers=hdrs, timeout=15)
    if resp4.status_code == 200:
        r.assert_(
            resp4.json().get("settings", {}).get("theme") == "dark",
            "get-after-set: theme=dark",
        )
    httpx.put(
        f"{base}/users/me/settings/theme",
        headers=hdrs,
        json={"value": "system"},
        timeout=15,
    )

    # N1: no auth → 401
    s, _ = raw_do("GET", f"{base}/users/me/settings", "")
    r.assert_(s == 401, "no-auth: 401", str(s))

    # N2: missing value body → 400
    resp5 = httpx.put(
        f"{base}/users/me/settings/theme", headers=hdrs, json={}, timeout=15
    )
    r.assert_(resp5.status_code == 400, "missing-value: 400", str(resp5.status_code))

    # N3: unknown key → 400
    resp6 = httpx.put(
        f"{base}/users/me/settings/nonexistent.key.xyz",
        headers=hdrs,
        json={"value": "test"},
        timeout=15,
    )
    r.assert_(resp6.status_code == 400, "unknown-key: 400", str(resp6.status_code))


if __name__ == "__main__":
    r = Runner("user-settings")
    cfg = config_from_env()
    run(r, cfg)
    r.print()
    sys.exit(r.exit_code())
