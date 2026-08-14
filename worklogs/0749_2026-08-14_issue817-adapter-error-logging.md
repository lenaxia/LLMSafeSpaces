# Worklog: #817 — SendMessage adapter error logging

**Date:** 2026-08-14
**Issue:** #817
**PR:** #851

---

## Context

Production v0.15.4: two 502s ("failed to send message") after exactly
2m5.01s where the LLM turn completed server-side but the response never
returned. The adapter error was swallowed — SendMessage (and
SendPromptAsync, DeleteSession) returned bare 502 bodies with no log,
unlike CreateSession/ListSessions/GetHistory which all log
"adapter failed".

## Investigation (live, prod)

Ruled out by direct measurement (see #817 comment for the full table):
- opencode sync endpoint healthy (200 in 1.7s in-pod, 1.15s pod-to-pod
  via the API's exact network path/auth/policy)
- No 125s timeout anywhere: adapter client (none), gin middleware,
  Traefik (respondingTimeouts=0), app http.Server (only
  ReadHeaderTimeout=10s), request buffer (30s)
- API healthy: 69 goroutines, no leak

Remaining hypothesis: an idle-connection middlebox deadline (Cilium
conntrack) or agentd-side artifact — needs the error string from this
logging to confirm.

## Changes

1. SendMessage: log "SendMessage: adapter failed" with the underlying
   error (both the /message adapter path)
2. SendPromptAsync (/prompt — the exact endpoint that failed in prod):
   log "SendPromptAsync: adapter failed"
3. DeleteSession: log "DeleteSession: adapter failed" (parity)
4. Test env: newTestEnvWithBackendAndLogger allows injecting an
   observing logger; testEnv.log widened to LoggerInterface
5. Regression tests: all three paths assert the error log fires
   (logger.NewObserved)

## Collisions

Multiple working-tree collisions with the other agent during this
session (shared checkout): my commits ended up on their branch twice,
their commit landed on mine once, untracked test files were deleted
and stashed. Recovered each time via remote refs and stash commits.
