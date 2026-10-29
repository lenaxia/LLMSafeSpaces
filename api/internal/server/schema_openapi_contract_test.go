// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package server

// OpenAPI ↔ wire schema-parity contract test (review round 3's gap:
// sdk-check validates spec shape + router parity but nothing compares
// the spec's SCHEMA FIELDS against the structs actually serialized —
// the round-2 openapi re-base silently deleted BoardTask.pending_input
// and every gate stayed green while spec, wire, and the shipped SDKs
// disagreed).
//
// BoardTask is the load-bearing case (the task board's wire shape, in
// SDKs and the frontend); the pattern extends to other schemas by
// adding them to the parity table.

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/lenaxia/llmsafespaces/pkg/board"
)

// specProperties extracts one component schema's property names from
// sdks/openapi.yaml.
func specProperties(t *testing.T, schema string) map[string]bool {
	t.Helper()
	repoRoot, err := findRepoRoot()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(repoRoot, "sdks", "openapi.yaml"))
	if err != nil {
		t.Fatalf("read openapi.yaml: %v", err)
	}
	var spec struct {
		Components struct {
			Schemas map[string]struct {
				Properties map[string]yaml.Node `yaml:"properties"`
			} `yaml:"schemas"`
		} `yaml:"components"`
	}
	if err := yaml.Unmarshal(data, &spec); err != nil {
		t.Fatalf("parse openapi.yaml: %v", err)
	}
	sch, ok := spec.Components.Schemas[schema]
	if !ok {
		t.Fatalf("openapi.yaml has no components.schemas.%s", schema)
	}
	out := make(map[string]bool, len(sch.Properties))
	for name := range sch.Properties {
		out[name] = true
	}
	return out
}

// wireFields extracts a struct's serialized field names (json tags;
// "-" and unexported fields skip).
func wireFields(v any) map[string]bool {
	out := map[string]bool{}
	rt := reflect.TypeOf(v)
	for rt != nil && rt.Kind() == reflect.Ptr {
		rt = rt.Elem()
	}
	if rt == nil || rt.Kind() != reflect.Struct {
		panic("schemaParity: non-struct wire type")
	}
	for i := 0; i < rt.NumField(); i++ {
		f := rt.Field(i)
		if !f.IsExported() {
			continue
		}
		tag := f.Tag.Get("json")
		if tag == "" {
			out[f.Name] = true
			continue
		}
		name := strings.SplitN(tag, ",", 2)[0]
		if name != "-" {
			out[name] = true
		}
	}
	return out
}

// parityAllowlist documents each known wire-only field with rationale
// (the router-contract pattern: exceptions carry justification, never
// silent additions).
var parityAllowlist = map[string]map[string]string{
	"BoardTask": {
		// Wire-only fields, deliberately not spec-declared (engine
		// internals that ride the JSON row; spec-declaring them is
		// follow-up work, tracked by their presence HERE — the gap is
		// explicit, never silent):
		"claim_incremented_attempts":  "engine accounting detail (the #1832 provenance flag) — pre-existing wire-only",
		"assignee_session_deprecated": "#1948 dual-accept mirror — pre-existing wire-only",
		"claim_persona":               "stage-persona provenance — pre-existing wire-only",
		// Spec-only fields, pre-existing drift (documented on BoardTask
		// but not serialized by board.Task itself):
		"mutated_criterion": "pre-existing spec-only (criteria-check payload shape), not this lane",
		"content_echo_diff": "pre-existing spec-only (criteria-check payload shape), not this lane",
	},
}

// TestOpenAPISchemaParity_BoardTask: every json-serialized board.Task
// field appears as a BoardTask property in the spec, and every spec
// property exists on the wire struct — deletion drift and addition
// drift both fail.
func TestOpenAPISchemaParity_BoardTask(t *testing.T) {
	spec := specProperties(t, "BoardTask")
	wire := wireFields(&board.Task{})

	for name := range wire {
		if spec[name] {
			continue
		}
		if _, ok := parityAllowlist["BoardTask"][name]; ok {
			continue
		}
		t.Errorf("board.Task serializes %q but openapi BoardTask has no such property — the spec is the SDK contract source (regenerating SDKs would silently drop it)", name)
	}
	for name := range spec {
		if wire[name] {
			continue
		}
		if _, ok := parityAllowlist["BoardTask"][name]; ok {
			continue
		}
		t.Errorf("openapi BoardTask declares %q but board.Task never serializes it — spec/wire drift", name)
	}
}
