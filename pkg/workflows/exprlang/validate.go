package exprlang

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"

	"github.com/expr-lang/expr"
)

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

func schemaToStruct(schema map[string]any) (reflect.Type, error) {
	props, _ := schema["properties"].(map[string]any)
	if len(props) == 0 {
		return nil, nil
	}
	fields := make([]reflect.StructField, 0, len(props))
	for name, prop := range props {
		propMap, ok := prop.(map[string]any)
		if !ok {
			continue
		}
		goName := capitalize(name)
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
