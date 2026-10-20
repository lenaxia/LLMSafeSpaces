#!/usr/bin/env python3
# Copyright (C) 2026 Michael Kao
# SPDX-License-Identifier: AGPL-3.0-or-later
"""S-BOARD-CRUD canary (#1699, design 0067 §4.3a) — user-level board CRUD
round-trip: project → epic → task, structural dependencies, guard classes,
and the OR4 pr:N anchor tag form."""

from __future__ import annotations
import re
import sys, os

sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), ".."))
from canary import jwt_login, Runner, Config, config_from_env
from llmsafespaces import LLMSafeSpaces

PRJ = re.compile(r"^prj-[0-9a-z]{8}$")
EPC = re.compile(r"^epc-[0-9a-z]{8}$")
TSK = re.compile(r"^tsk-[0-9a-z]{8}$")
PR_ANCHOR = re.compile(r"^pr:[1-9][0-9]*$")


def run(r: Runner, cfg: Config) -> None:
    c = LLMSafeSpaces(cfg.api_url, api_key=jwt_login(cfg), timeout=20.0)

    # Fixture: a workspace row for the project target (ruling R8).
    ok, ws = r.assert_no_error(
        lambda: c.workspaces.create(name="canary-py-board", runtime="base", storage_size="1Gi"),
        "fixture-ws: no error",
    )
    if not ok or not ws:
        return
    ws_id = ws.id
    try:
        # P1: project create — short id shape prj-XXXXXXXX
        ok, proj = r.assert_no_error(
            lambda: c.board.create_project(
                name="canary-py-board", target_workspace_id=ws_id, tags=["canary"]
            ),
            "create-project: no error",
        )
        if not ok or not proj:
            return
        r.assert_(bool(PRJ.match(proj.get("short_id", ""))), "create-project: short_id shape", str(proj.get("short_id")))
        r.assert_(proj.get("status") == "active", "create-project: active", str(proj.get("status")))
        project_sid = proj["short_id"]

        # P2: list + get (case-insensitive short id)
        ok, _ = r.assert_no_error(lambda: c.board.list_projects(), "list-projects: no error")
        ok, got = r.assert_no_error(
            lambda: c.board.get_project(project_sid.upper()), "get-project: no error"
        )
        if ok and got:
            r.assert_(got.get("id") == proj["id"], "get-project: id matches", str(got.get("id")))

        # P3: patch (rename; tags replace)
        ok, upd = r.assert_no_error(
            lambda: c.board.update_project(project_sid, name="canary-py-board-2", tags=["canary", "board"]),
            "patch-project: no error",
        )
        if ok and upd:
            r.assert_(upd.get("name") == "canary-py-board-2", "patch-project: renamed", str(upd.get("name")))

        # P4: archive → unarchive
        ok, arch = r.assert_no_error(lambda: c.board.archive_project(project_sid), "archive-project: no error")
        if ok and arch:
            r.assert_(arch.get("status") == "archived", "archive-project: archived", str(arch.get("status")))
        ok, unarch = r.assert_no_error(lambda: c.board.unarchive_project(project_sid), "unarchive-project: no error")
        if ok and unarch:
            r.assert_(unarch.get("status") == "active", "unarchive-project: active", str(unarch.get("status")))

        # P5: epic create (draft default) → activate → double-pause guard
        ok, epic = r.assert_no_error(
            lambda: c.board.create_epic(project_sid, title="E1", outcome="canary"),
            "create-epic: no error",
        )
        if not ok or not epic:
            return
        r.assert_(bool(EPC.match(epic.get("short_id", ""))), "create-epic: short_id shape", str(epic.get("short_id")))
        r.assert_(epic.get("status") == "draft", "create-epic: draft default", str(epic.get("status")))
        epic_sid = epic["short_id"]
        ok, act = r.assert_no_error(lambda: c.board.epic_activate(epic_sid), "activate-epic: no error")
        if ok and act:
            r.assert_(act.get("status") == "active", "activate-epic: active", str(act.get("status")))
        ok, _ = r.assert_no_error(lambda: c.board.epic_pause(epic_sid), "pause-epic: no error")
        if ok:
            r.assert_error(lambda: c.board.epic_pause(epic_sid), "pause-paused-epic: guard error")

        # P6: task create with the OR4 pr:N anchor tag + criteria
        ok, task_a = r.assert_no_error(
            lambda: c.board.create_task(
                epic_sid,
                title="Task A",
                tags=["pr:1234"],
                acceptance_criteria=[{"text": "c1", "met": True}, {"text": "c2", "met": False}],
            ),
            "create-task: no error",
        )
        if not ok or not task_a:
            return
        r.assert_(bool(TSK.match(task_a.get("short_id", ""))), "create-task: short_id shape", str(task_a.get("short_id")))
        tags = task_a.get("tags") or []
        r.assert_(bool(PR_ANCHOR.match(tags[0])) if tags else False, "create-task: OR4 pr:N anchor tag form", ",".join(tags))
        task_a_sid = task_a["short_id"]

        ok, task_b = r.assert_no_error(
            lambda: c.board.create_task(epic_sid, title="Task B"), "create-task-b: no error"
        )
        if not ok or not task_b:
            return
        task_b_sid = task_b["short_id"]

        # P7: structural dependency — blocked derivation, guards, unblock
        ok, dep = r.assert_no_error(
            lambda: c.board.add_dependency(task_b_sid, depends_on=task_a_sid), "add-dependency: no error"
        )
        if ok and dep:
            r.assert_(dep.get("task", {}).get("is_blocked") is True, "add-dependency: blocked", "")
        r.assert_error(lambda: c.board.add_dependency(task_b_sid, depends_on=task_b_sid), "self-edge: guard error")
        r.assert_error(lambda: c.board.add_dependency(task_a_sid, depends_on=task_b_sid), "cycle: guard error")
        ok, rem = r.assert_no_error(
            lambda: c.board.remove_dependency(task_b_sid, task_a_sid), "remove-dependency: no error"
        )
        if ok and rem:
            r.assert_(rem.get("task", {}).get("is_blocked") is False, "remove-dependency: unblocked", "")

        # P8: patch task — criteria check-off (Done preparation)
        ok, checked = r.assert_no_error(
            lambda: c.board.update_task(task_a_sid, set_criteria_met=1), "patch-task-criteria: no error"
        )
        if ok and checked:
            crit = checked.get("acceptance_criteria") or []
            r.assert_(bool(crit) and crit[1].get("met") is True, "patch-task-criteria: met", "")

        # P9: stage guard — backlog leaves by claim, not by drag
        r.assert_error(
            lambda: c.board.move_stage(task_a_sid, to_stage="in_progress"), "move-stage-from-backlog: guard error"
        )

        # P10: listing with the enumerated stage filter; unknown stage 400s
        ok, listed = r.assert_no_error(
            lambda: c.board.list_project_tasks(project_sid, stage="backlog"), "list-tasks: no error"
        )
        if ok and listed is not None:
            r.assert_(len(listed) == 2, "list-tasks: two backlog", str(len(listed)))
        r.assert_error(lambda: c.board.list_project_tasks(project_sid, stage="wip"), "list-tasks-bad-stage: error")

        # P11 (#1747): delete task + delete epic (cancel is eliminated —
        # the result lists orphaned dependents; no stage/status comes back)
        ok, deleted = r.assert_no_error(
            lambda: c.board.delete_task(task_b_sid), "delete-task: no error"
        )
        if ok and deleted:
            r.assert_(deleted.get("item_kind") == "task", "delete-task: item kind", str(deleted.get("item_kind")))
        ok, de = r.assert_no_error(
            lambda: c.board.delete_epic(epic_sid), "delete-epic: no error"
        )
        if ok and de:
            r.assert_(de.get("item_kind") == "epic", "delete-epic: item kind", str(de.get("item_kind")))

        # P12: read perimeter — a near-miss id is a plain 404
        r.assert_error(lambda: c.board.get_project("prj-zzzzzzzz"), "get-project-nearmiss: 404")
    finally:
        try:
            c.workspaces.delete(ws_id)
        except Exception:
            pass


def main() -> None:
    r = Runner("board-crud", "python-sdk")
    run(r, config_from_env())
    res = r.print()
    if res.failed > 0:
        sys.exit(1)


if __name__ == "__main__":
    main()
