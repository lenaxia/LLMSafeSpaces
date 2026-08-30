// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package sessionstate

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// seqCursor is the durable seq high-water mark on the platform/ PVC subPath
// (design 0055 M2/I9-prep). Policy — fsync-before-publish: every persist()
// writes the new high-water mark and fsyncs the file BEFORE the caller
// publishes the seq to subscribers, so a published seq is never reused
// after agentd death (kill -9 included). Cost: one pwrite+fsync of an
// 8-byte file per seq advance under the authority lock. Group-commit is
// the documented optimization if the send-path p99 budget ever requires it
// (same allowance as the ledger's I9 wording); correctness first.
//
// Format: plain decimal ASCII, single line, written via temp+rename into
// the platform dir. A missing file means seq 0 (first enable). A corrupt
// file is a hard error at open: guessing would risk seq reuse (I1).
type seqCursor struct {
	mu      sync.Mutex
	dir     string
	path    string
	lastSeq uint64
	// fast disables fsync (in-memory-ack durability). For scenario
	// harnesses that replay event BURSTS: fsync-per-event matches
	// production's human/LLM-paced event rate, not burst replay; the
	// durability property itself is covered by the fault-injection suite
	// (TestSeqMonotonicAcrossKill9 etc.) with fsync on.
	fast bool
}

func openSeqCursor(dir string, fast bool) (*seqCursor, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("create platform dir: %w", err)
	}
	c := &seqCursor{dir: dir, path: filepath.Join(dir, "seq-cursor"), fast: fast}
	data, err := os.ReadFile(c.path)
	if os.IsNotExist(err) {
		return c, nil
	}
	if err != nil {
		return nil, err
	}
	var n uint64
	if _, err := fmt.Sscanf(string(data), "%d\n", &n); err != nil {
		// Tolerate a missing trailing newline.
		if _, err2 := fmt.Sscanf(string(data), "%d", &n); err2 != nil {
			return nil, fmt.Errorf("corrupt seq cursor %q: %q", c.path, data)
		}
	}
	c.lastSeq = n
	return c, nil
}

func (c *seqCursor) last() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastSeq
}

// persist durably advances the high-water mark to seq (monotonic only) and
// fsyncs before returning.
func (c *seqCursor) persist(seq uint64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if seq <= c.lastSeq {
		return nil // already durable (reseed flush idempotence)
	}
	tmp := c.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(f, "%d\n", seq); err != nil {
		_ = f.Close()
		return err
	}
	if !c.fast {
		if err := f.Sync(); err != nil {
			_ = f.Close()
			return err
		}
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, c.path); err != nil {
		return err
	}
	if c.fast {
		return nil
	}
	// Directory fsync: make the rename itself durable.
	d, err := os.Open(c.dir)
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	if err := d.Sync(); err != nil {
		return err
	}
	c.lastSeq = seq
	return nil
}

func (c *seqCursor) close() error { return nil }
