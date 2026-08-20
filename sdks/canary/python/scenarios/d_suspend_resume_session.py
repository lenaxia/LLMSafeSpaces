#!/usr/bin/env python3
# Copyright (C) 2026 Michael Kao
# SPDX-License-Identifier: AGPL-3.0-or-later
"""D-SUSPEND-RESUME-SESSION canary — Python SDK"""

from __future__ import annotations

import sys
import os

sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), ".."))

from canary import (
    Runner,
    Config,
    config_from_env,
    wait_active,
    wait_phase,
    ensure_session_with_retry,
)
from llmsafespaces import LLMSafeSpaces
from llmsafespaces.client import message_text


def run(run: Runner, cfg: Config) -> None:
    if not cfg.llm_api_key:
        run.ok("suspend-resume-session: skipped (no LLM API key)")
        return

    c = LLMSafeSpaces(cfg.api_url, api_key=cfg.api_key, timeout=120.0)
    ws_id = None

    try:
        # P1: Create workspace, wait Active
        ok, ws = run.assert_no_error(
            lambda: c.workspaces.create(
                name="canary-py-sr-sess", runtime="base", storage_size="1Gi"
            ),
            "create-ws: no error",
        )
        if not ok:
            return
        ws_id = ws.id

        phase = wait_active(c, ws_id)
        run.assert_(phase == "Active", "reach-active", f"got {phase!r}")
        if phase != "Active":
            return

        # P2: Ensure session and send BEFORE
        try:
            sess = ensure_session_with_retry(c, ws_id, 5)
        except Exception as e:
            run.fail("ensure-session: no error", str(e))
            return
        run.ok("ensure-session: no error")
        session_id = sess.sessionId

        ok2, msg1 = run.assert_no_error(
            lambda: c.sessions.send_message(
                ws_id, session_id, "Reply with exactly: BEFORE"
            ),
            "send-before: no error",
        )
        if ok2:
            run.assert_(len(message_text(msg1)) > 0, "send-before: non-empty")

        # P3: History before suspend
        ok3, hist1 = run.assert_no_error(
            lambda: c.sessions.get_history(ws_id, session_id),
            "history-before-suspend: no error",
        )
        if ok3:
            run.assert_(
                len(hist1) >= 1, "history-before-suspend: ≥1 entry", str(len(hist1))
            )

        # P4: Suspend
        run.assert_no_error(lambda: c.workspaces.suspend(ws_id), "suspend: no error")
        susp_phase = wait_phase(c, ws_id, "Suspended", 60)
        run.assert_(
            susp_phase == "Suspended", "suspend: phase=Suspended", f"got {susp_phase!r}"
        )

        # P5: Resume
        run.assert_no_error(lambda: c.workspaces.activate(ws_id), "activate: no error")
        resume_phase = wait_active(c, ws_id, 120)
        run.assert_(
            resume_phase == "Active", "activate: phase=Active", f"got {resume_phase!r}"
        )

        # P6: Ensure session post-resume
        try:
            sess2 = ensure_session_with_retry(c, ws_id, 8)
        except Exception as e:
            run.fail("ensure-session-post-resume: no error", str(e))
            return
        run.ok("ensure-session-post-resume: no error")

        # P7: Send AFTER to the new session
        ok4, msg2 = run.assert_no_error(
            lambda: c.sessions.send_message(
                ws_id, sess2.sessionId, "Reply with exactly: AFTER"
            ),
            "send-after: no error",
        )
        if ok4 and msg2 is not None:
            run.assert_(len(message_text(msg2)) > 0, "send-after: non-empty")

        # P8: The BEFORE message must still be retrievable on the ORIGINAL session ID.
        # This is the actual persistence test — if PVC content is wiped by suspend/resume,
        # history on the original session will be empty.
        ok5, hist_original = run.assert_no_error(
            lambda: c.sessions.get_history(ws_id, session_id),
            "history-original-session-after-resume: no error",
        )
        if ok5 and hist_original is not None:
            run.assert_(
                len(hist_original) >= 1,
                "history-original-session-after-resume: BEFORE message persisted",
                f"got {len(hist_original)} entries — history was wiped by suspend/resume",
            )

        # Also verify the new session has its AFTER message
        ok6, hist2 = run.assert_no_error(
            lambda: c.sessions.get_history(ws_id, sess2.sessionId),
            "history-new-session-after-resume: no error",
        )
        if ok6 and hist2 is not None:
            run.assert_(
                len(hist2) >= 1,
                "history-new-session-after-resume: ≥1 entry",
                str(len(hist2)),
            )

    finally:
        if ws_id:
            try:
                c.workspaces.delete(ws_id)
            except Exception:
                pass


if __name__ == "__main__":
    r = Runner("suspend-resume-session")
    cfg = config_from_env()
    run(r, cfg)
    r.print()
    sys.exit(r.exit_code())
