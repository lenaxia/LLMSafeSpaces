// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

// Canary scenario: S-SECRET-AUDIT
// Tests the secret audit log endpoint.
package main

import (
	"context"
	"net/http"
	"os"
	"time"

	canary "github.com/lenaxia/llmsafespaces/sdks/canary/go"
)

func Handler(w http.ResponseWriter, r *http.Request) {
	run := canary.NewRunner("secret-audit", "go-sdk")
	cfg := canary.ConfigFromEnv()
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	runSecretAudit(ctx, run, cfg)
	run.WriteHTTP(w)
}

func main() {
	run := canary.NewRunner("secret-audit", "go-sdk")
	cfg := canary.ConfigFromEnv()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	runSecretAudit(ctx, run, cfg)
	res := run.Print()
	if res.Failed > 0 {
		os.Exit(1)
	}
}

func runSecretAudit(ctx context.Context, run *canary.Runner, cfg canary.Config) {
	c := cfg.Client()

	// Create a secret to generate an audit entry
	s, err := c.Secrets.CreateWithMetadata(ctx, "canary-audit-probe", "env-secret", "auditval", map[string]string{"var_name":"CANARY_VAR"})
	if run.AssertNoError(err, "create-for-audit: no error") {
		defer func() { _ = c.Secrets.Delete(context.Background(), s.ID) }()
	}

	// P1: Audit log accessible and returns entries array
	entries, err := c.Secrets.GetAuditLog(ctx)
	if run.AssertNoError(err, "get-audit: no error") {
		run.Assert(entries != nil, "get-audit: entries non-nil", "")

		// P2: After creating, log contains an entry for our secret
		if s != nil {
			found := false
			for _, e := range entries {
				if e.SecretID == s.ID {
					found = true
					// P3: Entry has required fields
					run.Assert(e.Action != "", "audit-entry: action field", "")
					run.Assert(e.UserID != "", "audit-entry: userId field", "")
					break
				}
			}
			run.Assert(found, "get-audit: entry for created secret present", "")
		}
	}

	// N1: No auth
	status, _, _ := canary.RawDo(ctx, "GET", cfg.APIURL+"/api/v1/secrets/audit", "", nil)
	run.Assert(status == 401, "audit-no-auth: 401", "")
}
