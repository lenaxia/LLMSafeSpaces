// crd-doc-gen generates docs/reference/crds.md from kubebuilder-annotated
// Go types in pkg/apis/llmsafespaces/v1/. Run from the repo root:
//
//	go run ./cmd/crd-doc-gen -w
//
// The generator replaces content between <!-- crd-gen:start:TYPENAME --> and
// <!-- crd-gen:end:TYPENAME --> markers. Everything outside markers is
// preserved verbatim. This means hand-written sub-struct tables, examples,
// and prose survive regeneration.
//
// Types generated: WorkspaceSpec, WorkspaceStatus, RuntimeEnvironmentSpec,
// RuntimeEnvironmentStatus, InferenceRelaySpec, InferenceRelayStatus.

package main

import (
	"fmt"
	"go/ast"
	"go/token"
	"os"
	"sort"
	"strings"

	"golang.org/x/tools/go/packages"
)

func main() {
	writeMode := len(os.Args) > 1 && os.Args[1] == "-w"

	// Load the Go types.
	cfg := &packages.Config{
		Mode: packages.NeedTypes | packages.NeedSyntax | packages.NeedCompiledGoFiles,
		Dir:  cwd(),
	}
	pkgs, err := packages.Load(cfg, "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1")
	if err != nil || len(pkgs) == 0 {
		fmt.Fprintf(os.Stderr, "crd-doc-gen: failed to load package: %v\n", err)
		os.Exit(1)
	}

	// Read existing file.
	target := "docs/reference/crds.md"
	content := ""
	if b, err := os.ReadFile(target); err == nil {
		content = string(b)
	}

	// Extract structs.
	pkg := pkgs[0]
	structs := extractFromPackage(pkg)

	// For each marker pair, replace the content with a fresh table.
	out := content
	for _, s := range structs {
		start := "<!-- crd-gen:start:" + s.name + " -->"
		end := "<!-- crd-gen:end:" + s.name + " -->"
		idxStart := strings.Index(out, start)
		idxEnd := strings.Index(out, end)
		if idxStart < 0 || idxEnd < 0 {
			continue
		}
		replacement := start + "\n" + strings.TrimSpace(renderTable(s)) + "\n" + end
		out = out[:idxStart] + replacement + out[idxEnd+len(end):]
	}

	if writeMode {
		if err := os.WriteFile(target, []byte(out), 0644); err != nil {
			fmt.Fprintf(os.Stderr, "crd-doc-gen: %v\n", err)
			os.Exit(1)
		}
		return
	}
	fmt.Print(out)
}

// --- struct extraction ---------------------------------------------------

type crdField struct {
	name     string
	jsonName string
	typeStr  string
	doc      string
	required bool
}

type crdStruct struct {
	name   string
	isSpec bool
	fields []crdField
}

func extractFromPackage(pkg *packages.Package) []crdStruct {
	var out []crdStruct
	for _, f := range pkg.Syntax {
		for _, decl := range f.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.TYPE {
				continue
			}
			for _, spec := range gen.Specs {
				ts, ok := spec.(*ast.TypeSpec)
				if !ok {
					continue
				}
				st, ok := ts.Type.(*ast.StructType)
				if !ok {
					continue
				}
				name := ts.Name.Name
				if !isCRDStruct(name) {
					continue
				}
				cs := crdStruct{name: name, isSpec: strings.HasSuffix(name, "Spec")}
				cs.fields = fieldsFromStruct(st)
				out = append(out, cs)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

func isCRDStruct(name string) bool {
	switch name {
	case "WorkspaceSpec", "WorkspaceStatus",
		"RuntimeEnvironmentSpec", "RuntimeEnvironmentStatus",
		"InferenceRelaySpec", "InferenceRelayStatus":
		return true
	}
	return false
}

func fieldsFromStruct(st *ast.StructType) []crdField {
	var flds []crdField
	for _, f := range st.Fields.List {
		if len(f.Names) == 0 {
			continue // embedded
		}
		name := f.Names[0].Name
		jsonName := jsonFromTag(f.Tag)
		if jsonName == "-" {
			continue
		}
		typeStr := typeExpr(f.Type)
		doc := fieldDoc(f)
		flds = append(flds, crdField{
			name:     name,
			jsonName: jsonName,
			typeStr:  typeStr,
			doc:      doc,
			required: !strings.Contains(tagValue(f.Tag, "json"), "omitempty"),
		})
	}
	return flds
}

// --- rendering ------------------------------------------------------------

func renderTable(s crdStruct) string {
	var b strings.Builder
	section := "Spec"
	if !s.isSpec {
		section = "Status"
	}
	crdName := strings.TrimSuffix(s.name, section)

	if s.isSpec {
		// The hand-written header already provides Kind/Scope/Short info.
		// Just render the table.
	} else {
		// Status block — let the existing hand-written intro stand.
	}

	b.WriteString("| Field | Type | Required | Description |\n")
	b.WriteString("|---|---|---|---|\n")
	for _, f := range s.fields {
		req := "no"
		if f.required {
			req = "yes"
		}
		desc := f.doc
		if desc == "" {
			desc = "_generated from source — update `" + crdName + section + "." + f.name + "` Go doc comment_"
		}
		b.WriteString(fmt.Sprintf("| `%s` | `%s` | %s | %s |\n",
			f.jsonName, f.typeStr, req, desc))
	}
	return b.String()
}

// --- helpers --------------------------------------------------------------

func jsonFromTag(tag *ast.BasicLit) string {
	v := tagValue(tag, "json")
	if v == "" || v == "-" {
		return "-"
	}
	return strings.Split(v, ",")[0]
}

func tagValue(tag *ast.BasicLit, key string) string {
	if tag == nil {
		return ""
	}
	val := strings.Trim(tag.Value, "`")
	for _, part := range strings.Fields(val) {
		kv := strings.SplitN(part, ":", 2)
		if len(kv) != 2 {
			continue
		}
		if kv[0] == key {
			return strings.Trim(kv[1], `"`)
		}
	}
	return ""
}

func fieldDoc(f *ast.Field) string {
	if f.Doc == nil {
		if f.Comment != nil {
			return cleanDoc(f.Comment.Text())
		}
		return ""
	}
	return cleanDoc(f.Doc.Text())
}

func cleanDoc(s string) string {
	lines := strings.Split(s, "\n")
	var out []string
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "+kubebuilder:") {
			continue
		}
		out = append(out, l)
	}
	return strings.Join(out, " ")
}

func typeExpr(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.StarExpr:
		return "*" + typeExpr(t.X)
	case *ast.SelectorExpr:
		return typeExpr(t.X) + "." + t.Sel.Name
	case *ast.ArrayType:
		if t.Len == nil {
			return "[]" + typeExpr(t.Elt)
		}
		return "[" + typeExpr(t.Len) + "]" + typeExpr(t.Elt)
	case *ast.MapType:
		return "map[" + typeExpr(t.Key) + "]" + typeExpr(t.Value)
	case *ast.BasicLit:
		return t.Value
	default:
		return fmt.Sprintf("%T", t)
	}
}

func cwd() string {
	if d, err := os.Getwd(); err == nil {
		return d
	}
	return "."
}
