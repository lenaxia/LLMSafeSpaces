# workspace-agentd (PID 1) does not reap adopted orphan processes

**Date:** 2026-08-04
**Status:** Diagnosis only — fix not yet implemented
**Severity:** Operational — accumulates zombies over pod lifetime, indirectly contributed to the workspace-wedge incident documented in sibling worklog `liveness-probe-wrong-target`

---

## Objective

Document a structural bug in `cmd/workspace-agentd` discovered while investigating a wedged production workspace pod (see sibling worklog `liveness-probe-wrong-target`). The supervisor (`workspace-agentd --supervise`) is the container's PID 1 and inherits orphaned processes from opencode's tool subprocesss, but it does not install a SIGCHLD handler or call `waitpid(-1)` for adopted children. Those children become un-reapable zombies for the lifetime of the pod.

---

## Evidence

### Production pod — workspace `5c25e2ef-3f07-48f9-ae50-9769382e6da8`

Pod ran `npm run dev` for `frontend/` plus a Playwright e2e run executed as opencode tool calls. Inspected with `kubectl exec ... ps -ef` 72 minutes after pod boot:

```
UID        PID  PPID  C STIME TTY      TIME CMD
sandbox      1     0  0 03:36 ?    00:00:15 workspace-agentd --supervise
sandbox     38     1  6 03:36 ?    00:04:27 opencode serve --hostname 0.0.0.0 --port 4096
sandbox     73     1  0 03:42 ?    00:00:00 [bash] <defunct>
sandbox     92     1  0 03:42 ?    00:00:00 [MainThread] <defunct>
sandbox    162     1  0 03:43 ?    00:00:00 [pkill] <defunct>
sandbox    226     1  0 03:59 ?    00:00:00 [pkill] <defunct>
sandbox    263     1  0 04:02 ?    00:00:00 [chrome-headless] <defunct>
... (50+ additional [chrome-headless] <defunct> with PPID=1)
```

**Total: 91 zombie processes (`<defunct>`) all with PPID=1.**

### Why PPID=1

opencode (PID 38) is the parent that forks tool commands via `os/exec` — bash scripts, `npx playwright test`, etc. Those children fork their own children (bash → chrome-headless, python → chrome-headless, teardown → pkill). When an intermediate parent exits before its child, the kernel reparents the orphan to PID 1 in the same PID namespace. That is PID 1's job.

In a normal Linux system, `init` (systemd, sysvinit, etc.) reaps these via `waitpid(-1)` driven by SIGCHLD. In a container, PID 1 is whatever binary the container starts — here, `workspace-agentd`. A Go program that does not explicitly loop on `waitpid(-1)` (or install a SIGCHLD handler that does so) will accumulate zombies from every orphan reparented to it.

### Confirmed against `cmd/workspace-agentd` source

`managed_process.go` handles reaping **only for opencode itself** — the supervisor's direct child. It calls `cmd.Wait()` on the opencode process it started (see managed_process.go:139 comment "reaped by Wait(). Port resources are free."). There is no general-purpose orphan reaper.

Confirmed by source search: no `SIGCHLD` handler installation, no `syscall.Wait4(-1, ...)` loop, no `tini`/`dumb-init` indirection in the container's entrypoint (`entrypoint-opencode.sh` execs `workspace-agentd` directly as PID 1).

---

## Impact

1. **PID table pressure.** Each zombie occupies a PID. With Playwright spawning many short-lived chrome-headless helper processes per test, a long-running active workspace can accumulate hundreds of zombies. Linux default PID max (`/proc/sys/kernel/pid_max`) is 4 MiB on modern kernels, so we are not at risk of exhaustion soon — but the rate is unbounded.

2. **Observability noise.** Any operator running `ps` or `top` inside a workspace pod for diagnosis sees a wall of `<defunct>` rows that obscures real process state (as happened in the wedge incident documented in `liveness-probe-wrong-target`).

3. **Misleading metrics.** If any future scraper reports process counts, zombies will inflate them. (No current scraper does this; documented as defensive.)

4. **Indirect contribution to the wedge incident.** The zombies themselves did not deadlock opencode (their RSS is 0; the kernel keeps only a `task_struct`). But the volume of zombies made the diagnostic picture harder to read during the investigation.

---

## Root Cause

`workspace-agentd` was designed as PID 1 for the workspace container but implements only the responsibilities relevant to its primary task (supervise opencode, manage credentials, expose admin/user HTTP servers). It does not fulfill PID 1's most basic Unix responsibility: reaping reparented orphans.

This is a well-known class of bug — "Go binary as PID 1 in a container doesn't reap children" — and is why projects like [`tini`](https://github.com/krallin/tini) and [`dumb-init`](https://github.com/Yelp/dumb-init) exist. Both the Docker and Kubernetes ecosystems have recommended using an init system as PID 1 for over a decade.

---

## Proposed Fix (not yet implemented)

Three options, evaluated. Pick one.

### Option A — Add a SIGCHLD-driven reaper inside workspace-agentd (recommended)

A small goroutine in `cmd/workspace-agentd/main.go` that:
1. Registers a handler for `syscall.SIGCHLD`.
2. On signal, loops `syscall.Wait4(-1, &status, syscall.WNOHANG, nil)` until it returns 0 or -1 (ECHILD).

Approximate 20 LoC. Keeps workspace-agentd as PID 1 (no extra image layer, no extra binary in the runtime image). This is the same approach container runtimes use when `--init` is unavailable.

**Risks:** Go's `os/signal` uses a single channel per signal; if any other code in workspace-agentd also listens for SIGCHLD, the reaps will be split. Current source has no other SIGCHLD consumer — verified by grep.

### Option B — Use `tini` as PID 1, workspace-agentd as PID 2

Install `tini` in the runtime base image and exec it from `entrypoint-opencode.sh`:
```
exec tini -- workspace-agentd --supervise
```

`tini` is ~30 KB, statically linked, MIT-licensed, and exists for exactly this problem. It reaps orphans and forwards signals.

**Tradeoff:** adds a binary to every runtime image (`runtimes/`), increases image size marginally, requires rebuild + redeploy of every runtime tag. workspace-agentd must then be made robust to being PID 2 (it currently assumes PID 1 — verify this assumption before adopting).

### Option C — Both

Install `tini` (defense-in-depth, well-trodden path) AND have workspace-agentd install its own SIGCHLD reaper. Belt-and-suspenders. Marginal extra cost over Option A alone.

---

## Recommended choice: Option A

Smallest blast radius, no runtime image changes, no rebuild of deployed workspaces. The fix lands in the next workspace-agentd build and is picked up on the next workspace pod recreate. Option B remains available if a future incident shows PID 1 doing more than just reaping (e.g. signal forwarding issues).

---

## Files to Modify (when fix is implemented)

- `cmd/workspace-agentd/main.go` — install SIGCHLD handler + Wait4 loop before supervisor starts
- `cmd/workspace-agentd/orphan_reaper_test.go` (new) — at minimum: spawn a child that spawns a grandchild, kill the child, assert grandchild is reaped by the agentd process within a bounded window
- `cmd/workspace-agentd/managed_process.go` — verify no conflict with existing opencode `Wait()` consumer (none found during diagnosis, but the implementing agent must re-verify)

---

## Assumptions (per Rule 7)

| Assumption | Validation |
|---|---|
| workspace-agentd is the container's PID 1 | Confirmed: `ps -ef` shows PID 1 = `workspace-agentd --supervise` |
| workspace-agentd does not install a SIGCHLD handler | Confirmed by grep: no `SIGCHLD` references in `cmd/workspace-agentd/` source outside of comment context |
| `managed_process.go` reaps only its direct opencode child | Confirmed by reading the file: it calls `cmd.Wait()` on the opencode process it started; no general-purpose `Wait4(-1, ...)` |
| Orphans reparent to PID 1 inside the container's PID namespace | Standard Linux kernel behavior; this is why the zombies have PPID=1 |
| The zombies did not directly cause opencode's deadlock | Inferred: zombie task_structs occupy no RSS; opencode's deadlock was a goroutine/lock issue (see `liveness-probe-wrong-target`). Zombies are a co-finding, not the root cause. |

---

## Tests Run (during diagnosis)

None — diagnosis only. The fix will require:
- New unit test asserting orphan reaping
- Manual verification on a real pod by triggering a multi-level fork (e.g. `bash -c 'sleep 60 & exit'` then check no zombie)

---

## Related

- **Sibling worklog `agentd-supervisor-blind-to-http-deadlock`** — orthogonal bug, same incident
- **Sibling worklog `liveness-probe-wrong-target`** — orthogonal bug, same incident
