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

## 6. Out of scope (kept)

Payload-param renames (tsk-wayward-whistle-exhales owns them); any
single-container behavior change (its delete always worked; only the
uniform /exchange mount + anchoring were added); the sidecar's read
surface (unchanged).
