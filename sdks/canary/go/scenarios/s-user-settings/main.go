// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

// Canary scenario: S-USER-SETTINGS
// Tests GET/PUT user settings and schema version drift detection.
package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"time"

	canary "github.com/lenaxia/llmsafespaces/sdks/canary/go"
)

const expectedSchemaVersion = 6

func Handler(w http.ResponseWriter, r *http.Request) {
	run := canary.NewRunner("user-settings", "go-sdk")
	cfg := canary.ConfigFromEnv()
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	runUserSettings(ctx, run, cfg)
	run.WriteHTTP(w)
}

func main() {
	run := canary.NewRunner("user-settings", "go-sdk")
	cfg := canary.ConfigFromEnv()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	runUserSettings(ctx, run, cfg)
	res := run.Print()
	if res.Failed > 0 {
		os.Exit(1)
	}
}

func runUserSettings(ctx context.Context, run *canary.Runner, cfg canary.Config) {
	c := cfg.Client()

	// P1: GET settings
	settings, err := c.UserSettings.Get(ctx)
	if run.AssertNoError(err, "get-settings: no error") {
		run.Assert(settings.Settings != nil, "get-settings: settings object present", "")
		run.Assert(settings.SchemaVersion > 0, "get-settings: schemaVersion > 0",
			fmt.Sprintf("got %d", settings.SchemaVersion))
	}

	// P2: GET schema
	schema, err := c.UserSettings.GetSchema(ctx)
	if run.AssertNoError(err, "get-schema: no error") {
		sv, _ := schema["schemaVersion"].(float64)
		run.Assert(int(sv) > 0, "get-schema: schemaVersion > 0", fmt.Sprintf("got %v", sv))

		// P3: Schema version drift detection — alert if it changes from expected
		run.Assert(int(sv) == expectedSchemaVersion,
			fmt.Sprintf("schema-version: equals expected %d", expectedSchemaVersion),
			fmt.Sprintf("got %d — SCHEMA DRIFT DETECTED, update canary expectedSchemaVersion", int(sv)))

		_, hasSettings := schema["settings"]
		run.Assert(hasSettings, "get-schema: settings array present", "")
	}

	// P5–P7: SET and verify round-trip
	result, err := c.UserSettings.Set(ctx, "theme", "dark")
	if run.AssertNoError(err, "set-theme: no error") {
		run.Assert(result["key"] == "theme", "set-theme: key field", fmt.Sprintf("%v", result["key"]))
		run.Assert(result["value"] == "dark", "set-theme: value field", fmt.Sprintf("%v", result["value"]))
	}

	settingsAfter, err := c.UserSettings.Get(ctx)
	if run.AssertNoError(err, "get-after-set: no error") {
		theme, _ := settingsAfter.Settings["theme"].(string)
		run.Assert(theme == "dark", "get-after-set: theme=dark", theme)
	}

	// Reset
	_, _ = c.UserSettings.Set(ctx, "theme", "system")

	// N1: No auth
	_, status, _ := rawGet(ctx, cfg.APIURL+"/api/v1/users/me/settings", "")
	run.Assert(status == 401, "no-auth: 401", fmt.Sprintf("got %d", status))

	// N2: Missing value body
	status2, _, _ := canary.RawDo(ctx, "PUT", cfg.APIURL+"/api/v1/users/me/settings/theme", cfg.APIKey, []byte(`{}`))
	run.Assert(status2 == 400, "missing-value: 400", fmt.Sprintf("got %d", status2))

	// N3: Unknown key
	status3, _, _ := canary.RawDo(ctx, "PUT", cfg.APIURL+"/api/v1/users/me/settings/nonexistent.key.xyz",
		cfg.APIKey, []byte(`{"value": "test"}`))
	run.Assert(status3 == 400, "unknown-key: 400", fmt.Sprintf("got %d", status3))
}

func rawGet(ctx context.Context, url, apiKey string) ([]byte, int, error) {
	status, body, err := canary.RawDo(ctx, "GET", url, apiKey, nil)
	return body, status, err
}
