// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

// Canary scenario: S-RATE-LIMIT
// Tests auth endpoint rate limiting by making rapid login attempts with
// wrong credentials and verifying 429 responses, while ensuring health
// endpoints remain reachable.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"time"

	canary "github.com/lenaxia/llmsafespaces/sdks/canary/go"
)

func Handler(w http.ResponseWriter, r *http.Request) {
	run := canary.NewRunner("rate-limit", "go-sdk")
	cfg := canary.ConfigFromEnv()
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	runRateLimit(ctx, run, cfg)
	run.WriteHTTP(w)
}

func main() {
	run := canary.NewRunner("rate-limit", "go-sdk")
	cfg := canary.ConfigFromEnv()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	runRateLimit(ctx, run, cfg)
	res := run.Print()
	if res.Failed > 0 {
		os.Exit(1)
	}
}

func runRateLimit(ctx context.Context, run *canary.Runner, cfg canary.Config) {
	loginURL := cfg.APIURL + "/api/v1/auth/login"
	loginBody := []byte(`{"email":"canary-ratelimit-ghost@nonexistent.invalid","password":"wrongpassword123"}`)

	status, _, err := canary.RawDo(ctx, "POST", loginURL, "", loginBody)
	run.AssertNoError(err, "P1: first login attempt no transport error")
	if err == nil {
		run.Assert(status == http.StatusOK || status == http.StatusUnauthorized,
			"P1: first login returns 200 or 401", fmt.Sprintf("got %d", status))
		run.Assert(status != http.StatusTooManyRequests,
			"P1: first login is not 429", fmt.Sprintf("got %d", status))
	}

	var got429 bool
	var body429 []byte
	for i := 0; i < 30; i++ {
		status, body, err := canary.RawDo(ctx, "POST", loginURL, "", loginBody)
		if err != nil {
			continue
		}
		if status == http.StatusTooManyRequests {
			got429 = true
			body429 = body
			break
		}
	}
	run.Assert(got429, "P2: rapid burst triggers 429", "no 429 after 30 rapid login attempts")

	if got429 && body429 != nil {
		run.Assert(canary.HasErrorField(body429),
			"P3: 429 body has error field", "")

		var obj map[string]any
		if jsonErr := json.Unmarshal(body429, &obj); jsonErr == nil {
			_, hasError := obj["error"]
			run.Assert(hasError, "P3: 429 body has error field", fmt.Sprintf("fields: %v", obj))
		}
	}

	for _, ep := range []struct {
		name string
		path string
	}{
		{"readyz", "/readyz"},
		{"livez", "/livez"},
	} {
		status, err := canary.JSONGet(ctx, cfg.APIURL+ep.path, "", nil)
		run.AssertNoError(err, "N1: "+ep.name+" reachable")
		if err == nil {
			run.Assert(status != http.StatusTooManyRequests,
				"N1: "+ep.name+" not rate-limited",
				fmt.Sprintf("got %d", status))
		}
	}
}
