// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package repolint

// CRDDriftCheck — detect schema drift between Go struct definitions
// (the source of truth for what fields the controller binary reads/writes
// against a custom resource) and the chart's CRD YAML (the source of
// truth for what fields kube-apiserver will accept on the wire).
//
// Why this lives in repolint: in the May 2026 cluster recovery (worklog
// 0118 → 0119) we found that the Go AgentSessionStatus type had a
// `Status string` field while the chart CRD still declared
// `lastActivityAt: date-time`. Result: every reconcile that wrote a
// session list logged `unknown field "status.sessions[N].status"` and
// the field was silently dropped by the apiserver. Symptoms were
// invisible in user-facing tests but crippled the frontend's busy/idle
// indicator. The same class of drift had also dropped 12 other status
// fields between the chart and the live CRD.
//
// The check runs at pre-commit (.githooks/pre-commit -> make repolint)
// and in CI (.github/workflows/ci.yml). A diff fails the build with a
// unified-style "+CRD has X" / "+Go has X" report so the operator
// knows which side moved.
//
// Limitations: this is a *name-set* check. It compares the set of JSON
// tag names in the Go struct against the set of property keys in the
// CRD's openAPIV3Schema. It does NOT verify that types match (string
// vs int), that constraints match (enum, format, minLength), or that
// required-list semantics are preserved. Catching field-add and
// field-rename — the two most common drift modes by far in this repo —
// is the explicit goal. Type/constraint drift is a larger surface and
// belongs in a separate kubebuilder-style codegen pipeline.

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// CRDBinding declares one Go-struct-to-CRD-path drift check.
//
// CRDPath walks into the parsed YAML; each element is either a map key
// (most commonly), or a string-encoded array index ("0", "1", …) when
// the path crosses an array. The path must terminate at a YAML node
// that is itself an `openAPIV3Schema`-style object (i.e. it has a
// `properties:` map). The check then compares that node's property
// keys to the JSON-tagged fields of GoStruct.
//
// For nested-struct comparisons (e.g. WorkspaceStatus has a
// `sessions: []AgentSessionStatus` field), declare a separate binding
// for AgentSessionStatus and point its CRDPath at the array's
// `items` schema (so the path ends at "items" rather than at the
// `sessions` array property itself).
type CRDBinding struct {
	// GoFile is the path to the Go source file (relative to repo root)
	// containing GoStruct. The file is parsed standalone; cross-file
	// type references are not followed (drift checks should bind to
	// the leaf struct directly, not to a parent that embeds it).
	GoFile string
	// GoStruct is the unqualified struct type name to inspect.
	GoStruct string
	// CRDFile is the path to the YAML manifest containing the
	// CustomResourceDefinition (relative to repo root).
	CRDFile string
	// CRDPath is the sequence of map keys (and string-encoded array
	// indices) that walks from the YAML document root to the schema
	// object whose `properties:` map should mirror GoStruct's fields.
	CRDPath []string
	// IgnoreGoFields lists Go JSON tags that the CRD does not need
	// to declare (rare; e.g. computed-only fields the API server
	// would never accept). Empty in normal operation.
	IgnoreGoFields []string
	// IgnoreCRDProperties lists CRD property keys that the Go struct
	// does not need to declare (rare; e.g. metadata sentinels that
	// were intentionally added to the schema for kubectl tooling but
	// are not deserialized by the controller).
	IgnoreCRDProperties []string
}

// CRDDriftReport is the result of a single CRDDriftCheck run.
type CRDDriftReport struct {
	// Binding records which check produced this report (so callers
	// can format messages tying the diff back to the file pair).
	Binding CRDBinding
	// GoMissingInCRD lists JSON tag names declared on GoStruct but
	// absent from the CRD's properties map. These are fields the
	// controller will write but kube-apiserver will silently drop.
	GoMissingInCRD []string
	// CRDMissingInGo lists property keys declared on the CRD but
	// absent from GoStruct. These are fields kube-apiserver will
	// accept but no Go reader unmarshals — usually a stale schema
	// from a renamed field.
	CRDMissingInGo []string
}

// OK reports whether the binding is drift-free. A nil-valued report
// (zero struct) is OK; this matches how SequenceReport/DriftReport
// behave in this package and lets callers do `if !rep.OK() {…}`.
func (r CRDDriftReport) OK() bool {
	return len(r.GoMissingInCRD) == 0 && len(r.CRDMissingInGo) == 0
}

// String returns a human-readable, unified-style diff of the drift,
// or "(ok)" when there is none.
func (r CRDDriftReport) String() string {
	if r.OK() {
		return "(ok)"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "  binding: %s::%s  vs  %s @ %s\n",
		r.Binding.GoFile, r.Binding.GoStruct,
		r.Binding.CRDFile, strings.Join(r.Binding.CRDPath, "."))
	if len(r.GoMissingInCRD) > 0 {
		fmt.Fprintf(&b, "  Go has but CRD missing (%d):\n", len(r.GoMissingInCRD))
		for _, f := range r.GoMissingInCRD {
			fmt.Fprintf(&b, "    + %s    (declared in Go, will be silently dropped by apiserver)\n", f)
		}
	}
	if len(r.CRDMissingInGo) > 0 {
		fmt.Fprintf(&b, "  CRD has but Go missing (%d):\n", len(r.CRDMissingInGo))
		for _, f := range r.CRDMissingInGo {
			fmt.Fprintf(&b, "    + %s    (declared in CRD, no Go reader; likely stale after rename)\n", f)
		}
	}
	return b.String()
}

// CRDDriftCheck loads the Go file and CRD YAML referenced by `b`
// (resolved relative to `root`) and compares the field/property names.
//
// Returns an error only on parse failure or path-not-found (i.e. the
// CRDPath does not resolve in the YAML document, or GoStruct is not
// found in GoFile). Drift itself is non-fatal and surfaces via
// CRDDriftReport.OK()==false.
func CRDDriftCheck(root string, b CRDBinding) (CRDDriftReport, error) {
	rep := CRDDriftReport{Binding: b}

	goFields, err := extractJSONTags(filepath.Join(root, b.GoFile), b.GoStruct)
	if err != nil {
		return rep, fmt.Errorf("parse Go: %w", err)
	}
	crdProps, err := extractCRDProperties(filepath.Join(root, b.CRDFile), b.CRDPath)
	if err != nil {
		return rep, fmt.Errorf("parse CRD: %w", err)
	}

	ignoreGo := toSet(b.IgnoreGoFields)
	ignoreCRD := toSet(b.IgnoreCRDProperties)

	// goFields - crdProps (modulo ignoreGo)
	for _, f := range goFields {
		if _, ok := crdProps[f]; ok {
			continue
		}
		if _, ok := ignoreGo[f]; ok {
			continue
		}
		rep.GoMissingInCRD = append(rep.GoMissingInCRD, f)
	}
	// crdProps - goFields (modulo ignoreCRD)
	goSet := toSet(goFields)
	for f := range crdProps {
		if _, ok := goSet[f]; ok {
			continue
		}
		if _, ok := ignoreCRD[f]; ok {
			continue
		}
		rep.CRDMissingInGo = append(rep.CRDMissingInGo, f)
	}
	sort.Strings(rep.GoMissingInCRD)
	sort.Strings(rep.CRDMissingInGo)
	return rep, nil
}

// ---------------------------------------------------------------------------
// Go AST → JSON-tag extraction
// ---------------------------------------------------------------------------

// extractJSONTags returns the deduplicated set of JSON field names
// declared by struct typeName in the given Go source file. Embedded
// structs (no field name; type referenced by name) are NOT followed —
// the binding for that nested type should be declared separately.
//
// Special cases handled:
//   - `json:"name"`                — name "name" included.
//   - `json:"name,omitempty"`      — name "name" included.
//   - `json:",inline"`             — field skipped (caller's binding
//     for the inlined type covers it).
//   - `json:"-"`                   — field excluded entirely (Go-only).
//   - field with no `json:` tag    — field excluded (camelCase default
//     is too fragile to assume; explicit
//     tags are the project convention).
func extractJSONTags(path, typeName string) ([]string, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, src, parser.ParseComments)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}

	var found *ast.StructType
	ast.Inspect(file, func(n ast.Node) bool {
		ts, ok := n.(*ast.TypeSpec)
		if !ok {
			return true
		}
		if ts.Name.Name != typeName {
			return true
		}
		if st, ok := ts.Type.(*ast.StructType); ok {
			found = st
			return false
		}
		return true
	})
	if found == nil {
		return nil, fmt.Errorf("type %s not found in %s", typeName, path)
	}

	out := make([]string, 0, len(found.Fields.List))
	seen := map[string]bool{}
	for _, field := range found.Fields.List {
		// Anonymous embedded field (e.g. `metav1.TypeMeta`) — skip.
		// Bind the embedded type with its own CRDBinding if needed.
		if len(field.Names) == 0 {
			continue
		}
		// Skip unexported fields (Go won't marshal them anyway).
		if !field.Names[0].IsExported() {
			continue
		}
		if field.Tag == nil {
			continue
		}
		tag := reflect.StructTag(strings.Trim(field.Tag.Value, "`"))
		jt := tag.Get("json")
		if jt == "" {
			continue
		}
		// Split off ",omitempty" / ",inline" / ",string"
		comma := strings.IndexByte(jt, ',')
		var name, opts string
		if comma == -1 {
			name = jt
		} else {
			name, opts = jt[:comma], jt[comma+1:]
		}
		if name == "-" {
			continue
		}
		// `json:",inline"` — name is empty, only opts present.
		// The fields of the embedded type are reachable via that
		// type's own binding; don't claim them here.
		if name == "" && strings.Contains(opts, "inline") {
			continue
		}
		if name == "" {
			// `json:",omitempty"` with no name is legal Go but never
			// a project convention. Treat as no tag (skip).
			continue
		}
		if !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out, nil
}

// ---------------------------------------------------------------------------
// CRD YAML → property-key extraction
// ---------------------------------------------------------------------------

// extractCRDProperties walks the YAML document at path along crdPath
// and returns the keys of the .properties map at the terminal node.
//
// Each crdPath element is interpreted as:
//   - a map key, if the current node is a YAML mapping;
//   - an integer index, if the current node is a YAML sequence
//     (the element must parse as a non-negative decimal integer).
//
// Returns ErrCRDPathNotFound (wrapped) if any element cannot be
// resolved, with the offending path prefix in the error message so
// the operator can fix the binding without re-deriving the path.
func extractCRDProperties(path string, crdPath []string) (map[string]struct{}, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(src, &doc); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}

	// yaml.Node wraps the document in a single-child Node of kind
	// DocumentNode. Descend into it.
	if doc.Kind == yaml.DocumentNode {
		if len(doc.Content) == 0 {
			return nil, fmt.Errorf("%s: empty YAML document", path)
		}
		doc = *doc.Content[0]
	}

	cur := &doc
	for i, key := range crdPath {
		next, err := stepInto(cur, key)
		if err != nil {
			return nil, fmt.Errorf("%s: at path %q: %w",
				path, strings.Join(crdPath[:i+1], "."), err)
		}
		cur = next
	}

	// `cur` should be the schema object — find its `properties` key.
	props, err := stepInto(cur, "properties")
	if err != nil {
		return nil, fmt.Errorf("%s: terminal node at %s has no `properties` map: %w",
			path, strings.Join(crdPath, "."), err)
	}
	if props.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("%s: `properties` at %s is not a mapping (kind=%d)",
			path, strings.Join(crdPath, "."), props.Kind)
	}

	out := map[string]struct{}{}
	for i := 0; i < len(props.Content); i += 2 {
		k := props.Content[i]
		if k.Kind == yaml.ScalarNode {
			out[k.Value] = struct{}{}
		}
	}
	return out, nil
}

// stepInto descends one level in the YAML tree along `key`. For a
// mapping node, key is matched against scalar children. For a
// sequence node, key is parsed as an integer index.
func stepInto(node *yaml.Node, key string) (*yaml.Node, error) {
	switch node.Kind {
	case yaml.MappingNode:
		for i := 0; i < len(node.Content); i += 2 {
			k, v := node.Content[i], node.Content[i+1]
			if k.Kind == yaml.ScalarNode && k.Value == key {
				return v, nil
			}
		}
		return nil, fmt.Errorf("key %q not found in mapping", key)
	case yaml.SequenceNode:
		var idx int
		if _, err := fmt.Sscanf(key, "%d", &idx); err != nil {
			return nil, fmt.Errorf("expected integer index, got %q", key)
		}
		if idx < 0 || idx >= len(node.Content) {
			return nil, fmt.Errorf("sequence index %d out of range [0,%d)", idx, len(node.Content))
		}
		return node.Content[idx], nil
	case yaml.AliasNode:
		// Follow YAML aliases (rare in CRDs, but well-defined).
		return stepInto(node.Alias, key)
	default:
		return nil, fmt.Errorf("cannot descend into node kind %d with key %q", node.Kind, key)
	}
}

func toSet(ss []string) map[string]struct{} {
	out := make(map[string]struct{}, len(ss))
	for _, s := range ss {
		out[s] = struct{}{}
	}
	return out
}

// LiveBindings returns the canonical (Go struct → CRD path) pairs that
// must remain drift-free in this repository. Both the CLI driver
// (cmd/repolint) and the live-tree test (TestLive_CRDDrift_NoDrift)
// consume this so they stay in sync; adding a binding here surfaces
// it in pre-commit, CI, and the test suite simultaneously.
//
// Binding rules:
//
//  1. Bind each leaf struct that the controller reads/writes through
//     the apiserver. Embedded/inline parents don't need bindings;
//     their fields are accounted for through the leaf.
//
//  2. Slice-of-struct fields need a separate binding for the element
//     type, with CRDPath ending in "items" rather than at the slice
//     property. AgentSessionStatus is the canonical example.
//
//  3. IgnoreGoFields / IgnoreCRDProperties are deliberately empty in
//     a healthy repo. Any non-empty entry should be paired with a
//     comment explaining why the asymmetry is intentional.
func LiveBindings() []CRDBinding {
	return []CRDBinding{
		{
			GoFile:   "pkg/apis/llmsafespaces/v1/workspace_types.go",
			GoStruct: "WorkspaceSpec",
			CRDFile:  "helm/crds/workspace.yaml",
			CRDPath:  []string{"spec", "versions", "0", "schema", "openAPIV3Schema", "properties", "spec"},
		},
		{
			GoFile:   "pkg/apis/llmsafespaces/v1/workspace_types.go",
			GoStruct: "WorkspaceStatus",
			CRDFile:  "helm/crds/workspace.yaml",
			CRDPath:  []string{"spec", "versions", "0", "schema", "openAPIV3Schema", "properties", "status"},
		},
		{
			GoFile:   "pkg/apis/llmsafespaces/v1/workspace_types.go",
			GoStruct: "AgentSessionStatus",
			CRDFile:  "helm/crds/workspace.yaml",
			CRDPath: []string{"spec", "versions", "0", "schema", "openAPIV3Schema",
				"properties", "status", "properties", "sessions", "items"},
		},
		{
			GoFile:   "pkg/apis/llmsafespaces/v1/runtimeenvironment_types.go",
			GoStruct: "RuntimeEnvironmentSpec",
			CRDFile:  "helm/crds/runtimeenvironment.yaml",
			CRDPath:  []string{"spec", "versions", "0", "schema", "openAPIV3Schema", "properties", "spec"},
		},
		{
			GoFile:   "pkg/apis/llmsafespaces/v1/runtimeenvironment_types.go",
			GoStruct: "RuntimeEnvironmentStatus",
			CRDFile:  "helm/crds/runtimeenvironment.yaml",
			CRDPath:  []string{"spec", "versions", "0", "schema", "openAPIV3Schema", "properties", "status"},
		},
		{
			GoFile:   "pkg/apis/llmsafespaces/v1/inferencerelay_types.go",
			GoStruct: "InferenceRelaySpec",
			CRDFile:  "helm/crds/inferencerelay.yaml",
			CRDPath:  []string{"spec", "versions", "0", "schema", "openAPIV3Schema", "properties", "spec"},
		},
		{
			GoFile:   "pkg/apis/llmsafespaces/v1/inferencerelay_types.go",
			GoStruct: "InferenceRelayStatus",
			CRDFile:  "helm/crds/inferencerelay.yaml",
			CRDPath:  []string{"spec", "versions", "0", "schema", "openAPIV3Schema", "properties", "status"},
		},
		{
			GoFile:   "pkg/apis/llmsafespaces/v1/inferencerelay_types.go",
			GoStruct: "RelayInstanceStatus",
			CRDFile:  "helm/crds/inferencerelay.yaml",
			CRDPath:  []string{"spec", "versions", "0", "schema", "openAPIV3Schema", "properties", "status", "properties", "instances", "items"},
		},
	}
}
