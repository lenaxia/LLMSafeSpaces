// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later
// S-BOARD-CRUD canary (#1699, design 0067 §4.3a) — user-level board CRUD
// round-trip: project → epic → task, structural dependencies, the guard
// classes, and the OR4 `pr:N` anchor tag form.

import { LLMSafeSpaces } from '../../../typescript/src/index.js';
import { jwtLogin, Runner, Config, configFromEnv, nodeFetch } from '../canary.js';

async function run(r: Runner, cfg: Config): Promise<void> {
  const c = new LLMSafeSpaces({ baseUrl: cfg.apiUrl, apiKey: await jwtLogin(cfg), timeout: 20000, fetch: nodeFetch as any });

  // Fixture: a workspace row for the project target (ruling R8).
  const [wsOk, ws] = await r.assertNoError(
    () => c.workspaces.create({ name: 'canary-ts-board', runtime: 'base', storageSize: '1Gi' }),
    'fixture-ws: no error',
  );
  if (!wsOk || !ws) return;
  const wsID = (ws as { id: string }).id;
  try {
    // P1: project create — short id shape prj-XXXXXXXX
    const [pOk, proj] = await r.assertNoError(
      () => c.board.createProject({ name: 'canary-ts-board', targetWorkspaceId: wsID, tags: ['canary'] }),
      'create-project: no error',
    );
    if (!pOk || !proj) return;
    r.assert(/^prj-[0-9a-z]{8}$/.test(proj.short_id), 'create-project: short_id shape', proj.short_id);
    r.assert(proj.status === 'active', 'create-project: active', proj.status);
    const projectSid = proj.short_id;

    // P2: project list + get (case-insensitive short id)
    await r.assertNoError(() => c.board.listProjects(), 'list-projects: no error');
    const [gOk, got] = await r.assertNoError(() => c.board.getProject(projectSid.toUpperCase()), 'get-project: no error');
    if (gOk && got) r.assert(got.id === proj.id, 'get-project: id matches', `${got.id} vs ${proj.id}`);

    // P3: patch (rename; tags replace)
    const [uOk, upd] = await r.assertNoError(
      () => c.board.updateProject(projectSid, { name: 'canary-ts-board-2', tags: ['canary', 'board'] }),
      'patch-project: no error',
    );
    if (uOk && upd) r.assert(upd.name === 'canary-ts-board-2', 'patch-project: renamed', upd.name);

    // P4: archive → unarchive
    const [aOk, arch] = await r.assertNoError(() => c.board.archiveProject(projectSid), 'archive-project: no error');
    if (aOk && arch) r.assert(arch.status === 'archived', 'archive-project: archived', arch.status);
    const [unOk, unarch] = await r.assertNoError(() => c.board.unarchiveProject(projectSid), 'unarchive-project: no error');
    if (unOk && unarch) r.assert(unarch.status === 'active', 'unarchive-project: active', unarch.status);

    // P5: epic create (draft default) → activate → double-pause guard
    const [eOk, epic] = await r.assertNoError(
      () => c.board.createEpic(projectSid, { title: 'E1', outcome: 'canary' }),
      'create-epic: no error',
    );
    if (!eOk || !epic) return;
    r.assert(/^epc-[0-9a-z]{8}$/.test(epic.short_id), 'create-epic: short_id shape', epic.short_id);
    r.assert(epic.status === 'draft', 'create-epic: draft default', epic.status);
    const epicSid = epic.short_id;
    const [acOk, act] = await r.assertNoError(() => c.board.activateEpic(epicSid), 'activate-epic: no error');
    if (acOk && act) r.assert(act.status === 'active', 'activate-epic: active', act.status);
    const [p1Ok] = await r.assertNoError(() => c.board.pauseEpic(epicSid), 'pause-epic: no error');
    if (p1Ok) await r.assertError(() => c.board.pauseEpic(epicSid), 'pause-paused-epic: guard error');

    // P6: task create with the OR4 pr:N anchor tag + criteria
    const [tOk, taskA] = await r.assertNoError(
      () =>
        c.board.createTask(epicSid, {
          title: 'Task A',
          tags: ['pr:1234'],
          acceptanceCriteria: [
            { text: 'c1', met: true },
            { text: 'c2', met: false },
          ],
        }),
      'create-task: no error',
    );
    if (!tOk || !taskA) return;
    r.assert(/^tsk-[0-9a-z]{8}$/.test(taskA.short_id), 'create-task: short_id shape', taskA.short_id);
    r.assert(/^pr:[1-9][0-9]*$/.test(taskA.tags?.[0] ?? ''), 'create-task: OR4 pr:N anchor tag form', (taskA.tags ?? []).join(','));
    const taskASid = taskA.short_id;

    const [tbOk, taskB] = await r.assertNoError(
      () => c.board.createTask(epicSid, { title: 'Task B' }),
      'create-task-b: no error',
    );
    if (!tbOk || !taskB) return;
    const taskBSid = taskB.short_id;

    // P7: structural dependency — blocked derivation, self-edge guard,
    // cycle guard, unblock on remove
    const [dOk, dep] = await r.assertNoError(
      () => c.board.addDependency(taskBSid, { dependsOn: taskASid }),
      'add-dependency: no error',
    );
    if (dOk && dep) r.assert(dep.task?.is_blocked === true, 'add-dependency: blocked', JSON.stringify(dep.task?.blocked_by));
    await r.assertError(() => c.board.addDependency(taskBSid, { dependsOn: taskBSid }), 'self-edge: guard error');
    await r.assertError(() => c.board.addDependency(taskASid, { dependsOn: taskBSid }), 'cycle: guard error');
    const [rOk, rem] = await r.assertNoError(
      () => c.board.removeDependency(taskBSid, taskASid),
      'remove-dependency: no error',
    );
    if (rOk && rem) r.assert(rem.task?.is_blocked === false, 'remove-dependency: unblocked', '');

    // P8: patch task — criteria check-off (Done preparation)
    const [ckOk, checked] = await r.assertNoError(
      () => c.board.updateTask(taskASid, { setCriteriaMet: 1 }),
      'patch-task-criteria: no error',
    );
    if (ckOk && checked) r.assert(checked.acceptance_criteria?.[1]?.met === true, 'patch-task-criteria: met', '');

    // P9: stage guard — backlog leaves by claim, not by drag
    await r.assertError(() => c.board.moveStage(taskASid, { toStage: 'in_progress' }), 'move-stage-from-backlog: guard error');

    // P10: listing with the enumerated stage filter; unknown stage is a 400
    const [lOk, listed] = await r.assertNoError(
      () => c.board.listProjectTasks(projectSid, 'backlog'),
      'list-tasks: no error',
    );
    if (lOk && listed) r.assert(listed.tasks.length === 2, 'list-tasks: two backlog', `${listed.tasks.length}`);
    await r.assertError(() => c.board.listProjectTasks(projectSid, 'wip'), 'list-tasks-bad-stage: error');

    // P11 (#1747): delete task + delete epic (cancel is eliminated — the
    // result lists orphaned dependents; no stage/status comes back)
    const [cOk, del] = await r.assertNoError(
      () => c.board.deleteTask(taskBSid),
      'delete-task: no error',
    );
    if (cOk && del) r.assert(del.item_kind === 'task', 'delete-task: item kind', del.item_kind);
    const [ceOk, de] = await r.assertNoError(
      () => c.board.deleteEpic(epicSid, true),
      'delete-epic: no error',
    );
    if (ceOk && de) r.assert(de.item_kind === 'epic', 'delete-epic: item kind', de.item_kind);

    // P12: cross-tenant read perimeter — a near-miss id is a plain 404
    await r.assertError(() => c.board.getProject('prj-zzzzzzzz'), 'get-project-nearmiss: 404');

    // P12b (#1699 seams): epic listing carries derived progress
    const [eOk, epics] = await r.assertNoError(
      () => c.board.listBoardEpics(projectSid),
      'list-epics: no error',
    );
    if (eOk && epics) r.assert(epics.epics.length === 1 && epics.epics[0].progress_derived === true, 'list-epics: derived', '');

    // P12c (#1699 seams): board search — hit + near-miss suggestion
    const [hOk, hits] = await r.assertNoError(
      () => c.board.searchBoard('Task A'),
      'search-hit: no error',
    );
    if (hOk && hits) r.assert(hits.results.length === 1, 'search-hit: one result', `${hits.results.length}`);
    const [nOk, near] = await r.assertNoError(
      () => c.board.searchBoard(mutateShortID(taskASid)),
      'search-nearmiss: no error',
    );
    if (nOk && near) {
      r.assert(near.results.length === 0, 'search-nearmiss: zero results', '');
      r.assert(near.suggestions.includes(taskASid), 'search-nearmiss: suggested', '');
    }

    // P12d (#1699 seams): the health snapshot renders
    const [hhOk, health] = await r.assertNoError(
      () => c.board.getBoardHealth(),
      'board-health: no error',
    );
    if (hhOk && health) {
      r.assert(Array.isArray(health.active_leases), 'board-health: leases key', '');
      r.assert(health.pending_input_by_origin != null, 'board-health: origins key', '');
    }

    // P13 (#1699 replay keys): post-cancel activate from paused; the
    // SAME requestId twice renders 200-with-original on the replay
    const replayKey = '11111111-2222-3333-4444-555555555555';
    const [a1Ok, act1] = await r.assertNoError(
      () => c.board.activateEpic(epicSid, { requestId: replayKey }),
      'lifecycle-replay-1: no error',
    );
    if (a1Ok && act1) r.assert(act1.status === 'active', 'lifecycle-replay-1: active', String(act1.status));
    const [a2Ok, act2] = await r.assertNoError(
      () => c.board.activateEpic(epicSid, { requestId: replayKey }),
      'lifecycle-replay-2: 200-with-original',
    );
    if (a2Ok && act2) r.assert(act2.status === 'active', 'lifecycle-replay-2: original status', String(act2.status));
  } finally {
    try {
      await c.workspaces.delete(wsID);
    } catch {
      // fixture cleanup is best-effort
    }
  }
}

function mutateShortID(sid: string): string {
  const tail = sid.endsWith('0') ? '1' : '0';
  return sid.slice(0, -1) + tail;
}

async function main() {
  const r = new Runner('board-crud');
  await run(r, configFromEnv());
  r.print();
  process.exit(r.exitCode());
}
main().catch(e => { console.error(e); process.exit(1); });
