
## Addendum 2 (2026-08-26): kind run 2 — init-fs mode contract

Second K1–K13 execution (v0.21.1, 08:42 UTC): platform inits green
(volume fix), sidecar boot phase now fails correctly-fatal on
`materialize: /sandbox-runtime/rt/secrets.json: permission denied` —
init-fs created rt dirs 0700 (the pre-US-4b contract) instead of 0770
(the US-4b cross-uid contract the uid-2000 sidecar requires via shared
gid 1000). Note the failure chain worked as designed: bootstrap
degraded (API off, logged, exit 0), materialize hit the mode bug, the
boot phase propagated exit 2, CrashLoopBackOff surfaced with the
reason in the message.

Fix: rt dirs 0770 with EXACT-mode semantics (chmod after MkdirAll —
MkdirAll applies the process umask, and 0770 & ~022 = 0750 loses the
group-write bit). Pinned by the init-fs corpus. Shipped as v0.21.2.

Prod note: legacy overlay mode on a stale factory base still
crash-loops in the MAIN container (pre-#863 baked entrypoint runs the
baked pre-fix agentd for boot materialize) — only the sidecar flip
removes that dependency (K13's claim). The platform-init half of the
incident is fixed for all modes.
