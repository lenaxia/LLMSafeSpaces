// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

// Canary scenario: S-SECRET-CRUD
// Tests secret create/list/get/update/delete and name validation.
package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"time"

	llm "github.com/lenaxia/llmsafespaces/sdk/go"
	canary "github.com/lenaxia/llmsafespaces/sdks/canary/go"
)

func Handler(w http.ResponseWriter, r *http.Request) {
	run := canary.NewRunner("secret-crud", "go-sdk")
	cfg := canary.ConfigFromEnv()
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	runSecretCRUD(ctx, run, cfg)
	run.WriteHTTP(w)
}

func main() {
	run := canary.NewRunner("secret-crud", "go-sdk")
	cfg := canary.ConfigFromEnv()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	runSecretCRUD(ctx, run, cfg)
	res := run.Print()
	if res.Failed > 0 {
		os.Exit(1)
	}
}

func runSecretCRUD(ctx context.Context, run *canary.Runner, cfg canary.Config) {
	c := cfg.Client()
	const name = "canary-secret-crud"
	const value = "canary-value-xyz-123"

	// P1: Create
	secret, err := c.Secrets.CreateWithMetadata(ctx, name, "env-secret", value, map[string]string{"var_name":"CANARY_VAR"})
	if !run.AssertNoError(err, "create: no error") {
		return
	}
	run.Assert(secret.ID != "", "create: id non-empty", "")
	run.Assert(secret.Name == name, "create: name", secret.Name)
	run.Assert(secret.Type == "env-secret", "create: type", secret.Type)
	secretID := secret.ID

	defer func() { _ = c.Secrets.Delete(context.Background(), secretID) }()

	// P2: List ({"secrets": [...]})
	list, err := c.Secrets.List(ctx)
	if run.AssertNoError(err, "list: no error") {
		found := false
		for _, s := range list {
			if s.ID == secretID {
				found = true
				break
			}
		}
		run.Assert(found, "list: secret present", "")
	}

	// P3: Get (no value field)
	got, err := c.Secrets.Get(ctx, secretID)
	if run.AssertNoError(err, "get: no error") {
		run.Assert(got.Name == name, "get: name", got.Name)
		// Confirm value field is absent via raw response
		statusCode, body, _ := canary.RawDo(ctx, "GET",
			cfg.APIURL+"/api/v1/secrets/"+secretID, cfg.APIKey, nil)
		run.Assert(statusCode == 200, "get-raw: 200", fmt.Sprintf("got %d", statusCode))
		run.Assert(!canary.HasField(body, "value"), "get-raw: no value field", "")
	}

	// P4: Update value
	err = c.Secrets.Update(ctx, secretID, "updated-canary-value")
	run.AssertNoError(err, "update: no error")

	// P5: Delete
	err = c.Secrets.Delete(ctx, secretID)
	run.AssertNoError(err, "delete: no error")

	// P6: Re-create with same name after delete — should succeed (no lingering 409)
	secret2, err := c.Secrets.CreateWithMetadata(ctx, name, "env-secret", value, map[string]string{"var_name":"CANARY_VAR"})
	if run.AssertNoError(err, "re-create-after-delete: no error") {
		run.Assert(secret2.ID != secretID, "re-create-after-delete: new id", "")
		_ = c.Secrets.Delete(ctx, secret2.ID)
	}

	// N1: Get nonexistent
	_, err = c.Secrets.Get(ctx, "00000000-0000-0000-0000-000000000099")
	run.Assert(err != nil && llm.IsNotFound(err), "get-nonexistent: 404",
		canary.ErrDetail(err, "expected 404"))

	// N2: Invalid name (uppercase)
	_, err = c.Secrets.CreateWithMetadata(ctx, "My-Secret-UPPER", "env-secret", "val", map[string]string{"var_name":"CANARY_VAR"})
	run.Assert(err != nil, "create-invalid-name: error", canary.ErrDetail(err, "expected 400"))

	// N3: Empty name
	_, err = c.Secrets.CreateWithMetadata(ctx, "", "env-secret", "val", map[string]string{"var_name":"CANARY_VAR"})
	run.Assert(err != nil, "create-empty-name: error", canary.ErrDetail(err, "expected 400"))

	// N4: Duplicate name — create two with same name
	s1, _ := c.Secrets.CreateWithMetadata(ctx, "canary-dup-test", "env-secret", "val1", map[string]string{"var_name":"CANARY_VAR"})
	if s1 != nil {
		defer func() { _ = c.Secrets.Delete(context.Background(), s1.ID) }()
		_, err = c.Secrets.CreateWithMetadata(ctx, "canary-dup-test", "env-secret", "val2", map[string]string{"var_name":"CANARY_VAR"})
		run.Assert(err != nil && llm.IsConflict(err), "create-duplicate: 409 Conflict",
			canary.ErrDetail(err, "expected 409"))
	}

	// N5: Delete nonexistent
	err = c.Secrets.Delete(ctx, "00000000-0000-0000-0000-000000000098")
	run.Assert(err != nil, "delete-nonexistent: error", canary.ErrDetail(err, "expected error"))
}
