// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package shadowconsumer

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

// Recorder writes the scenario artifacts (issue #1139: "recorded streams +
// tracker state"): the raw dialect stream the harness fed, and every
// divergence observed. One directory per run.
type Recorder struct {
	mu       sync.Mutex
	dialect  *bufio.Writer
	dialectF *os.File
	divF     *bufio.Writer
	divFile  *os.File
}

func NewRecorder(dir string) (*Recorder, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, err
	}
	df, err := os.Create(filepath.Join(dir, "dialect_stream.ndjson"))
	if err != nil {
		return nil, err
	}
	vf, err := os.Create(filepath.Join(dir, "divergences.ndjson"))
	if err != nil {
		_ = df.Close()
		return nil, err
	}
	return &Recorder{
		dialect:  bufio.NewWriter(df),
		dialectF: df,
		divF:     bufio.NewWriter(vf),
		divFile:  vf,
	}, nil
}

func (r *Recorder) RecordDialect(raw []byte) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	_, _ = r.dialect.Write(append(raw, '\n'))
}

func (r *Recorder) RecordDivergence(d Divergence) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	b, err := json.Marshal(d)
	if err != nil {
		return
	}
	_, _ = r.divF.Write(append(b, '\n'))
}

func (r *Recorder) Close() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	_ = r.dialect.Flush()
	_ = r.divF.Flush()
	e1 := r.dialectF.Close()
	e2 := r.divFile.Close()
	if e1 != nil {
		return e1
	}
	return e2
}
