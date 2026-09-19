# Worklog: /opencode/plugins traversal-mode fix (origin plugin silent no-op)

**Date:** 2026-09-19
**Session:** #1469 follow-up incident — the origin-injection plugin shipped untraversable; every send_message degraded to self-declared origin on the live pod.
**Status:** Complete

---

## Objective

Live symptom: agentd refused origin injection (orchestrator's sends fell back to from_session_id self-declared mode). Diagnosis (orchestrator, live pod): `/opencode/plugins/llmsafespaces-origin.js` EXISTS in the overlay mount; the sidecar-written agent-config.json carries the plugin entry (ConfigWriter/injectPlatformAgentConfig worked); **but /opencode/plugins is mode 644 — no execute bit — untraversable by uid 1000 (opencode's runtime uid); the file:// import fails silently → plugin no-op → graceful fallback (the #1469 hybrid design degraded exactly as specified — nothing broke, provenance just lost its attested mode).**

## Root cause

`COPY --chmod=644 runtimes/opencode/plugins/llmsafespaces-origin.js /plugins/llmsafespaces-origin.js` — BuildKit applies `--chmod` to IMPLICITLY CREATED PARENT DIRECTORIES as well as the copied entry. The 644 was meant for the .js file; it also landed on the auto-created `/plugins`. Incident-validated semantics (the live dir proves 644 propagated).

## Work Completed

1. **Fix**: the COPY is now `--chmod=755` (plugin is data; the x bit on it is harmless — the DIR's traversal bit is load-bearing) with the full-rationale comment pinned in the Dockerfile.
2. **Source pin** (`pkg/repolint/opencode_plugins_mode_test.go`): every Dockerfile COPY referencing `plugins` must carry a traversable chmod (644/666/640 rejected — BuildKit implicit-parent semantics); RED on the old line, GREEN on the fix.
3. **Built-image pin** (ci.yml, `Build Opencode (linux/amd64)` job): after the push build, a cache-hit `--load` rebuild → `docker create` + `docker export` → tar-header assertion that `/plugins` carries the traversal bit (drwx*x*x*x) and the plugin file is readable. The assertion reads the REAL artifact's layer modes — a source-level pin cannot see this failure class (the source tree's modes were always fine; BuildKit created the dir at build time).
4. Deployment note (orchestrator's): rides the next train; the pod's overlay activates after the next compute refresh.

## Key Decisions

- 755 over splitting the chmod: scratch-stage has no shell (no RUN to pre-create the dir); a single traversable chmod on file+implicit-dir is the minimal correct form and matches the binary COPY's existing 755 convention.
- Image-level assertion over source-only: this incident's entire failure surface was BUILD-TIME dir creation — invisible to every source pin. The tar-header check closes exactly that gap, cheaply (builder cache).
- The amd64 leg only: mode semantics don't vary by arch; one leg keeps the cost at seconds.

## Assumptions → validation record (Rule 7)

- "BuildKit applies --chmod to implicit parents" → incident-validated (live /plugins = 644 from a 644 file-copy); the fix mirrors it (755 file-copy → 755 dir), verified by the CI tar-mode assertion once it runs.
- "uid 1000 needs x on every path component" → POSIX traversal; the orchestrator's uid-1000 shell reproduced the Permission denied.
- "docker export tar lists dirs with trailing slash and mode in field 1" → standard tar -tv output; the awk matches both `plugins` and `plugins/` forms; first CI run is the mechanical confirmation (no docker in this pod — stated, not silently skipped).

## Blockers

None. (Local docker verification impossible — dev pod has no docker daemon by design; the CI job is the verification surface.)

## Tests Run

- `go test ./pkg/repolint/ -run TestOpencodeOverlay_PluginsCopy` — RED on 644, GREEN on 755.
- ci.yml parses; the mode-pin step is YAML-valid.
- CI will exercise: full package + the new built-image assertion on the next push.

## Next Steps

1. Merge → next train rolls the overlay → compute refresh activates the plugin on this pod → the orchestrator's sends regain injected (attested) mode. Watch the first post-merge Build Opencode run for the tar-mode step.
2. Optional follow-up (not this PR): surface plugin-load failures loudly — opencode logs the import error; agentd could watch for it at boot and report degraded mode via statusz (the silent no-op made this incident invisible until a human noticed the fallback).

## Files Modified

- `runtimes/opencode/Dockerfile` (chmod 755 + rationale comment)
- `pkg/repolint/opencode_plugins_mode_test.go` (new source pin)
- `.github/workflows/ci.yml` (built-image /plugins mode assertion in Build Opencode amd64)
- `worklogs/NNNN_2026-09-19_opencode-plugins-dir-mode.md` (this file)
