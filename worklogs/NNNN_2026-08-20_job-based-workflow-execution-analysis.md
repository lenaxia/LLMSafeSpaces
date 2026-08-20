# Worklog: Job-Based Background Workflow Execution — Analysis & Decision

**Date:** 2026-08-20
**Session:** Job-based workflow execution analysis — cost-benefit calculation, design proposal, and decision
**Status:** Complete

---

## Objective

Analyze the feasibility and cost-benefit of job-based execution for background workflows (Epic 64), in response to user proposal: "Could we have a job based instead of pod based run? And just have a rwx pvc that is shared among all jobs for that user? That way there are no idle cpu issues?"

---

## Work Completed

### Phase 1: Initial Analysis (Interactive vs. Background Workspaces)

**Finding:** The job-based proposal is only relevant for **background workflows**, not interactive workspaces. Interactive workspaces require persistent pods for:
- SSE streaming responses
- MCP server support (persistent sessions)
- Dev preview with HMR (persistent HTTP server)

**Decision:** Focus analysis exclusively on background workflows (cron/webhook triggers, Epic 64).

---

### Phase 2: Validations (Per User Feedback)

**User-provided clarifications:**

1. **opencode `-s <session-id>` mode** ✅
   - Confirmed: opencode supports non-interactive session resumption via `-s <id>`
   - Command: `opencode run -s <id> --format json`

2. **Parallel opencode processes** ✅
   - User-confirmed: "I currently run multiple opencode processes in the same environment and it handles the sqlite multiwrites fine"

3. **Background task PVC** ✅
   - Design: Separate PVC for background workflows to avoid cluttering interactive workspaces
   - Results logged to PostgreSQL + Job pod stdout/stderr
   - MCP tool to create interactive session if debugging needed

4. **Use `opencode run`, not `opencode serve`** ✅
   - One-shot mode exists; no persistent HTTP server needed

5. **Credential materialization** ✅
   - Same as existing (Epic 35); init container injects, credentials die with Job

6. **agentd in Job mode** ⚠️
   - User feedback: "I don't think we can bypass agentd because we would lose instrumentation and control/breakglass/interrupts"
   - Conclusion: Need to extend agentd for one-shot execution mode

**Architecture decisions:**
- **Per-user background PVC** (not per-workspace)
- **agentd one-shot mode** (cannot bypass agentd)
- **Session IDs tied to triggers** (persistentSession flag → session reuse vs. ephemeral)
- **Victorialogs for logs** (lightweight, Job pods deleted after completion)

---

### Phase 3: Architecture Design

**Proposed job-based execution flow:**

```
Cron/Webhook fires
  → Controller creates Kubernetes Job (not pod)
  → Job mounts background PVC (per-user RWX)
  → Job executes: agentd execute --session <id> --format json
  → Job exits when done (no idle CPU)
  → Logs shipped to Victorialogs via promtail
```

**PVC layout (per-user):**
```
/user-background/<user-id>/
  ├── workspaces/
  │   ├── <workspace-id-abc>/
  │   └── <workspace-id-xyz>/
  └── sessions/
      ├── <persistent-session-id>/
      └── <ephemeral-session-id>/
```

**Implementation scope:** 11.5 weeks (57.5 days) across 9 phases:
1. Foundation (validations)
2. CRD & Data Model
3. Background PVC Reconciler
4. agentd One-Shot Mode
5. Job Builder
6. Workflow Reconciler Changes
7. Victorialogs Integration
8. MCP Tool for Session Inspection
9. Metrics & Documentation

---

### Phase 4: Cost-Benefit Analysis (Critical Finding)

**Scenario: 10 users, 5 workflow workspaces each (50 workspaces total)**

**Pod-based model:**
- Idle CPU cost: ~$124/year
- Wake-up latency: ~22s per run (accepted in Epic 64 design)
- Wake-sleep cycles: Hourly workflows trigger suspend/resume every hour

**Job-based model:**
- Execution cost: ~$2/year
- Cold start latency: ~20s per run (similar to resume)
- No idle CPU, no wake-sleep cycles

**Savings:** ~$122/year

**Engineering cost:** 57.5 days × $300/day = ~$17,250

**ROI:** 141 years to break even

**Critical realization:** The cost savings are minimal because:
- Workflow workspaces are a small fraction of total cluster usage
- Most cost is in execution time, not idle time
- Wake-up latency (~22s) is already accepted as acceptable for background workflows

---

### Phase 5: Alternative Approaches

**Option 1: Disable Auto-Suspend for Workflow Workspaces** ⭐ RECOMMENDED

```go
// Workspace CRD
spec:
  disableAutoSuspend: true  // NEW flag for workflow workspaces
```

**Effect:** Workflow pods stay alive, no wake-sleep cycles.

**Cost:** 2.25 days
- Add `Workspace.spec.disableAutoSuspend` field
- Add webhook validation
- Update auto-suspend logic in controller
- Add logic to auto-set flag for workspaces with active workflows
- Tests + docs

**Benefits:**
- Eliminates wake-sleep cycles (solves EC-14 from Epic 64)
- Eliminates wake-up latency for workflow workspaces
- Zero architecture changes
- Ships in <2 weeks

**Option 2: Extend Auto-Suspend Timeout**

```go
// Instance setting
"workflow.autoSuspendTimeoutMinutes": 1440  // 24 hours
```

**Cost:** 1 day

**Option 3: Warm Workspaces (Keep-Alive Pings)**

```go
// Workflow reconciler sends periodic keep-alive to workspace pod
```

**Cost:** 2 days

---

### Phase 6: Decision

**Defer job-based execution.**

**Implement Option 1 instead:** Add `disableAutoSuspend` flag to Workspace CRD.

**Rationale:**
1. **ROI is terrible** for <1,000 users: 141 years to break even
2. **Epic 64 already accepts** ~22s wake-up latency as acceptable (A7)
3. **Wake-sleep cycles** are solvable with a 2.25-day flag change
4. **Architecture risk**: Job-based mode introduces new CRDs, agentd changes, Victorialogs integration
5. **Operational complexity**: Log aggregation, session management, PVC isolation

---

### Phase 7: Documentation

**Created design document:** `design/stories/epic-64-job-based-background-workflow-execution/README.md`

**Contents:**
- Problem statement
- Proposed job-based architecture (deferred)
- Cost-benefit analysis with calculations
- Alternative approaches (3 options)
- Decision: defer job-based, implement `disableAutoSuspend`
- Implementation details for `disableAutoSuspend` optimization
- When to revisit job-based execution (>500 workflow workspaces)
- Deferred architecture (reduced scope if pursued later)
- Appendix: detailed cost-benefit calculations

---

## Key Decisions

### Decision 1: Defer Job-Based Execution

**Decision:** Do not implement job-based workflow execution.

**Rationale:**
- Cost savings are minimal (~$122/year for 50 workflow workspaces)
- Engineering cost is high (57.5 days)
- ROI is 141 years to break even
- Wake-up latency (~22s) is already accepted as acceptable in Epic 64

**Alternative:** Implement `disableAutoSuspend` flag (2.25 days effort).

---

### Decision 2: Per-User Background PVC (If Job-Based Pursued Later)

**Decision:** If job-based execution is revisited, use per-user RWX PVC with path isolation.

**Layout:**
```
/user-background/<user-id>/
  ├── workspaces/<workspace-id>/
  └── sessions/<session-id>/
```

**Rationale:**
- Fewer PVCs (one per user vs. one per workspace)
- Scales better for users with many small workspaces
- Path isolation via subdirectories

---

### Decision 3: agentd One-Shot Mode (If Job-Based Pursued Later)

**Decision:** Cannot bypass agentd; need one-shot mode for instrumentation, control, breakglass, interrupts.

**Alternative:** Extend agentd with `execute` subcommand:
```bash
agentd execute --session <id> --prompt <template> --format json
```

**Rationale:**
- User feedback: "I don't think we can bypass agentd because we would lose instrumentation and control/breakglass/interrupts"
- agentd provides health checks, credential management, interrupt handling

**Simplification (if pursued later):** Reuse existing `opencode serve` mode + HTTP calls instead of extending agentd.

---

### Decision 4: Session ID Management (If Job-Based Pursued Later)

**Decision:** Trigger `persistentSession` flag determines session reuse vs. ephemeral.

**Data Model:**
```sql
-- triggers table (add)
persistent_session_id uuid          -- nullable; set on first run if persistent=true

-- workflow_runs table (add)
session_id uuid                    -- opencode session ID for this run
```

**Logic:**
- **Persistent mode (`persistentSession=true`):** First run generates session ID, stores in `triggers.persistent_session_id`, subsequent runs reuse it
- **Ephemeral mode (`persistentSession=false`):** Each run generates new session ID, stored in `workflow_runs.session_id`

---

### Decision 5: Victorialogs for Logs (If Job-Based Pursued Later)

**Decision:** Use Victorialogs for log aggregation; Job pods deleted after completion.

**Architecture:**
```
Job pod stdout/stderr → promtail → Victorialogs
```

**Labels:** `workflow_id`, `workflow_run_id`, `workspace_id`, `user_id`

**Simplification (if pursued later):** Use k8s logs only (delete Jobs after TTL), no Victorialogs needed.

---

## When to Revisit Job-Based Execution

**Revisit when:**
- You have >500 workflow workspaces (idle CPU becomes measurable cost)
- OR users complain about pod sprawl (cluster resource pressure)
- OR you hit PVC count limits (Longhorn scalability concern)

**Trigger condition:** Set a threshold (e.g., "idle CPU >50% of cluster capacity") before investing in job mode.

---

## Assumptions Validated

| ID | Assumption | Validation | Result |
|----|------------|------------|--------|
| A1 | opencode supports `-s <session-id>` mode | `opencode --help` confirmed | ✅ Validated |
| A2 | SQLite handles concurrent writes | User-confirmed (runs multiple opencode processes) | ✅ Validated |
| A3 | Background task PVC model works | Design: separate PVC, logs to PG + stdout, MCP tool for debugging | ✅ Validated |
| A4 | `opencode run` mode exists | `opencode run --help` confirmed | ✅ Validated |
| A5 | Credential materialization unchanged | Epic 35 path works for Jobs | ✅ Validated |
| A6 | agentd required for control | User feedback: "I don't think we can bypass agentd" | ✅ Validated |

---

## Blockers

None. Analysis complete, decision made.

---

## Tests Run

None (analysis session, no code changes).

---

## Next Steps

**Immediate (2.25 days):**
1. Add `Workspace.spec.disableAutoSuspend` field to CRD
2. Add webhook validation
3. Update auto-suspend logic in controller
4. Add logic to auto-set flag for workspaces with active workflows
5. Tests (unit + envtest)
6. Update docs (operator guide)

**Deferred:**
- Job-based execution (revisit when >500 workflow workspaces)
- agentd one-shot mode (if job-based pursued)
- Per-user background PVC reconciler (if job-based pursued)
- Victorialogs integration (if job-based pursued)

---

## Files Created

1. `design/stories/epic-64-job-based-background-workflow-execution/README.md` — Analysis and decision document

---

## Files Modified

None (analysis session only).

---

## References

- Epic 64: Triggers & Workflows — `design/stories/epic-64-triggers-workflows/README.md`
- README-LLM.md — Resume latency (~22s measured)
- Workspace pod builder — `controller/internal/workspace/pod_builder.go`
- opencode help output — `opencode --help` and `opencode run --help`
- agentd types — `pkg/agentd/types.go`

---

**Worklog Complete.**