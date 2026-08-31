// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// rev_anchor.go — US-70.2 Part 2: the pod-side revision anchor and the
// W2 apply-guard.
//
// The anchor is a sibling of the materialized secrets-env file
// (<secrets-env>.rev) holding the delivery revision of the batch the
// current materialized state came from:
//
//	{"rev":"<seq>:<manifestHash>","appliedSeq":<seq>}
//
// Readers: /v1/spawn-env composes "<seq>:<manifestHash>:<contentHash>"
// from rev; the materialize path compares appliedSeq against an
// incoming PULLED batch's seq (≤ applied ⇒ skip: idempotent-equal is a
// no-op, lower is stale — the out-of-order/stale-replica race W2).
// Legacy/push batches (no revision) write no anchor and REMOVE a stale
// one (marker back to unknown). The file lives on the same tmpfs as the
// state it describes: pod death wipes both together, so an anchor can
// never describe state that is absent.

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	sec "github.com/lenaxia/llmsafespaces/pkg/secrets"
)

// revAnchorSuffix extends the secrets-env path into its anchor sibling.
const revAnchorSuffix = ".rev"

// revAnchor is the on-disk anchor shape.
type revAnchor struct {
	// Rev is "<seq>:<manifestHash>" — the delivery revision of the
	// applied batch.
	Rev string `json:"rev"`
	// AppliedSeq is the seq recorded as APPLIED (the guard's high-water
	// mark). 0 means unknown.
	AppliedSeq int64 `json:"appliedSeq,omitempty"`
}

func revAnchorPath(secretsEnvPath string) string {
	return secretsEnvPath + revAnchorSuffix
}

// readRevAnchor parses the anchor. Absent is (zero, err) — callers
// treat that as "unknown" and proceed ungated.
func readRevAnchor(path string) (revAnchor, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return revAnchor{}, err
	}
	var a revAnchor
	if err := json.Unmarshal(data, &a); err != nil {
		return revAnchor{}, fmt.Errorf("parse rev anchor %s: %w", path, err)
	}
	return a, nil
}

func writeRevAnchor(path string, a revAnchor) error {
	data, err := json.Marshal(a)
	if err != nil {
		return err
	}
	return atomicWriteAny(path, data, 0o600)
}

func removeRevAnchor(path string) {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		// Loud-but-non-fatal: a stuck anchor fails the guard OPEN (the
		// next revisioned pull skips as "already applied") — surface it.
		_, _ = fmt.Fprintf(os.Stderr, "materialize: removing stale rev anchor %s failed: %v\n", path, err)
	}
}

// anchoredSpawnRev composes the served/supervisor rev: when anchor (a
// "<seq>:<manifestHash>" string) is present the rev gains it as a
// prefix; the content hash stays terminal (design 0057 I4 — it is
// always computed over what is actually present, never taken from the
// anchor).
func anchoredSpawnRev(anchor, contentHash string) string {
	if anchor == "" {
		return contentHash
	}
	return anchor + ":" + contentHash
}

// anchoredPrefix splits a served rev into its anchored prefix. A bare
// content hash (no anchor) yields "".
func anchoredPrefix(servedRev string) string {
	parts := strings.Split(servedRev, ":")
	if len(parts) != 3 {
		return ""
	}
	return parts[0] + ":" + parts[1]
}

// anchorFromSeqHash renders the anchor string from its parts (inverse
// of the split, kept next to it for symmetry).
func anchorFromSeqHash(seq int64, manifestHash string) string {
	return strconv.FormatInt(seq, 10) + ":" + manifestHash
}

// setRevAnchorAfterApply maintains the anchor after a completed apply:
// a revisioned batch records its seq (arming the W2 apply-guard and the
// spawn-seam rev anchoring); a legacy effective batch removes any stale
// anchor — its live state is not a revisioned pull, and a left-behind
// marker would both wrongly gate later pulls and wrongly anchor the
// served revs.
func setRevAnchorAfterApply(anchorPath string, rev *sec.BatchRevision, stderr io.Writer) {
	if rev == nil {
		removeRevAnchor(anchorPath)
		return
	}
	anchor := revAnchor{Rev: anchorFromSeqHash(rev.Seq, rev.ManifestHash), AppliedSeq: rev.Seq}
	if err := writeRevAnchor(anchorPath, anchor); err != nil {
		_, _ = fmt.Fprintf(stderr, "materialize: failed to write rev anchor %s: %v\n", anchorPath, err)
	}
}
