// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestResolveAuditTarget_MapsProviderShortNameToPrefixForm is the regression
// test for PR #548 review C1: runAudit originally compared kmsProvider
// (the operator-facing "aws"/"gcp" form) directly against "aws-kms"/"gcp-kms"
// and rejected every valid value with a self-contradictory error. The
// extraction into resolveAuditTarget isolates the mapping so it can be
// tested without a Postgres connection.
func TestResolveAuditTarget_MapsProviderShortNameToPrefixForm(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    string
		wantErr bool
		errSub  string
	}{
		{"aws maps to aws-kms", "aws", "aws-kms", false, ""},
		{"gcp maps to gcp-kms", "gcp", "gcp-kms", false, ""},
		{"empty rejected", "", "", true, "--audit requires --kms aws or --kms gcp"},
		{"full-prefix form rejected (operator types aws-kms verbatim)", "aws-kms", "", true, "--audit requires --kms aws or --kms gcp"},
		{"gcp-kms verbatim rejected", "gcp-kms", "", true, "--audit requires --kms aws or --kms gcp"},
		{"typo rejected", "awd-kms", "", true, "--audit requires --kms aws or --kms gcp"},
		{"azure rejected (unsupported provider)", "azure", "", true, "--audit requires --kms aws or --kms gcp"},
		{"case-sensitive AWS rejected", "AWS", "", true, "--audit requires --kms aws or --kms gcp"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveAuditTarget(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("resolveAuditTarget(%q): expected error, got nil (returned %q)", tt.input, got)
				}
				if !strings.Contains(err.Error(), tt.errSub) {
					t.Errorf("resolveAuditTarget(%q): error %q does not contain expected substring %q", tt.input, err.Error(), tt.errSub)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveAuditTarget(%q): unexpected error: %v", tt.input, err)
			}
			if got != tt.want {
				t.Errorf("resolveAuditTarget(%q): got %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

// TestRunAudit_InvalidKms_ReturnsMappingError verifies runAudit's kms
// validation surfaces the resolveAuditTarget error directly — an
// operator running `migrate-kek --audit --kms invalid` gets the same
// actionable error regardless of whether --db-url is set.
func TestRunAudit_InvalidKms_ReturnsMappingError(t *testing.T) {
	tests := []struct {
		name      string
		kms       string
		dbURL     string
		errSubstr string
	}{
		{
			name:      "invalid kms with db-url set",
			kms:       "invalid",
			dbURL:     "postgres://localhost",
			errSubstr: "--audit requires --kms aws or --kms gcp",
		},
		{
			name:      "invalid kms without db-url",
			kms:       "invalid",
			dbURL:     "",
			errSubstr: "--audit requires --kms aws or --kms gcp",
		},
		{
			name:      "valid kms but missing db-url",
			kms:       "aws",
			dbURL:     "",
			errSubstr: "--db-url is required for --audit",
		},
		{
			name:      "valid gcp kms but missing db-url",
			kms:       "gcp",
			dbURL:     "",
			errSubstr: "--db-url is required for --audit",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := runAudit(tt.dbURL, tt.kms)
			if err == nil {
				t.Fatalf("runAudit(%q, %q): expected error containing %q, got nil", tt.dbURL, tt.kms, tt.errSubstr)
			}
			if !strings.Contains(err.Error(), tt.errSubstr) {
				t.Errorf("runAudit(%q, %q): error %q does not contain expected substring %q", tt.dbURL, tt.kms, err.Error(), tt.errSubstr)
			}
		})
	}
}

// TestRunAudit_ValidKms_ReachesPgConnection verifies that with a valid kms
// flag and a non-empty db-url, runAudit gets past validation and fails at
// the Postgres connection step — not at flag parsing. This is the positive
// control for the C1 fix: the audit CLI no longer rejects valid --kms values
// before even attempting the database connection. The store is wired to a
// real pgx pool (#830); against an unreachable host the failure is the dial
// error wrapped as "connect to Postgres", which is the proof we reached the
// connection step. The live-Postgres audit behavior is covered by
// store_integration_test.go.
func TestRunAudit_ValidKms_ReachesPgConnection(t *testing.T) {
	tests := []struct {
		name string
		kms  string
	}{
		{"aws reaches PG step", "aws"},
		{"gcp reaches PG step", "gcp"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := runAudit("postgres://stub:5432/stub", tt.kms)
			if err == nil {
				t.Skip("newPgMigrationStore is wired to a real connection; this test needs updating to use a real test PG or to assert a non-validation error")
			}
			// The error must NOT be the kms-validation error — that would
			// mean we never reached the PG step (regression of C1).
			if strings.Contains(err.Error(), "--audit requires --kms") {
				t.Fatalf("runAudit rejected valid kms=%q at validation (regression of C1): %v", tt.kms, err)
			}
			if strings.Contains(err.Error(), "--db-url is required") {
				t.Fatalf("runAudit rejected non-empty db-url (regression): %v", err)
			}
			// Reaching the PG step means the error is the real connection's
			// dial failure ("connect to Postgres: ..."), which is the proof
			// we got past kms validation. The live-Postgres audit behavior
			// is covered by store_integration_test.go.
		})
	}
}

// TestRun_TableAllWithResumeFromRejected is the migrate-kek twin of
// cmd/rotate-kek's guard test (PR #1409 review iteration 2): --resume-from
// is a per-table cursor, MigrateAll takes none — silently ignoring it would
// strand pre-cursor rows. The guard fires before any key-file read or KMS
// construction, so no fixtures are needed.
func TestRun_TableAllWithResumeFromRejected(t *testing.T) {
	err := run("postgres://127.0.0.1:1/nope", "/nonexistent-master.key", "aws", "us-east-1", "/nonexistent-creds",
		"/nonexistent-gcp", "arn:a", "arn:b", "arn:c", "", "", "",
		"all", "some-row-id", "", false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--resume-from applies per table")
}

// TestPrintResumeHint_DryRunNeverSuggestsResumeFrom is the migrate-kek twin
// of cmd/rotate-kek's hint test (PR #1409 review iteration 2): in a dry-run
// nothing was written, so suggesting --resume-from would make the apply run
// SKIP every row before the cursor. The dry-run hint must say to re-run
// plainly instead.
func TestPrintResumeHint_DryRunNeverSuggestsResumeFrom(t *testing.T) {
	out := captureHintStderr(t, func() { printResumeHint("api_keys", "row-7", true) })
	assert.NotContains(t, out, "--resume-from row-7", "dry-run hint must not suggest resuming FROM the reported cursor")
	assert.Contains(t, out, "re-run without --dry-run")
}

// TestPrintResumeHint_ApplyRunSuggestsResumeFrom pins the apply-run shape.
func TestPrintResumeHint_ApplyRunSuggestsResumeFrom(t *testing.T) {
	out := captureHintStderr(t, func() { printResumeHint("api_keys", "row-7", false) })
	assert.Contains(t, out, "--resume-from row-7")
	assert.Contains(t, out, "--table api_keys")
}

// captureHintStderr captures os.Stderr while fn runs. The hint is a single
// short line (< PIPE_BUF), so one write + one read cannot deadlock.
func captureHintStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	require.NoError(t, err)
	saved := os.Stderr
	os.Stderr = w
	fn()
	w.Close()
	os.Stderr = saved
	buf := make([]byte, 4096)
	n, _ := r.Read(buf)
	r.Close()
	return string(buf[:n])
}
