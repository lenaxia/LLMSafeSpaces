# local/lib — shared e2e harness libraries

Bash libraries sourced by the cluster e2e suites in `local/`. Not standalone
scripts: they define functions and (for `us70-common.sh`) expect the caller's
env contract.

## us70-common.sh

Shared machinery for the Epic 70 secret-delivery suites
(`../us-70-secret-delivery-e2e.sh`, `../us-70-faults-e2e.sh`): logging
(`log`/`ok`/`warn`/`die`), `kc`, port-forward lifecycle (`harness_start`,
`reconnect_api`, cleanup trap), `wait_phase`, `secrets_converged`,
`pod_of`, `runtime_container`, `agent_environ` (the `/proc/<agent>/environ`
read), `env_in_child`, `seed_user` (psql), `seed_workspace` (Workspace CR
apply — adds the `llmsafespaces.dev/allow-runtime-class-override` annotation
when a runtime class is set, per the admission webhook), `bind_env`, and
`detect_runtime_class` (gVisor feature-detect).

Env contract (names and defaults are load-bearing — the nightly and pool
workflows set them): `CLUSTER_NAME`, `CTX`, `NS`, `PORTFWD_PORT`, `API_KEY`,
`USER_ID`, `WS_BASE`, `SUSPEND_SECONDS`, `RESUME_SCALE`,
`RESUME_SCALE_TIMEOUT_S`, `P95_BUDGET_MS`.

## gvisor.sh

runsc provisioning for kind nodes, extracted from the proven S5.6 recipe in
`../s5-overlay-validation.sh` (which keeps its own inline copy deliberately —
do not refactor the weekly-validated script). CLI:

```bash
bash local/lib/gvisor.sh install [node-name]   # default: first node
bash local/lib/gvisor.sh runtimeclass          # apply gvisor/runsc RuntimeClass
```

Downloads `runsc` + `containerd-shim-runsc-v1` from the gVisor latest
release with sha512 verification (the `^([0-9a-f]{128})$` guard is pinned by
`../us70_harness_script_test.go`), registers the containerd runtime handler,
restarts containerd, and applies the RuntimeClass.
