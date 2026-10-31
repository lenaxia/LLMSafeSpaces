# Worklog — design 0070 §2.4a: the payload exchange dir (sidecar auto-delete fix)

Lane: tsk-jasper-vignette-hovers (epic "Sprint Board: naming, onboarding,
and surface polish"; project "Agent DX: reference patterns"). Branch
`fix/0070-exchange-dir` off `main` (a345b8101), isolated worktree
`/tmp/opencode/exchange-dir-wt` — the shared /workspace/LLMSafeSpaces
tree carried the machinery-rename lane's uncommitted work; nothing
there was touched.

## 1. The charter

Owner report 2026-10-30: every board file-payload call (design 0070
cutover) returns `cleanup_warning: … read-only file system`. The
charter carried the full root cause (investigation complete) and the
chosen fix direction: a new dual-rw exchange dir on the workspace PVC.
Three alternatives pre-rejected (do not re-derive): flipping the
sidecar workspace mount to RW (0055 M2 regression), payloads under
/platform (runtime cannot write it), runtime-side self-cleanup (the
broken status quo).

## 2. Root cause (verified against main, not just re-read from the charter)

- The delete: `cmd/workspace-agentd/board_manager_tools.go` —
  `os.Remove(consumedPath)` after a successful wire call, in the
  SIDECAR agentd process.
- The mount: `controller/internal/workspace/agentd_sidecar.go` —
  `{workspace, /workspace, SubPath workspace, ReadOnly: true}` by
  design; reads succeed, unlink gets EROFS. `/platform` is RW in the
  sidecar but uid-2000-exclusive (never in uid-1000 space, 0055 M2).
- Path anchoring could not escape: relative paths joined
  `boardFileWorkspaceRoot = /workspace` — every reachable payload path
  lived under the RO mount.
- Why tests passed: single-container mode mounts /workspace RW in the
  ONE container that holds both roles (`pod_builder.go`); no test
  exercised the payload-delete path under a sidecar pod spec.

A prior sibling lane (tsk-slick-carousel-hovers, branch
fix/0070-payload-cleanup-erofs, in_review since 2026-10-27, never
merged) fixed the same symptom by escalating the delete to the
supervisor over a new `file_delete` control-socket method. This
charter supersedes it with the exchange-dir direction; that branch is
untouched and its PR decision remains the orchestrator's.

## 3. The implementation

- `pkg/agentd/types.go`: `ExchangePath = "/exchange"`.
- Mounts: main container (BOTH modes) + agentd sidecar both mount
  `{workspace PVC, SubPath exchange}` RW. The sidecar's workspace
  mount stays ReadOnly (pinned, plus a guard test asserting the
  exchange subPath is the sidecar's ONLY RW PVC subPath).
- Creation: `init-fs --exchange-subpath=create|skip|only` mirroring
  `--platform-subpath` (sidecar: platform-init skips, the uid-2000
  platform-dirs init creates it; single-container: platform-init
  creates it). `ensureExchangeDir`: Mkdir 0770 + explicit Chmod
  (umask would strip group-write); tolerant on existing dirs whose
  group bits are right (NO chmod — the other uid's init owns a
  mode-flipped PVC's dir; chmod would EPERM/crash-loop); repairs
  wrong modes; refuses planted non-directories loudly.
- Mode decision (0770 shared group, NOT sticky): unlink permission
  rides the PARENT dir's group-write bit — sticky would block the
  sidecar unlinking the runtime's files, the exact operation the dir
  exists for. Shared gid 1000 = pod RunAsGroup/FSGroup (sidecar runs
  RunAsGroup 1000; runtime carries gid 1000). US-4b rt/ precedent.
- agentd anchoring: relative board file params resolve against
  `/exchange` (`boardFileExchangeRoot` var); absolute paths pass
  through. Compat arm (decision: read+warn, one release): absolute
  /workspace paths stay readable; a failed delete there gets a
  POINTED warning (legacy location, why it can never auto-delete,
  where payload files belong); the not-found hint names /exchange.
- MCP descriptions (the six payload-bearing tools + board_validate_file)
  and board_payload_schema's Fix text now say payloads live under
  /exchange; no doc still promises deletion of files it cannot delete
  (design 0070 amended with §3a: root cause, mode decision + threat
  model, compat decision, version-skew note).

## 4. Red-first + mutation evidence

- Born-RED (criterion 3): `TestExchangeSubPath_SidecarTopology` and
  `_SingleContainer` run against the pre-fix controller (main +
  only the additive constant) fail with "the runtime container must
  mount /exchange (the payload exchange dir)" — the original gap, not
  just the new feature.
- Mutations, each verified RED then restored (staged-index restore):
  M1 sidecar exchange mount ReadOnly → SidecarTopology RED;
  M2 ensureExchangeDir unconditional chmod → ctime tolerance pin RED;
  M3 boardPathIsLegacyWorkspace → false → pointed-warning pin RED;
  M4 anchoring reverted to the workspace root → exchange anchoring +
  no-warning pins RED.

## 5. Gates

go build ./… ; full `cmd/workspace-agentd` suite (372s, includes the
subprocess init-fs pins) ; full `controller/internal/workspace`
package (68s) ; gofmt clean. (Remaining gates in CI.)

## 6. AI review round 1 (CHANGES_REQUESTED) — remediated in full

- **F1 guard inverted**: the sidecar-mount guard's default branch
  asserted the INVERSE (new RW subPaths passed silently). Rewritten as
  an explicit allowlist {workspace: RO, exchange: RW, platform: RW} —
  anything new fails; mutation M5 (a planted `future-rw` subPath) now
  goes RED with the audit message.
- **F2 nested paths**: the original report's own layout
  (reports/charters/…) silently broke the dual-uid contract (write
  tool creates subdirs 0755, unlink needs the parent's group-write →
  EACCES). Now REFUSED at read time (`refuseNestedExchangePath`, the
  choke point the resolver and board_validate_file share) with the
  flat-contract message; all MCP descriptions + the design §3a state
  the flat contract. Pins: nested relative + nested absolute +
  validate-file finding + flat happy shape (M6 RED-verified).
- **F3 version-skew note false**: rewritten truthfully — old agentd
  image + new controller dies at init-fs flag parse (exit 2,
  Init:Error) before any container start; a root-owned 0755 dir
  cannot be repaired by a non-root init (EPERM, loud) — recovery is
  operator repair.
- **F4 mode-blind warning**: `agentdRunsAsSidecar` (set at
  runSidecarCommand entry) selects the rationale — the sidecar form
  asserts never-deletable read-only truth; the single-container form
  calls the failure transient (no false read-only claim). Both
  topology arms pinned.
- **Missing-test 4 + robustness**: tolerant path now requires EXACTLY
  0770-and-no-sticky (planted 0777/1777 repaired; M7b RED-verified
  with the pre-r1 condition); Lstat (planted symlink refused);
  --exchange-subpath value validation (exit 2 on unknown, no silent
  skip).
- **Rule-7 false comment + E2E gap**: the "validated at the kind
  layer" claim is now TRUE — `local/exchange-dir-e2e.sh` (nightly,
  sidecar topology) EXECUTES the real dual-uid mechanism: the runtime
  container (uid 1000) writes a payload directly under /exchange, a
  real board_create_task through the SIDECAR's user mux (port-forward
  :4097) consumes it, the response carries NO cleanup_warning, and
  the file is GONE from the runtime's own view; X4 executes the
  nested-refusal; X2 pins mode 0770. Structure-pinned by
  local/exchange_dir_script_test.go (syntax + assertion rows + the
  4097 mux wiring).
- Probe-oracle note added to §3a's threat model (pre-existing,
  closes with the compat arm).

## 6b. AI review round 2 (CHANGES_REQUESTED) — remediated

- **F1 (seed crash)**: the WS_BASE prefix carried a literal 'x' —
  ws_id seeds it into workspaces.id (uuid column) and ON_ERROR_STOP
  killed the script at X1 before any row. Fixed (hex-only
  e2e070e0-…) AND pinned: TestExchangeDirScript_WorkspaceBaseIsAValid
  UUIDPrefix parses the prefix and uuid.Parse()s the derived id —
  verified RED against the broken literal.
- **F2 (wired into nothing + false execution claims)**: the script is
  now wired into e2e-nightly.yml as its own step, sited BEFORE the
  secret-delivery rows whose pre-existing six-day failure
  (2026-10-25..30, infra) aborts the job — the step comment records
  the placement rationale. Every committed "executed at the kind
  layer" claim reworded to the precise truth: the mechanism EXECUTES
  in the nightly's design-0070 §2.4a rows (a wiring statement); the
  green-run LINK is PR-comment evidence, not committed prose.
- **F3 (flat-symlink edge)**: documented in §3a's threat model — a
  flat symlink at the exchange root lets a caller consume a file
  outside the tree (the read follows the link; the cleanup unlinks
  the LINK only) — same class as the one-release read oracle, same
  closure (the compat arm's removal).
- First green execution linked in the PR (workflow_dispatch of the
  nightly on this branch — my step runs before the pre-existing
  failure point).

## 7. Gates (post-r1)

go build ./… ; full `cmd/workspace-agentd` suite ; full
`controller/internal/workspace` (unit + envtest) ; `local/` script-test
suite ; gofmt clean ; golangci-lint 0 issues. Mutation ledger (all
RED-verified then restored): M1 sidecar exchange mount RO; M2
unconditional chmod (ctime pin); M3 legacy predicate neutered; M4
pre-§2.4a anchoring; M5 planted future-rw subPath (guard allowlist);
M6 flat gate neutered; M7b pre-r1 tolerant mode check (sticky/world
repair pin).

## 8. Out of scope (kept)

Payload-param renames (tsk-wayward-whistle-exhales owns them); any
single-container behavior change (its delete always worked; only the
uniform /exchange mount + anchoring were added); the sidecar's read
surface (unchanged).
