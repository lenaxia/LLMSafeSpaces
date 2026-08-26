
## Addendum 3 (2026-08-26): kind run 3 — rt/ parent + silent-degrade triage

K13 GREEN (degraded base boots Ready — the incident class closed at
L3), K1/K9/K12/K4/K5/K6/K7/K8/K2 green. Three fails, one root cause
for two of them: init-fs chmod'ed rt/{ssh,secrets} but left the rt/
PARENT at MkdirAll-default (0750+setgid under umask/fsGroup). The
sidecar's bootstrap writes rt/secrets.json directly IN rt/ — silently
(writeEmptySecrets ignored its error) — so the batch never persisted,
materialize degraded on the missing file, boot still succeeded (the
degrade chain is graceful end-to-end, which is why K13 passed), and
the K11 restart guard never held. Fixes: rt/ joins the exact-0770
managed set (contract test extended); writeEmptySecrets reports the
failure (never-block-boot preserved — the silence cost a triage cycle);
K10's recreation check switched from pod-name diff (deterministic
names — can never differ) to creationTimestamp.
