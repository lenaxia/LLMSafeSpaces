#!/usr/bin/env python3
# Copyright (C) 2026 Michael Kao
# SPDX-License-Identifier: AGPL-3.0-or-later
"""D-SESSION-LIMIT canary — Python SDK"""

from __future__ import annotations

import sys
import os
import json
import threading

import httpx

sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), ".."))

from canary import (
    Runner,
    Config,
    config_from_env,
    wait_active,
    ensure_session_with_retry,
    raw_do,
)
from llmsafespaces import LLMSafeSpaces, RateLimitError


def run(r: Runner, cfg: Config) -> None:
    if not cfg.llm_api_key:
        r.ok("skipped: no LLM key")
        return

    c = LLMSafeSpaces(cfg.api_url, api_key=cfg.api_key, timeout=60.0)
    ws_id = None
    try:
        ok, ws = r.assert_no_error(
            lambda: c.workspaces.create(
                name="canary-py-sess-limit", runtime="base", storage_size="1Gi"
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

        active_info = c.sessions.get_active(ws_id)
        max_active = active_info.get("maxActive", 0)
        r.assert_(max_active > 0, "max-active-positive", f"got {max_active}")

        session_ids = []
        for i in range(max_active + 2):
            try:
                sess = c.sessions.ensure(ws_id)
                if hasattr(sess, "session_id") and sess.session_id:
                    session_ids.append(sess.session_id)
                elif isinstance(sess, dict) and sess.get("sessionId"):
                    session_ids.append(sess["sessionId"])
            except Exception:
                break
        r.assert_(
            len(session_ids) >= max_active + 1,
            "enough-sessions",
            f"need {max_active + 1} got {len(session_ids)}",
        )

        errors = []
        barrier = threading.Barrier(min(max_active, len(session_ids)), timeout=30)

        def send_async(idx):
            try:
                barrier.wait()
                c.sessions.send_prompt_async(ws_id, session_ids[idx], f"Count slowly from {idx}")
            except Exception as e:
                errors.append(e)

        threads = []
        for i in range(min(max_active, len(session_ids))):
            t = threading.Thread(target=send_async, args=(i,))
            t.start()
            threads.append(t)
        for t in threads:
            t.join(timeout=60)
        r.assert_(len(errors) == 0, "p1-fill-slots", f"{len(errors)} errors")

        import time
        time.sleep(2)

        if len(session_ids) > max_active:
            extra_idx = max_active
            status, body, raw_err = raw_do(
                "POST",
                f"{cfg.api_url}/api/v1/workspaces/{ws_id}/sessions/{session_ids[extra_idx]}/prompt",
                cfg.api_key,
                json.dumps({"message": "hello"}).encode(),
            )
            if raw_err:
                r.fail("p2-over-limit", str(raw_err))
            else:
                r.assert_(status == 429, "p2-429-active-limit", f"got {status}")
                if status == 429:
                    body_obj = json.loads(body) if body else {}
                    r.assert_("error" in body_obj, "p2-has-error-field", "")
                    r.assert_("retryAfter" in body_obj or "maxActiveSessions" in body_obj,
                              "p2-has-limit-fields", f"keys={list(body_obj.keys())}")

        for sid in session_ids[:max_active]:
            try:
                c.sessions.abort(ws_id, sid)
            except Exception:
                pass

        try:
            sess2 = c.sessions.ensure(ws_id)
            sid2 = sess2.session_id if hasattr(sess2, "session_id") else sess2["sessionId"]
        except RateLimitError:
            r.ok("p3-post-abort-still-limited")
            sid2 = None
        except Exception as e:
            r.fail("p3-post-abort", str(e))
            sid2 = None

        if sid2:
            r.ok("p3-post-abort-new-msg-succeeds")

        streams = []
        for i in range(11):
            try:
                resp = httpx.stream(
                    "GET",
                    f"{cfg.api_url}/api/v1/workspaces/{ws_id}/events",
                    headers={
                        "Authorization": f"Bearer {cfg.api_key}",
                        "Accept": "text/event-stream",
                    },
                    timeout=10,
                )
                resp.__enter__()
                streams.append(resp)
            except Exception:
                pass

        sse_429 = False
        try:
            check = httpx.get(
                f"{cfg.api_url}/api/v1/workspaces/{ws_id}/events",
                headers={
                    "Authorization": f"Bearer {cfg.api_key}",
                    "Accept": "text/event-stream",
                },
                timeout=3.0,
            )
            if check.status_code == 429:
                sse_429 = True
            check.close()
        except Exception:
            pass

        if len(streams) >= 11:
            r.assert_(sse_429, "sse-limit: 11th stream gets 429", "no 429 observed")

        for s in streams:
            try:
                s.__exit__(None, None, None)
            except Exception:
                pass

    finally:
        if ws_id:
            try:
                c.workspaces.delete(ws_id)
            except Exception:
                pass


if __name__ == "__main__":
    r = Runner("session-limit")
    cfg = config_from_env()
    run(r, cfg)
    r.print()
    sys.exit(r.exit_code())
