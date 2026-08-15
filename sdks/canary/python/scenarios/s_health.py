#!/usr/bin/env python3
# Copyright (C) 2026 Michael Kao
# SPDX-License-Identifier: AGPL-3.0-or-later
"""S-HEALTH canary — Python SDK"""

from __future__ import annotations
import sys, os

sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), ".."))
from canary import Runner, Config, config_from_env, raw_do, has_field


def run(r: Runner, cfg: Config) -> None:
    for name, path in [
        ("livez", "/livez"),
        ("health-alias", "/health"),
        ("readyz", "/readyz"),
    ]:
        s, b = raw_do("GET", cfg.api_url + path, "")
        if r.assert_(s == 200, f"{name}: 200", f"got {s}"):
            r.assert_(has_field(b, "status"), f"{name}: has status field")
    # Not rate-limited under 10 rapid requests
    ok = all(raw_do("GET", cfg.api_url + "/livez", "")[0] != 429 for _ in range(10))
    r.assert_(ok, "livez: not rate-limited under 10 requests")


if __name__ == "__main__":
    r = Runner("health")
    cfg = config_from_env()
    run(r, cfg)
    r.print()
    sys.exit(r.exit_code())
