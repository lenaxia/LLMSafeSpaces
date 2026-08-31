// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

// Command sessiongen generates pkg/session's contract types from the
// frozen ABI schema (pkg/abi/llmsafespaces/abi/v1/contract.proto).
//
// ADR 0056 T3 / issue #1161: the schema is the single source of truth for
// the session contract; pkg/session is its dialect-preserving Go view
// (string enums, flat Part, time.Time, int) that the API's JSON egress
// (SSE/REST) serves until the S3 frontend cutover — US-69.10/.11 deletes
// this view. The output is committed and verified fresh by `make
// abi-check`, so the two representations cannot drift: a schema change
// regenerates this file or fails CI.
//
// The translation rules (the same ones the S1 parity test asserted):
//   - enum values: strip the enum prefix, lowercase; EventType additionally
//     maps `_`→`.` (the dotted wire set); *_UNSPECIFIED is dropped.
//   - google.protobuf.Timestamp: plain → time.Time, `optional` → *time.Time.
//   - message fields → pointers, except the VALUE_FIELDS table.
//   - int32 → int (`optional` int32 → *int); bytes → json.RawMessage.
//   - oneof → flat fields on the parent (single-field wrappers flatten).
//   - json tags: proto JSON names; omitempty everywhere except the
//     REQUIRED_FIELDS table (proto3 cannot express required-ness).
package main

import (
	"bytes"
	"flag"
	"fmt"
	"go/format"
	"os"
	"strings"

	"google.golang.org/protobuf/reflect/protoreflect"

	abi "github.com/lenaxia/llmsafespaces/pkg/abi/v1"
)

// enumConstPrefix maps an enum to the constant-name prefix its session-side
// constants carry (historical naming: ToolStatusPending but ChangeAdded).
var enumConstPrefix = map[string]string{
	"SessionStatus": "Status",
	"PartType":      "Part",
	"ToolStatus":    "ToolStatus",
	"ChangeStatus":  "Change",
	"MessageType":   "Message",
	"InputKind":     "Input",
	"EventType":     "Event",
}

// enumTypeName renames an enum for the session view: SessionStatus's
// session-side type is the historical short name Status.
var enumTypeName = map[string]string{"SessionStatus": "Status"}

// dottedEnums are the enum sets whose wire values use dots (event names).
var dottedEnums = map[string]bool{"EventType": true}

// valueMessageFields lists message-typed fields rendered as Go values (not
// pointers). ToolPart.state is embedded by value: the state always exists.
var valueMessageFields = map[string]bool{"ToolPart.state": true}

// requiredFields lists fields whose json tag carries NO omitempty — the
// semantically-required set proto3 cannot express.
var requiredFields = map[string]map[string]bool{
	"Session":      {"id": true, "workspace_id": true, "status": true},
	"TimeRange":    {"started_at": true},
	"ContextUsage": {"used": true},
	"ModelRef":     {"id": true},
	"Part":         {"type": true},
	"ToolState":    {"status": true},
	"ToolPart":     {"name": true, "state": true},
	"FileDiff":     {"path": true, "status": true, "patch": true},
	"CustomPart":   {"kind": true},
	"Message":      {"id": true, "type": true},
	"Error":        {"message": true},
	"InputOption":  {"label": true},
	"InputRequest": {"id": true, "kind": true},
	"Event":        {"type": true, "timestamp": true},
}

// docComments carries the Go-view documentation for each generated
// declaration. The embedded descriptor has no source info (protoc-gen-go
// strips it), so the session view's docs are maintained here — keyed by
// the proto declaration name, rendered under the session-side type name.
var docComments = map[string]string{
	"SessionStatus": "Status is the lifecycle state of an agent session.",
	"TimeRange":     "TimeRange bounds a session or message. CompletedAt is nil while busy.",
	"Cost":          "Cost is display-only token/cost data. Billing is cgroup-based; these fields are never authoritative for metering (design 0049 §4.1 rule 5).",
	"ContextUsage": "ContextUsage is the session's live context occupancy — the numerator for the\n" +
		"\"context: 45% used\" display that ModelInfo.ContextWindow denominates. Used is\n" +
		"semantic (tokens of window currently occupied), computed by the adapter from\n" +
		"the agent's own accounting conventions; raw token ledgers live in Cost.\n" +
		"Non-monotonic by design: compaction resets it.",
	"ModelRef":   "ModelRef identifies a model an adapter selected for a session or message.",
	"Session":    "Session is the platform-owned view of one agent session. It is the unit a client lists, opens, and renders. Agent/model switches and compaction are carried as Message transcript entries, not side-band fields here.",
	"PartType":   "PartType discriminates the closed part union. The union is capped at 5 forever (design 0049 §4.1 rule 1); adding a type is a contract change.",
	"Part":       "Part is one typed piece of an assistant message. Exactly one payload field is set, matching Type; the rest are omitted from the wire form.",
	"ToolStatus": "ToolStatus is the tool-call state-machine value (design 0049 §4.3): pending -> running -> completed | error.",
	"ToolState":  "ToolState is the lifecycle state of one tool call, separate from the call's identity and input/output.",
	"ToolPart": "ToolPart is the payload of a Tool part. Every tool call — bash, edit, read, grep,\n" +
		"todos, plan mode, subagent spawn — is a ToolPart discriminated by Name (design\n" +
		"0049 §4.1 rule 2). Input/Output are raw JSON because tool schemas are open-ended;\n" +
		"adapters and renderers decode them per Name.",
	"ChangeStatus": "ChangeStatus classifies one file's change in a FileDiff.",
	"FileDiff": "FileDiff is the payload of a FileChange part: one file's unified diff. Patch is\n" +
		"authoritative unified-diff text (design 0049 §4.1 rule 4) — renderers (GitHub,\n" +
		"monaco-diff, terminal) all consume it directly; no hunk structs.",
	"CustomPart": "CustomPart is the pressure-relief valve for extension-defined semantics (design\n" +
		"0049 §4.3). Kind is a required discriminator so extensions do not force new\n" +
		"PartType constants; Data carries the extension-specific payload.",
	"MessageType": "MessageType discriminates a Message. Agent/model switches and compaction are transcript entries (not side-band config) so the timeline stays coherent after a switch (design 0049 §4.5).",
	"Message": "Message is one entry in a session transcript. It is a flat discriminated\n" +
		"struct: Type selects which payload fields are meaningful, and the rest are\n" +
		"omitted from the wire form. Constructors are the documented write path so\n" +
		"the Type<->field pairing is encoded in one place.",
	"Error": "Error is the payload of an error Event (and an assistant Message's Error\n" +
		"field). It is deliberately NOT a PartType: the part union is capped at 5\n" +
		"forever (design 0049 §4.1 rule 1); errors flow through the error Event.",
	"EventType": "EventType discriminates a streaming Event. The values are pinned in event_test.go's TestEventTypeCountMatchesExplicitList.",
	"InputKind": "InputKind unifies questions and permissions: both are \"the agent needs a\n" +
		"human\" (design 0049 §4.5).",
	"InputOption": "InputOption is one selectable choice within a question InputRequest.",
	"ToolRef":     "ToolRef identifies the tool call that triggered an InputRequest, if any.",
	"InputRequest": "InputRequest is the unified pending-input shape. Question-specific fields\n" +
		"apply when Kind == InputQuestion; permission-specific fields when\n" +
		"Kind == InputPermission. Metadata values are raw JSON because permission\n" +
		"metadata is open-ended extension data, not a known shape.",
	"Event": "Event is one item on a session's streaming event stream. Type selects which\n" +
		"payload fields are meaningful; the rest are omitted.",
}

// fieldOrderOverride reorders plain (non-oneof) fields; oneof members are
// appended after, in schema order. Part puts its discriminator first.
var fieldOrderOverride = map[string][]string{"Part": {"type", "id"}}

const timestampFull = "google.protobuf.Timestamp"

func main() {
	out := flag.String("out", "pkg/session/contract_gen.go", "output file")
	flag.Parse()

	fd := abi.File_llmsafespaces_abi_v1_contract_proto
	if fd == nil {
		fmt.Fprintln(os.Stderr, "sessiongen: contract.proto descriptor not linked")
		os.Exit(1)
	}

	var b bytes.Buffer
	b.WriteString("// Copyright (C) 2026 Michael Kao\n")
	b.WriteString("// SPDX-License-Identifier: AGPL-3.0-or-later\n\n")
	b.WriteString("// Code generated by cmd/sessiongen from the frozen ABI schema\n")
	b.WriteString("// (pkg/abi/llmsafespaces/abi/v1/contract.proto). DO NOT EDIT;\n")
	b.WriteString("// regenerate with `make abi-generate` (ADR 0056 T3, issue #1161).\n\n")
	b.WriteString("package session\n\n")
	b.WriteString("import (\n\t\"encoding/json\"\n\t\"time\"\n)\n")

	for i := 0; i < fd.Enums().Len(); i++ {
		writeEnum(&b, fd, fd.Enums().Get(i))
	}
	for i := 0; i < fd.Messages().Len(); i++ {
		writeMessage(&b, fd, fd.Messages().Get(i))
	}

	src, err := format.Source(b.Bytes())
	if err != nil {
		fmt.Fprintf(os.Stderr, "sessiongen: gofmt: %v\nsource:\n%s\n", err, b.String())
		os.Exit(1)
	}
	if err := os.WriteFile(*out, src, 0o644); err != nil { //nolint:gosec // G306: a committed source file — git normalizes perms on checkout
		fmt.Fprintf(os.Stderr, "sessiongen: write %s: %v\n", *out, err)
		os.Exit(1)
	}
	fmt.Printf("sessiongen: wrote %s (%d bytes)\n", *out, len(src))
}

func writeEnum(b *bytes.Buffer, fd protoreflect.FileDescriptor, e protoreflect.EnumDescriptor) {
	protoName := string(e.Name())
	name := protoName
	if ren, ok := enumTypeName[protoName]; ok {
		name = ren
	}
	writeDocComment(b, name, protoName)
	fmt.Fprintf(b, "type %s string\n\n", name)

	prefix, ok := enumConstPrefix[protoName]
	if !ok {
		fmt.Fprintf(os.Stderr, "sessiongen: no constant prefix for enum %s\n", protoName)
		os.Exit(1)
	}
	valuePrefix := screamingSnake(protoName) + "_"

	b.WriteString("const (\n")
	for i := 0; i < e.Values().Len(); i++ {
		v := e.Values().Get(i)
		stripped := strings.TrimPrefix(string(v.Name()), valuePrefix)
		if stripped == "UNSPECIFIED" {
			continue // the proto zero value; session's sets start at the first real value
		}
		wire := strings.ToLower(stripped)
		if dottedEnums[protoName] {
			wire = strings.ReplaceAll(wire, "_", ".")
		}
		fmt.Fprintf(b, "\t%s%s %s = %q\n", prefix, constNamePart(stripped), name, wire)
	}
	b.WriteString(")\n\n")
}

// screamingSnake converts a CamelCase name to ITS_PROTO_CONVENTION
// (SessionStatus -> SESSION_STATUS), the prefix of its value names.
func screamingSnake(name string) string {
	var b strings.Builder
	for i, r := range name {
		if i > 0 && r >= 'A' && r <= 'Z' {
			b.WriteByte('_')
		}
		b.WriteRune(r)
	}
	return strings.ToUpper(b.String())
}

func writeMessage(b *bytes.Buffer, fd protoreflect.FileDescriptor, m protoreflect.MessageDescriptor) {
	name := string(m.Name())
	writeDocComment(b, name, name)
	fmt.Fprintf(b, "type %s struct {\n", name)

	// Oneof partition: proto3 `optional` fields sit in SYNTHETIC oneofs at
	// the descriptor level — only real oneofs flatten.
	var plain, oneof []protoreflect.FieldDescriptor
	for i := 0; i < m.Fields().Len(); i++ {
		f := m.Fields().Get(i)
		if od := f.ContainingOneof(); od != nil && !od.IsSynthetic() {
			oneof = append(oneof, f)
		} else {
			plain = append(plain, f)
		}
	}
	if override, ok := fieldOrderOverride[name]; ok {
		byName := map[string]protoreflect.FieldDescriptor{}
		for _, f := range plain {
			byName[string(f.Name())] = f
		}
		ordered := make([]protoreflect.FieldDescriptor, 0, len(plain))
		for _, n := range override {
			if f, ok := byName[n]; ok {
				ordered = append(ordered, f)
				delete(byName, n)
			}
		}
		for _, f := range plain { // schema order for anything not in the override
			if _, still := byName[string(f.Name())]; still {
				ordered = append(ordered, f)
			}
		}
		plain = ordered
	}

	req := requiredFields[name]
	for _, f := range append(plain, oneof...) {
		goName := goFieldName(string(f.Name()))
		tag := f.JSONName()
		if !req[string(f.Name())] {
			tag += ",omitempty"
		}
		fmt.Fprintf(b, "\t%s %s `json:\"%s\"`\n", goName, fieldGoType(name, f), tag)
	}
	b.WriteString("}\n\n")
}

// fieldGoType renders the session-side Go type of one schema field.
func fieldGoType(parent string, f protoreflect.FieldDescriptor) string {
	if f.IsMap() {
		return "map[string]" + fieldBaseType(parent, f.MapValue())
	}
	if f.IsList() {
		return "[]" + fieldBaseType(parent, f)
	}
	return fieldBaseType(parent, f)
}

// fieldBaseType renders the element/singular Go type (no slice/map
// wrapping): repeated messages become value slices, singular ones pointers.
func fieldBaseType(parent string, f protoreflect.FieldDescriptor) string {
	switch {
	case f.Kind() == protoreflect.MessageKind && string(f.Message().FullName()) == timestampFull:
		if f.HasOptionalKeyword() {
			return "*time.Time"
		}
		return "time.Time"
	case f.Kind() == protoreflect.MessageKind:
		base := string(f.Message().Name())
		if f.IsList() || valueMessageFields[parent+"."+string(f.Name())] {
			return base // repeated messages are value slices ([]Part)
		}
		return "*" + base
	case f.Kind() == protoreflect.EnumKind:
		if ren, ok := enumTypeName[string(f.Enum().Name())]; ok {
			return ren
		}
		return string(f.Enum().Name())
	default:
		s := scalarGo(f.Kind())
		if f.HasOptionalKeyword() && !f.IsList() {
			return "*" + s
		}
		return s
	}
}

func goFieldName(name string) string {
	parts := strings.Split(name, "_")
	for i, p := range parts {
		switch p {
		case "id", "usd", "api", "url", "sse", "mcp":
			parts[i] = strings.ToUpper(p)
		default:
			parts[i] = strings.ToUpper(p[:1]) + p[1:]
		}
	}
	return strings.Join(parts, "")
}

func constNamePart(stripped string) string {
	parts := strings.Split(stripped, "_")
	for i, p := range parts {
		if p == "" {
			continue
		}
		parts[i] = strings.ToUpper(p[:1]) + strings.ToLower(p[1:])
	}
	return strings.Join(parts, "")
}

func scalarGo(k protoreflect.Kind) string {
	switch k {
	case protoreflect.StringKind:
		return "string"
	case protoreflect.BoolKind:
		return "bool"
	case protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind:
		return "int64"
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind:
		return "int"
	case protoreflect.DoubleKind:
		return "float64"
	case protoreflect.BytesKind:
		return "json.RawMessage"
	default:
		return fmt.Sprintf("/* kind %v */", k)
	}
}

// writeDocComment emits the session-view doc comment for a declaration
// (single blank-comment-line separation from the table in this file).
func writeDocComment(b *bytes.Buffer, sessionName, protoName string) {
	doc, ok := docComments[protoName]
	if !ok {
		return
	}
	for _, line := range strings.Split(doc, "\n") {
		b.WriteString("// ")
		b.WriteString(line)
		b.WriteString("\n")
	}
}
