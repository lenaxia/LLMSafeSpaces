// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"strings"
	"testing"
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
// the (pre-existing stub) Postgres connection step — not at flag parsing.
// This is the positive control for the C1 fix: the audit CLI no longer
// rejects valid --kms values before even attempting the database connection.
//
// Note: when newPgMigrationStore is wired to a real connection (currently
// a stub), this test's assertion will need to change from "postgres connection"
// to whatever the real connection error looks like, or set up a real PG
// container. The point of this test today is to prove runAudit gets past
// kms validation; the PG stub's error string is the proxy for that.
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
			// Reaching the PG step means the error is whatever the stub
			// (or real connection) produces. The current stub returns
			// "postgres connection not yet wired"; a real connection would
			// return a dial error. Either is acceptable proof that we got
			// past kms validation.
		})
	}
}
