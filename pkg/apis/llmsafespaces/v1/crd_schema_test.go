// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package v1

// CRD↔Go-type structural-drift test.
//
// Compares the chart's CRD openAPIV3Schema against the Go struct
// definitions in this package. Catches the class:
//
//   - added a field to the Go struct, forgot to update the CRD
//   - removed a field from the Go struct, CRD still mentions it
//   - typo in JSON tag drifts from CRD property name
//
// Does NOT validate types/constraints/enums/defaults — that requires
// running controller-gen. We chose structural-only here because it's
// fast (parse YAML once + reflect once) and catches the most common
// drift. The richer check is on the roadmap (epic-19 follow-up).
//
// The Go types we cross-check live in workspace_types.go and
// runtimeenvironment_types.go. Adding a new CRD type means adding it
// to crdSpecs at the top of TestCRDSchemaMatchesGoTypes.

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestCRDSchemaMatchesGoTypes asserts every Go-struct field in our
// CRD types appears in the chart's openAPIV3Schema, and vice versa.
func TestCRDSchemaMatchesGoTypes(t *testing.T) {
	repoRoot, err := findRepoRootForCRD()
	if err != nil {
		t.Fatalf("repo root: %v", err)
	}

	type spec struct {
		crdFile         string
		specType        reflect.Type
		statusType      reflect.Type
		ignoredCRDProps map[string]string // prop name -> reason
		ignoredGoFields map[string]string // json tag -> reason
	}

	specs := []spec{
		{
			crdFile:         "helm/crds/workspace.yaml",
			specType:        reflect.TypeOf(WorkspaceSpec{}),
			statusType:      reflect.TypeOf(WorkspaceStatus{}),
			ignoredCRDProps: map[string]string{
				// Add CRD properties that intentionally don't have a
				// Go counterpart here, with rationale. Empty today.
			},
			ignoredGoFields: map[string]string{
				// Add Go fields that intentionally aren't in the CRD
				// here (e.g. internal-only computed fields). Empty
				// today.
			},
		},
		{
			crdFile:    "helm/crds/runtimeenvironment.yaml",
			specType:   reflect.TypeOf(RuntimeEnvironmentSpec{}),
			statusType: reflect.TypeOf(RuntimeEnvironmentStatus{}),
		},
	}

	for _, s := range specs {
		t.Run(filepath.Base(s.crdFile), func(t *testing.T) {
			schemaPath := filepath.Join(repoRoot, s.crdFile)
			data, err := os.ReadFile(schemaPath)
			if err != nil {
				t.Fatalf("read CRD: %v", err)
			}

			specProps, err := extractSchemaProps(data, "spec")
			if err != nil {
				t.Fatalf("extract spec props from %s: %v", s.crdFile, err)
			}
			statusProps, err := extractSchemaProps(data, "status")
			if err != nil {
				// Some CRDs don't have a status block in the schema (it
				// may be entirely controller-managed). Tolerate.
				statusProps = nil
			}

			compareFields(t, "spec", specProps, s.specType, s.ignoredCRDProps, s.ignoredGoFields)
			if s.statusType != nil && statusProps != nil {
				compareFields(t, "status", statusProps, s.statusType, nil, nil)
			}
		})
	}
}

// compareFields asserts that the set of CRD property names matches
// the set of JSON tags on the Go struct, modulo allowlists.
func compareFields(t *testing.T, name string, crdProps map[string]any, goType reflect.Type, ignoredCRD, ignoredGo map[string]string) {
	t.Helper()

	goFields := jsonTagsOf(goType)

	// CRD has a property the Go struct doesn't.
	var crdOnly []string
	for k := range crdProps {
		if !goFields[k] && ignoredCRD[k] == "" {
			crdOnly = append(crdOnly, k)
		}
	}
	sort.Strings(crdOnly)

	// Go struct has a JSON tag the CRD doesn't.
	var goOnly []string
	for k := range goFields {
		if _, present := crdProps[k]; !present && ignoredGo[k] == "" {
			goOnly = append(goOnly, k)
		}
	}
	sort.Strings(goOnly)

	if len(crdOnly) > 0 {
		t.Errorf("[%s] CRD has %d propertie(s) without a matching Go field:", name, len(crdOnly))
		for _, p := range crdOnly {
			t.Errorf("  %s", p)
		}
		t.Errorf("Either add the field to the Go struct or add to ignoredCRDProps with rationale.")
	}
	if len(goOnly) > 0 {
		t.Errorf("[%s] Go struct has %d field(s) not in the CRD schema:", name, len(goOnly))
		for _, p := range goOnly {
			t.Errorf("  %s", p)
		}
		t.Errorf("Either add to the CRD schema or add to ignoredGoFields with rationale.")
	}
}

// extractSchemaProps walks the CRD's openAPIV3Schema for the given
// top-level field (e.g. "spec" or "status") and returns its
// properties map. Returns an empty map (not error) if the field is
// absent — some CRDs have spec but no explicit status schema.
func extractSchemaProps(data []byte, field string) (map[string]any, error) {
	var doc struct {
		Spec struct {
			Versions []struct {
				Schema struct {
					OpenAPIV3Schema struct {
						Properties map[string]any `yaml:"properties"`
					} `yaml:"openAPIV3Schema"`
				} `yaml:"schema"`
			} `yaml:"versions"`
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("unmarshal: %w", err)
	}
	if len(doc.Spec.Versions) == 0 {
		return nil, fmt.Errorf("no versions in CRD")
	}
	root := doc.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties
	sub, ok := root[field].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("no %s in schema", field)
	}
	props, ok := sub["properties"].(map[string]any)
	if !ok {
		// status with no properties is legal; return empty.
		return map[string]any{}, nil
	}
	return props, nil
}

// jsonTagsOf returns the set of JSON tag names declared on a struct
// type, EXCLUDING fields tagged `json:"-"` and embedded ones.
// Inline tags (`json:",inline"`) are unwrapped: their fields are
// promoted into the parent's field set.
func jsonTagsOf(t reflect.Type) map[string]bool {
	tags := make(map[string]bool)
	if t == nil || t.Kind() != reflect.Struct {
		return tags
	}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		tag := f.Tag.Get("json")
		if tag == "" || tag == "-" {
			continue
		}
		// Split off ",omitempty" / ",string" suffix.
		name := tag
		if comma := strings.IndexByte(tag, ','); comma >= 0 {
			name = tag[:comma]
			rest := tag[comma:]
			if strings.Contains(rest, "inline") {
				// Promote embedded struct fields up one level.
				for k := range jsonTagsOf(f.Type) {
					tags[k] = true
				}
				continue
			}
		}
		if name != "" {
			tags[name] = true
		}
	}
	return tags
}

// findRepoRootForCRD walks up looking for go.mod. Same as the OpenAPI
// contract test's helper — duplicated to keep the test files
// independent.
func findRepoRootForCRD() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no go.mod found from working directory")
		}
		dir = parent
	}
}
