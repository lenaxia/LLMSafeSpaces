// Package exprlang compiles condition expressions for workflow DAG validation.
// It builds a typed environment from a JSON Schema (the predecessor node's
// outputSchema), then uses expr-lang to compile the expression — catching
// missing fields, wrong types, and syntax errors at validate time rather than
// at runtime.
//
// SECURITY: SchemaToEnv and CompileCondition must not panic on adversarial
// or malformed input. JSON Schemas are user-authored (workflow definitions);
// duplicate field names after CamelCase conversion, invalid Go identifiers,
// and other edge cases are caught and returned as errors before reaching
// reflect.StructOf.
package exprlang

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"unicode"

	"github.com/expr-lang/expr"
)

// SchemaToEnv converts a JSON Schema (the predecessor node's outputSchema)
// into an expr-lang environment: a map with an "input" key whose value is a
// pointer to a reflect.StructOf-generated struct. The struct's fields are
// CamelCase versions of the JSON Schema's property names (snake_case,
// kebab-case, etc. all become CamelCase), with Go types derived from the
// schema's "type" keyword.
//
// The returned environment type-checks strictly: missing fields and wrong
// types in expressions produce compile errors. A nil/empty schema produces a
// map[string]any environment (no type-checking); callers requiring strict
// checking should reject empty schemas upstream.
func SchemaToEnv(schemaJSON []byte) (map[string]any, error) {
	var schema map[string]any
	if err := json.Unmarshal(schemaJSON, &schema); err != nil {
		return nil, fmt.Errorf("parse schema: %w", err)
	}
	structTyp, err := schemaToStruct(schema)
	if err != nil {
		return nil, err
	}
	if structTyp == nil {
		return map[string]any{"input": map[string]any{}}, nil
	}
	val := reflect.New(structTyp).Elem().Addr().Interface()
	return map[string]any{"input": val}, nil
}

// CompileCondition compiles a single condition expression against the
// JSON-Schema-derived environment. Returns nil on success; on failure, returns
// an error detailing the type mismatch, missing field, or syntax error.
// Expressions reference fields via input.FieldName (CamelCase).
func CompileCondition(expression string, schemaJSON []byte) error {
	env, err := SchemaToEnv(schemaJSON)
	if err != nil {
		return err
	}
	_, err = expr.Compile(expression, expr.Env(env))
	if err != nil {
		return fmt.Errorf("compile condition: %w", err)
	}
	return nil
}

func schemaToStruct(schema map[string]any) (reflect.Type, error) {
	props, _ := schema["properties"].(map[string]any)
	if len(props) == 0 {
		return nil, nil
	}
	fields := make([]reflect.StructField, 0, len(props))
	seenNames := make(map[string]string, len(props))
	for name, prop := range props {
		propMap, ok := prop.(map[string]any)
		if !ok {
			continue
		}
		goName := capitalize(name)
		if !isValidGoIdentifier(goName) {
			return nil, fmt.Errorf("property %q produces invalid Go identifier %q", name, goName)
		}
		if prev, exists := seenNames[goName]; exists {
			return nil, fmt.Errorf("properties %q and %q collide after CamelCase conversion (both → %q)", prev, name, goName)
		}
		seenNames[goName] = name
		fieldType, err := schemaTypeToGoType(propMap)
		if err != nil {
			return nil, fmt.Errorf("property %q: %w", name, err)
		}
		fields = append(fields, reflect.StructField{
			Name: goName,
			Type: fieldType,
			Tag:  reflect.StructTag(`json:"` + name + `"`),
		})
	}
	return reflect.StructOf(fields), nil
}

// schemaTypeToGoType maps JSON Schema types to Go reflect.Types. Arrays with
// an `items` schema return []any{} (the items schema is NOT reflected into
// the element type — array-element access will not type-check). Document this
// limitation in US-64.4: conditions referencing input.Tags[0].Name will
// compile but not type-check the element shape.
func schemaTypeToGoType(schema map[string]any) (reflect.Type, error) {
	t, _ := schema["type"].(string)
	switch t {
	case "boolean":
		return reflect.TypeOf(false), nil
	case "string":
		return reflect.TypeOf(""), nil
	case "integer":
		return reflect.TypeOf(int(0)), nil
	case "number":
		return reflect.TypeOf(float64(0)), nil
	case "array":
		// NOTE: items schema not reflected — array element access is untyped.
		return reflect.TypeOf([]any{}), nil
	case "object":
		nested, err := schemaToStruct(schema)
		if err != nil {
			return nil, err
		}
		if nested == nil {
			return reflect.TypeOf(map[string]any{}), nil
		}
		return nested, nil
	default:
		return reflect.TypeOf(map[string]any{}), nil
	}
}

// capitalize converts a JSON property name to a Go-exportable field name.
// Splits on underscore and hyphen, capitalizes each segment, joins. Examples:
// foo_bar → FooBar, foo-bar → FooBar, fooBar → FooBar (note: collides with
// foo_bar — schemaToStruct detects this and returns an error).
func capitalize(s string) string {
	if s == "" {
		return s
	}
	parts := strings.FieldsFunc(s, func(r rune) bool {
		return r == '_' || r == '-'
	})
	for i, p := range parts {
		if p != "" {
			parts[i] = strings.ToUpper(p[:1]) + p[1:]
		}
	}
	return strings.Join(parts, "")
}

// isValidGoIdentifier reports whether s is a valid Go-exported identifier:
// starts with an uppercase letter (so it's exported), followed by letters or
// digits, non-empty. Disallows digits-first, spaces, punctuation, $.
func isValidGoIdentifier(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		if i == 0 {
			if !unicode.IsUpper(r) {
				return false
			}
			continue
		}
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			return false
		}
	}
	return true
}
