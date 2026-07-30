// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

// Canary scenario: S-SECRET-REVEAL
// Tests reveal endpoint requires password reconfirmation.
package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"time"

	canary "github.com/lenaxia/llmsafespaces/sdks/canary/go"
)

func Handler(w http.ResponseWriter, r *http.Request) {
	run := canary.NewRunner("secret-reveal", "go-sdk")
	cfg := canary.ConfigFromEnv()
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	runSecretReveal(ctx, run, cfg)
	run.WriteHTTP(w)
}

func main() {
	run := canary.NewRunner("secret-reveal", "go-sdk")
	cfg := canary.ConfigFromEnv()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	runSecretReveal(ctx, run, cfg)
	res := run.Print()
	if res.Failed > 0 {
		os.Exit(1)
	}
}

func runSecretReveal(ctx context.Context, run *canary.Runner, cfg canary.Config) {
	if cfg.Password == "" {
		run.OK("reveal: skipped (LLMSAFESPACES_PASSWORD not set)")
		return
	}

	c := cfg.Client()
	const secretValue = "canary-reveal-test-val-xyz"

	secret, err := c.Secrets.CreateWithMetadata(ctx, "canary-reveal-test", "env-secret", secretValue, map[string]string{"var_name":"CANARY_VAR"})
	if !run.AssertNoError(err, "create: no error") {
		return
	}
	defer func() { _ = c.Secrets.Delete(context.Background(), secret.ID) }()

	// P2: Reveal with correct password
	val, err := c.Secrets.Reveal(ctx, secret.ID, cfg.Password)
	if run.AssertNoError(err, "reveal-correct-pw: no error") {
		run.Assert(val == secretValue, "reveal: value matches", fmt.Sprintf("got %q", val))
	}

	// P3: Value absent from GET response
	_, body, _ := canary.RawDo(ctx, "GET", cfg.APIURL+"/api/v1/secrets/"+secret.ID, cfg.APIKey, nil)
	run.Assert(!canary.HasField(body, "value"), "get: no value field", "")

	// N1: Missing password body
	status1, _, _ := canary.RawDo(ctx, "POST",
		cfg.APIURL+"/api/v1/secrets/"+secret.ID+"/reveal",
		cfg.APIKey, []byte(`{}`))
	run.Assert(status1 == 400, "reveal-no-password: 400", fmt.Sprintf("got %d", status1))

	// N2: Wrong password
	status2, body2, _ := canary.RawDo(ctx, "POST",
		cfg.APIURL+"/api/v1/secrets/"+secret.ID+"/reveal",
		cfg.APIKey, []byte(`{"password":"definitely-wrong-password-xyz"}`))
	run.Assert(status2 == 403, "reveal-wrong-password: 403", fmt.Sprintf("got %d", status2))
	run.Assert(canary.HasErrorField(body2), "reveal-wrong-password: error field present", "")

	// N3: Nonexistent secret
	status3, _, _ := canary.RawDo(ctx, "POST",
		cfg.APIURL+"/api/v1/secrets/00000000-0000-0000-0000-000000000099/reveal",
		cfg.APIKey, []byte(`{"password":"pw"}`))
	run.Assert(status3 >= 400, "reveal-nonexistent: error status", fmt.Sprintf("got %d", status3))
}
