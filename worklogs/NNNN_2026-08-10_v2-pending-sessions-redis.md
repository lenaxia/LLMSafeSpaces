# Worklog: Redis-backed v2PendingSessions for Multi-Replica Support

**Date:** 2026-08-10
**Branch:** `fix/epic-63-v2-pending-sessions-redis`
**Status:** Complete

## Problem

Real-cluster validation (worklog NNNN_epic-63-v2-real-cluster-validation)
surfaced that the in-memory `v2PendingSessions` causes the second queued
message to strand on multi-replica deployments. Enqueue lands on replica A;
SSE reconnect/sweep runs on replica B (empty pending set) → no wake fires.

## Fix

Extracted `v2PendingTracker` interface with two implementations:
- `v2PendingSessions`: in-memory (tests, single-replica fallback)
- `v2PendingRedis`: Redis-backed (production, multi-replica shared)

`app.go` injects the Redis-backed tracker via `SetV2PendingTracker` when a
cache client is available.

### TOCTOU race fix

The initial implementation had a TOCTOU race: `HINCRBY -1` then `HDel` when
count reached zero. A concurrent `add` between those two operations would
be clobbered by `HDel`. Fix: **never HDel**. Readers filter count > 0; the
TTL sweeps the hash key. Negative counts are invisible and self-correct on
the next `add`.

### Nil interface fix

`NewV2PendingTracker(nil)` must return `nil` (not a non-nil interface
wrapping a nil pointer) so `SetV2PendingTracker`'s nil guard works.

## Tests (10 new)

- Track-and-clear, reference-counted multi-input, nil-safe, cross-workspace isolation
- TTL expiry, Redis-down graceful degradation
- Interface conformance, behavioral parity (in-memory vs Redis)
- **Handler integration**: enqueueV2 → tracker.add → Prompted → tracker.remove
- **Concurrent add/remove**: regression test for the TOCTOU race (100 adds + 99 removes = count 1)
