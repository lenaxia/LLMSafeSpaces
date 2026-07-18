# gVisor Overhead Benchmark (Epic 51 S51.1 AC #8)

**Acceptance criterion 8:** *"Performance: gVisor overhead on a representative workload (LLM-coding session) measured and documented — accept/reject gVisor based on <30% overhead target."*

This page defines what "measured" means here, how to run the measurement on your cluster, and how to make the accept/reject call. The companion script is at [`helm/scripts/gvisor-benchmark.sh`](https://github.com/lenaxia/LLMSafeSpaces/blob/main/helm/scripts/gvisor-benchmark.sh).

## Why this isn't already done

Epic 51 S51.1 shipped the gVisor RuntimeClass wiring, the admin-gated opt-out, and the per-workspace `spec.runtimeClass` field. The default (`gvisor.enabled: false`) was kept because AC #8 requires a measured overhead on a **real** LLM-coding workload before flipping the default. That measurement cannot be done in the abstract — it depends on your node hardware, your workspace image's syscall profile, and your typical LLM round-trip latency. The script below runs the measurement on your cluster so the result reflects your deployment, not a synthetic benchmark run somewhere else.

The single biggest RCE-resistance win available is flipping `gvisor.enabled: true` once the measurement is in hand. The default stays off only because flipping it unmeasured violates AC #8 and Rule 7 of the project guidelines ("validate every assumption").

## What the benchmark measures

Three phases of a real LLM-coding session, run under both `runc` and `gvisor` on the same node pool with the same workspace image:

| Phase | What it captures | Why it's in the benchmark |
|---|---|---|
| **pod_boot** | Pending → Active wall-clock | Dominated by PVC attach + opencode boot — the init path most likely to surface gVisor's syscall-interception cost. |
| **cold_prompt** | First user message → first assistant token (SSE) | Proxy + opencode + first LLM API round-trip. LLM API latency dominates, which is the point: if gVisor's overhead is invisible at the workload level because LLM latency swamps it, that is the answer AC #8 wants. |
| **file_io** | Write 5 MiB random to `/workspace`, read back, sha256 | Pure pod-local syscall cost — the worst case per gvisor.dev's own docs (5–30% on syscall-heavy workloads). Isolates what gVisor *would* cost if your workload were I/O-bound. |

Phases 1 and 2 intentionally include non-pod latency. A microbenchmark that strips out the LLM API call would report a higher gVisor overhead percentage than any real user will experience, and would mislead the accept/reject decision. Phase 3 isolates the worst case so you can see both numbers.

## Running the benchmark

```bash
export API_URL=https://safespaces.example.com
export API_TOKEN=<a token that can create+delete workspaces>
export ITERATIONS=5           # per runtime per phase; bump for tighter CI
# Optional overrides (defaults shown):
# export WORKSPACE_RUNTIME=base              # RuntimeEnvironment name
# export WORKSPACE_ORGID=                    # empty = personal workspaces; set for org-scoped benchmarks
# export LLM_MODEL=opencode/free             # model for the cold_prompt phase

helm/scripts/gvisor-benchmark.sh > gvisor-bench.tsv
```

The script writes one row per (runtime × phase × iteration) as TSV to stdout;
progress logs go to stderr. Sample output:

```
runtime  phase        iteration  seconds
runc     pod_boot     1          23.412
runc     cold_prompt  1          2.187
runc     file_io      1          0.142
gvisor   pod_boot     1          24.801
gvisor   cold_prompt  1          2.205
gvisor   file_io      1          0.181
...
```

**Cost:** with `ITERATIONS=5`, the script creates and tears down 10 workspaces
(5 per runtime), each taking ~25–30 s to boot. End-to-end runtime is roughly
**15–25 minutes** plus LLM API latency on the cold prompts. Run it during a
quiet period; it does not interfere with other workspaces but does consume
node resources.

**Cleanup:** the script deletes every workspace it creates, including on
Ctrl-C (via a trap). If the script crashes hard, leftover workspaces will
have names starting `gvisor-bench-` — `kubectl delete workspace -n llmsafespaces -l llmsafespaces.dev/bench=gvisor-benchmark`
or `kubectl delete workspace -n llmsafespaces $(kubectl get workspaces -n llmsafespaces -o name | grep gvisor-bench)`
will clean them up.

### Prerequisites

1. At least one node with `runsc` installed and registered as a container
   runtime handler. See [`docs/operator/security.md` §gVisor](security.md#gvisor-kernel-isolation).
2. The chart deployed with `gvisor.enabled: true` so the `gvisor`
   RuntimeClass exists. **Do not** also flip the default in the chart yet —
   the benchmark needs to opt out per-workspace, not flip the cluster-wide
   default. The script applies the admin-gated annotation directly to the
   Workspace CR for the runc leg.
3. `kubectl`, `jq`, `curl` on the machine running the script.
4. **kubectl configured with cluster-admin RBAC** — required to apply the
   admin-gated `llmsafespaces.dev/allow-runtime-class-override=true`
   annotation on the runc-leg workspaces. The REST API does not expose
   `spec.runtimeClass` (the opt-out is admin-gated by design — see
   `controller/internal/webhooks/workspace_webhook.go`).
5. **An API token** for the cold-prompt phase (sessions + messages go
   through the REST API, which requires auth). Set `API_TOKEN` to the
   benchmark user's `lsp_…` API key; the token's user owns the created
   workspaces.

## Interpreting the results

The accept/reject gate is a per-phase median overhead, computed against
the `runc` baseline. Median, not mean — a single slow PVC attach on a
noisy neighbor should not flip the decision. Compute the per-phase numbers
with `awk`/`datamash`/a spreadsheet:

```bash
# Per-phase median per runtime.
datamash -H --delimiter $'\t' median:4 \
  groupby 1,2 < gvisor-bench.tsv
```

Output (illustrative — your numbers will differ):

```
GroupBy(runtime)  GroupBy(phase)   median(seconds)
gvisor            cold_prompt      2.211
gvisor            file_io          0.184
gvisor            pod_boot         25.022
runc              cold_prompt      2.193
runc              file_io          0.141
runc              pod_boot         23.605
```

Per-phase overhead %:

```
overhead(phase) = 100 × (median_gvisor(phase) − median_runc(phase)) / median_runc(phase)
```

### Decision

| Outcome | Action |
|---|---|
| **pod_boot overhead < 30%** AND **cold_prompt overhead < 30%** AND **file_io overhead < 30%** | **Accept.** Flip `gvisor.enabled: true` in your site values. The default in the chart stays `false` so other operators make their own measurement; you have made yours. |
| **file_io overhead ≥ 30%** but **pod_boot + cold_prompt < 30%** | **Accept with caveat.** The realistic workload (boot + LLM round-trip) is under target. The file_io number tells you a purely I/O-bound workspace would see more overhead — surface this in operator docs so tenants with I/O-heavy workloads know to request the `runc` opt-out if they hit it. |
| **pod_boot OR cold_prompt overhead ≥ 30%** | **Reject for your deployment.** Do not flip the default. Investigate: a boot overhead that high suggests the workspace image's init path is hitting an unimplemented gVisor syscall (check the pod's `dmesg` or gVisor's `runsc` logs for `Unsupported` warnings). A cold_prompt overhead that high is unlikely unless your LLM API latency is already single-digit-millisecond (unrealistic); if you see it, suspect the proxy path, not gVisor. |

The 30% threshold comes from the Epic 51 design doc. It is a judgment call,
not a physical constant — gVisor's own published overhead is 5–30% on
syscall-bound workloads, near-zero on CPU-bound ones. The threshold picks
the top of that range so a workload that lands in the worst case for gVisor
still passes if the rest of the stack (LLM latency, PVC attach) absorbs
the cost. Lower the threshold for latency-sensitive deployments.

## What this benchmark does NOT cover

- **Side-channel resistance.** gVisor does not mitigate CPU/cache timing or
  Rowhammer. That is an accepted risk in the threat model — see Epic 51's
  "Out of scope" table. The benchmark measures performance, not isolation.
- **Memory overhead.** gVisor's sentinel process adds ~50–100 MiB per pod.
  The benchmark does not measure this because it is a fixed per-pod cost,
  not a workload-dependent overhead. Account for it in node sizing.
- **Network performance.** gVisor's network stack is userspace; throughput
  on net-bound workloads (large artifact uploads) can be 10–20% lower. The
  benchmark's cold_prompt phase exercises the network path implicitly
  (proxy → pod), but a dedicated netperf run is out of scope.

## After the measurement

If you accept, file the result as a worklog entry citing this doc and the
raw TSV. The chart's default flip (if you choose to upstream it) is a
separate PR — Epic 51 S51.1 stays "default off" in the chart until two
or more operators report independent measurements under 30%, so the
default reflects reality rather than one operator's hardware.
