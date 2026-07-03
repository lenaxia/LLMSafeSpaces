// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package auth

import (
	"bytes"
	"testing"
)

// TestEachSigningKey_IteratesAllInOrder proves the enumerator delivers
// primary first, then previous keys in insertion order. Order matters
// because secrets.KeyService.GetDEKForUser iterates and stops on the
// first successful unwrap — putting the ACTIVE (most-recent) key
// first minimizes wasted decrypt attempts in the vast-majority case
// where the row was wrapped under the current active key.
func TestEachSigningKey_IteratesAllInOrder(t *testing.T) {
	primary := []byte("primary-signing-key-XXXXXXXXXXXXXXXX")
	prev1 := []byte("previous-key-1-XXXXXXXXXXXXXXXXXXXX")
	prev2 := []byte("previous-key-2-XXXXXXXXXXXXXXXXXXXX")

	svc := &Service{
		jwtSecret:          primary,
		jwtPreviousSecrets: [][]byte{prev1, prev2},
	}

	var seen [][]byte
	svc.EachSigningKey(func(key []byte) bool {
		cp := make([]byte, len(key))
		copy(cp, key)
		seen = append(seen, cp)
		return true
	})

	if len(seen) != 3 {
		t.Fatalf("expected 3 keys, got %d", len(seen))
	}
	if !bytes.Equal(seen[0], primary) {
		t.Errorf("first key must be primary; got %q", seen[0])
	}
	if !bytes.Equal(seen[1], prev1) {
		t.Errorf("second key must be prev1; got %q", seen[1])
	}
	if !bytes.Equal(seen[2], prev2) {
		t.Errorf("third key must be prev2; got %q", seen[2])
	}
}

// TestEachSigningKey_StopsWhenCallbackReturnsFalse proves the early-
// exit contract: KeyService uses this to stop after the first
// successful unwrap. Without early exit, we'd copy and pass every
// key to the callback even after a success — wasted work and a
// wider retention window for keys the callee doesn't need.
func TestEachSigningKey_StopsWhenCallbackReturnsFalse(t *testing.T) {
	svc := &Service{
		jwtSecret:          []byte("primary"),
		jwtPreviousSecrets: [][]byte{[]byte("prev1"), []byte("prev2")},
	}
	var count int
	svc.EachSigningKey(func(_ []byte) bool {
		count++
		return false // stop after first
	})
	if count != 1 {
		t.Errorf("expected exactly 1 callback (stop on first-false); got %d", count)
	}
}

// TestEachSigningKey_NoPreviousKeysStillYieldsPrimary covers the fresh-
// install case with no rotation history.
func TestEachSigningKey_NoPreviousKeysStillYieldsPrimary(t *testing.T) {
	svc := &Service{
		jwtSecret: []byte("only-key"),
	}
	var count int
	svc.EachSigningKey(func(_ []byte) bool {
		count++
		return true
	})
	if count != 1 {
		t.Errorf("expected 1 (primary only); got %d", count)
	}
}

// TestEachSigningKey_CopiesKeysDoesNotAliasInternalState proves the
// callback receives independent bytes each iteration — mutating a
// received slice must NOT affect the Service's state, nor subsequent
// callback deliveries.
//
// Why this matters: secrets.KeyService.tryUnwrapRowWithKnownKeys
// mutates the derived KEK-material buffer (appends jti, zeroes on
// return). If EachSigningKey passed the same backing slice each
// call, the second iteration's key would see the residue from the
// first append. That would produce a different-looking key than the
// signingKeyByIndex accessor returns via direct index call, and
// unwrap would fail in a way that's hard to debug.
func TestEachSigningKey_CopiesKeysDoesNotAliasInternalState(t *testing.T) {
	primary := []byte{0x01, 0x02, 0x03, 0x04}
	prev := []byte{0x0a, 0x0b, 0x0c, 0x0d}
	svc := &Service{
		jwtSecret:          primary,
		jwtPreviousSecrets: [][]byte{prev},
	}

	var received [][]byte
	svc.EachSigningKey(func(key []byte) bool {
		// Mutate the received slice in-place.
		key[0] = 0xff
		received = append(received, key)
		return true
	})

	// Service's own state must be unchanged.
	if primary[0] != 0x01 {
		t.Errorf("primary mutated by callback: got %#v", primary)
	}
	if prev[0] != 0x0a {
		t.Errorf("prev mutated by callback: got %#v", prev)
	}
	// Fresh-copy-per-iteration contract: received[0] and received[1]
	// MUST be independent memory. If the enumerator reused one backing
	// buffer, mutating `key` in iteration 2 (0xff) would also mutate
	// what iteration 1 handed us. We detect that by asserting
	// received[0]'s remaining bytes still reflect primary's original
	// values ([0x02, 0x03, 0x04]) rather than prev's second-through-
	// last bytes ([0x0b, 0x0c, 0x0d]). A shared-buffer implementation
	// that copies prev over primary on iteration 2 would fail this
	// assertion.
	if !bytes.Equal(received[0][1:], []byte{0x02, 0x03, 0x04}) {
		t.Errorf("received[0] tail was overwritten by later iteration: got %#v (shared-buffer aliasing)", received[0])
	}
	if !bytes.Equal(received[1][1:], []byte{0x0b, 0x0c, 0x0d}) {
		t.Errorf("received[1] tail unexpected: got %#v", received[1])
	}
}
