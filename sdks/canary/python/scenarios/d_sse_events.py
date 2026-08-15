#!/usr/bin/env python3
# Copyright (C) 2026 Michael Kao
# SPDX-License-Identifier: AGPL-3.0-or-later
"""D-SSE-EVENTS canary — Python SDK"""

from __future__ import annotations
import json, sys, os, time, threading

sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), ".."))
from canary import Runner, Config, config_from_env, wait_active, wait_phase
from llmsafespaces import LLMSafeSpaces


def run(r: Runner, cfg: Config) -> None:
    c = LLMSafeSpaces(cfg.api_url, api_key=cfg.api_key, timeout=60.0)
    ws_id = None
    try:
        ok, ws = r.assert_no_error(
            lambda: c.workspaces.create(
                name="canary-py-sse", runtime="base", storage_size="1Gi"
            ),
            "create: no error",
        )
        if not ok:
            return
        ws_id = ws.id

        phase = wait_active(c, ws_id)
        r.assert_(phase == "Active", "reach-active", f"got {phase!r}")
        if phase != "Active":
            return

        # Start collecting SSE events in background
        events = []
        stop_flag = threading.Event()

        def collect_events():
            import httpx

            try:
                with httpx.stream(
                    "GET",
                    f"{cfg.api_url}/api/v1/workspaces/{ws_id}/events",
                    headers={
                        "Authorization": f"Bearer {cfg.api_key}",
                        "Accept": "text/event-stream",
                    },
                    timeout=90,
                ) as resp:
                    for line in resp.iter_lines():
                        if stop_flag.is_set():
                            break
                        if line.startswith("data: "):
                            try:
                                events.append(json.loads(line[6:]))
                            except Exception:
                                pass
            except Exception:
                pass

        t = threading.Thread(target=collect_events, daemon=True)
        t.start()
        time.sleep(0.5)

        # P1: SSE connected — check via HTTP directly for headers
        import httpx

        resp_check = httpx.get(
            f"{cfg.api_url}/api/v1/workspaces/{ws_id}/events",
            headers={
                "Authorization": f"Bearer {cfg.api_key}",
                "Accept": "text/event-stream",
            },
            timeout=3.0,
        )
        r.assert_(
            resp_check.status_code == 200,
            "sse-connect: 200",
            str(resp_check.status_code),
        )
        ct = resp_check.headers.get("content-type", "")
        r.assert_("text/event-stream" in ct, "sse-connect: content-type", ct)
        resp_check.close()

        # P2+P3: Suspend triggers workspace.phase event
        r.assert_no_error(lambda: c.workspaces.suspend(ws_id), "suspend: no error")
        deadline = time.time() + 30
        phase_event_received = False
        while time.time() < deadline:
            for e in events:
                if e.get("type") == "workspace.phase" and e.get("phase") in (
                    "Suspending",
                    "Suspended",
                ):
                    phase_event_received = True
                    break
            if phase_event_received:
                break
            time.sleep(1)
        r.assert_(
            phase_event_received, "sse: workspace.phase event received on suspend"
        )

        # P4: Activate triggers another phase event
        r.assert_no_error(lambda: c.workspaces.activate(ws_id), "activate: no error")
        prev_count = len([e for e in events if e.get("type") == "workspace.phase"])
        deadline2 = time.time() + 60
        resume_event_received = False
        while time.time() < deadline2:
            new = [e for e in events if e.get("type") == "workspace.phase"]
            if len(new) > prev_count:
                resume_event_received = True
                break
            time.sleep(1)
        r.assert_(
            resume_event_received, "sse: workspace.phase event received on resume"
        )

        stop_flag.set()

        # N1: SSE on nonexistent workspace
        from canary import raw_do as _raw

        s, _ = _raw(
            "GET",
            f"{cfg.api_url}/api/v1/workspaces/00000000-0000-0000-0000-000000000000/events",
            cfg.api_key,
        )
        r.assert_(s == 404, "sse-nonexistent: 404", str(s))

    finally:
        if ws_id:
            try:
                c.workspaces.delete(ws_id)
            except:
                pass


if __name__ == "__main__":
    r = Runner("sse-events")
    cfg = config_from_env()
    run(r, cfg)
    r.print()
    sys.exit(r.exit_code())
