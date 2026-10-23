# Worklog: secrets-integration dispatch-index re-registration (#1844 instance 6)

## Scope
A fresh registration commit on `.github/workflows/secrets-integration.yml`
(PR #1894, follow-up to #1893, kept separate per the lane ruling):
the workflow_dispatch trigger itself landed in #1874; instance 6
showed a dispatch registered before that landing serving a STALE
workflow index for the #1874-touched files. This commit re-registers
the file's dispatch index and extends the lever's comment with the
stale-index symptom (`gh workflow run secrets-integration.yml --ref
<branch>` answering "does not have a workflow_dispatch trigger" right
after a trigger lands) and the re-touch discipline — the #1874 marker
pattern, so the next operator recognizes it in one read.

No job/step/gate semantics touched; comment + re-registration only.

## Evidence
- The lever proven live four times this session on
  fix/1832-cascade-integration (runs 39936615088, 39940488589,
  39942404652) — the iteration vehicle that surfaced and closed the
  six never-executed #1832 cascade pins (#1893, merged 0e3ac756).
- #1874's original live verification (2026-10-23 branch suppression).
- Structural-green baseline: main's first fully-green
  secrets-integration run of the window, 39947285622 on fcbf3655
  (post-#1893) — post-#1893 reds in this suite are signal again.

## Gates
Comment-only diff; orchestrator read PASS (marker-literal confirmed
in-file on main); docs-only class per the #1876 precedent.
