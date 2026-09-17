# Worklog NNNN — gVisor bundle provisioning must install the sidecar tree (STRICT policy)

**Date:** 2026-09-17
**Session:** Coordinator hot-fix — pool run 35169738298 (post-#1403, build retries green) failed the secret-delivery rows: every gVisor workspace pod wedged in sandbox creation — `FailedCreatePodSandBox: sidecar "gvisor_sentry" not usable (stat /usr/local/bin/gvisor-bin/gvisor_sentry: no such file or directory) and --sidecar-usage-policy is set to STRICT` — so AC-13 wave 1 never went Active. Root cause: the #1375 bundle flow installed only the two top-level binaries (`runsc`, `containerd-shim-runsc-v1`); the current release bundle's runsc stages its sentry under `gvisor-bin/` and runs STRICT — the tree must be installed too. (Regression introduced by #1375; the last green pool predates it.)
**Status:** Complete

## Work completed (TDD)

1. **RED:** `TestUS70GvisorBundle_InstallsSidecarTree` — requires the tar extraction to include `gvisor-bin` and the install to land it at `/usr/local/bin/gvisor-bin` (the STRICT lookup path). Failed against the two-binary extraction.
2. **Fix (`local/lib/gvisor.sh`):** extract `gvisor-bin` with the two binaries; `rm -rf /usr/local/bin/gvisor-bin && cp -a /tmp/gvisor-bin /usr/local/bin/gvisor-bin`; cleanup extended. Also caught in-house: the fix comment originally contained an apostrophe that terminated the single-quoted `bash -c` block (the #1373 "curl's" trap class) — `bash -n` now green.

## Tests run

- `go test -timeout 300s ./local/` — ok; `bash -n local/lib/gvisor.sh` clean; `make repolint` passed.

## Files modified

- `local/lib/gvisor.sh`, `local/us70_harness_script_test.go`, this worklog
