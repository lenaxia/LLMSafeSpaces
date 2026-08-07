// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workflows

// Shared types for the controller workflows package. These are used by both
// the reconciler (US-64.8) and the scheduler (US-64.9). When both PRs merge
// to main, this file and the duplicated definitions in reconciler.go should
// be deduplicated.

type ReconcilerLogger interface {
	Info(msg string, keysAndValues ...any)
	Error(err error, msg string, keysAndValues ...any)
}

type noopLogger struct{}

func (noopLogger) Info(string, ...any)         {}
func (noopLogger) Error(error, string, ...any) {}

func strPtr(s string) *string { return &s }
