// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package abi_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/lenaxia/llmsafespaces/pkg/session"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
)

// TestPkgSessionContractParity is the dual-track guard of the Epic 65
// source-of-truth agreement (ADR design/0056): during S1 the proto schema is
// wire-authoritative for the ABI while pkg/session stays the in-memory
// contract. Two hand-maintained representations of one contract WILL drift —
// this test pins field-name equivalence for every paired type and enum so
// drift fails CI instead of shipping. At the S2 freeze, pkg/session flips to
// generated types and this scaffold is deleted (US-65 story, per the ADR).
type parityPair struct {
	abi string
	go_ any
}

func TestPkgSessionContractParity(t *testing.T) {
	pairs := []parityPair{
		{"Part", session.Part{}},
		{"ToolPart", session.ToolPart{}},
		{"ToolState", session.ToolState{}},
		{"FileDiff", session.FileDiff{}},
		{"CustomPart", session.CustomPart{}},
		{"Message", session.Message{}},
		{"Error", session.Error{}},
		{"Event", session.Event{}},
		{"InputRequest", session.InputRequest{}},
		{"InputOption", session.InputOption{}},
		{"ToolRef", session.ToolRef{}},
		{"Session", session.Session{}},
		{"TimeRange", session.TimeRange{}},
		{"Cost", session.Cost{}},
		{"ContextUsage", session.ContextUsage{}},
		{"ModelRef", session.ModelRef{}},
	}
	for _, p := range pairs {
		t.Run(p.abi, func(t *testing.T) {
			abiJSONNames := messageJSONNames(t, p.abi)
			goJSONNames := structJSONNames(t, p.go_)
			for want := range goJSONNames {
				if !abiJSONNames[want] {
					t.Errorf("pkg/session %s has field %q missing from ABI message %s — contract drift", p.goTypeName(), want, p.abi)
				}
			}
			for got := range abiJSONNames {
				if !goJSONNames[got] {
					t.Errorf("ABI message %s has field %q missing from pkg/session %s — contract drift", p.abi, got, p.goTypeName())
				}
			}
		})
	}

	enumParity(t)
}

func (p parityPair) goTypeName() string {
	return reflect.TypeOf(p.go_).Name()
}

// structJSONNames extracts the JSON tag names from a contract struct.
func structJSONNames(t *testing.T, v any) map[string]bool {
	t.Helper()
	rt := reflect.TypeOf(v)
	out := map[string]bool{}
	for i := 0; i < rt.NumField(); i++ {
		f := rt.Field(i)
		tag := f.Tag.Get("json")
		name := strings.Split(tag, ",")[0]
		if name == "" || name == "-" {
			t.Fatalf("field %s.%s lacks a usable json tag", rt.Name(), f.Name)
		}
		out[name] = true
	}
	return out
}

func messageJSONNames(t *testing.T, name string) map[string]bool {
	t.Helper()
	md := abiMessage(t, name)
	out := map[string]bool{}
	for i := 0; i < md.Fields().Len(); i++ {
		fd := md.Fields().Get(i)
		out[fd.JSONName()] = true
	}
	return out
}

func abiMessage(t *testing.T, name string) protoreflect.MessageDescriptor {
	t.Helper()
	d, err := protoregistry.GlobalFiles.FindDescriptorByName(abiPackageName.Append(protoreflect.Name(name)))
	if err != nil {
		t.Fatalf("ABI message %s not found: %v", name, err)
	}
	md, ok := d.(protoreflect.MessageDescriptor)
	if !ok {
		t.Fatalf("%s is not a message", name)
	}
	return md
}

// enumParity pins the closed value sets using the documented mapping rules:
// proto enum value name → lowercased, and for EventType '_' → '.' (the
// contract's dotted wire strings; the opencode adapter performs this exact
// translation when emitting contract events). The contract constants are
// referenced explicitly — reflection cannot enumerate them — so a new
// constant on either side without its counterpart fails here.
func enumParity(t *testing.T) {
	t.Helper()
	cases := []struct {
		abiEnum  string
		goValues map[string]bool
		dotted   bool
	}{
		{"SessionStatus", setOf(
			string(session.StatusUnknown), string(session.StatusIdle),
			string(session.StatusBusy), string(session.StatusError),
			string(session.StatusCompacting), string(session.StatusArchived)), false},
		{"MessageType", setOf(
			string(session.MessageUser), string(session.MessageAssistant),
			string(session.MessageShell), string(session.MessageAgentSwitch),
			string(session.MessageModelSwitch), string(session.MessageCompaction),
			string(session.MessageSystem)), false},
		{"PartType", setOf(
			string(session.PartText), string(session.PartReasoning),
			string(session.PartTool), string(session.PartFileChange),
			string(session.PartCustom)), false},
		{"ToolStatus", setOf(
			string(session.ToolStatusPending), string(session.ToolStatusRunning),
			string(session.ToolStatusCompleted), string(session.ToolStatusError)), false},
		{"ChangeStatus", setOf(
			string(session.ChangeAdded), string(session.ChangeModified),
			string(session.ChangeDeleted), string(session.ChangeRenamed)), false},
		{"InputKind", setOf(
			string(session.InputQuestion), string(session.InputPermission)), false},
		{"EventType", setOf(
			string(session.EventSessionStatus), string(session.EventSessionUpdated),
			string(session.EventMessageStart), string(session.EventMessageEnd),
			string(session.EventPartStart), string(session.EventPartDelta),
			string(session.EventPartEnd), string(session.EventInputRequest),
			string(session.EventInputResolved), string(session.EventError)), true},
	}
	for _, c := range cases {
		t.Run(c.abiEnum, func(t *testing.T) {
			prefix := abiEnumPrefix(t, c.abiEnum)
			abiValues := abiEnumNames(t, c.abiEnum)
			canon := map[string]bool{}
			for name := range abiValues {
				canon[canonicalEnumValue(prefix, name, c.dotted)] = true
			}
			for goValue := range c.goValues {
				if !canon[goValue] {
					t.Errorf("pkg/session enum value %q has no ABI counterpart in %s", goValue, c.abiEnum)
				}
			}
			for canonical := range canon {
				if !c.goValues[canonical] {
					t.Errorf("ABI enum %s value %q has no pkg/session counterpart", c.abiEnum, canonical)
				}
			}
		})
	}
}

// canonicalEnumValue maps an ABI enum value name to the contract's wire
// string: strip the enum's value prefix, lowercase, and use dots for the
// dotted event set (EVENT_TYPE_SESSION_STATUS → session.status).
func canonicalEnumValue(prefix, valueName string, dotted bool) string {
	stripped := strings.TrimPrefix(valueName, prefix)
	lower := strings.ToLower(stripped)
	if dotted {
		return strings.ReplaceAll(lower, "_", ".")
	}
	return lower
}

func setOf(values ...string) map[string]bool {
	out := map[string]bool{}
	for _, v := range values {
		out[v] = true
	}
	return out
}

// abiEnumPrefix derives the enum's value prefix from its zero value: every
// enum's zero value is <PREFIX>_UNSPECIFIED, so the prefix is everything up
// to and including the final underscore. CamelCase-to-underscore conversion
// (InputKind → INPUT_KIND) makes naive ToUpper wrong here.
func abiEnumPrefix(t *testing.T, name string) string {
	t.Helper()
	d, err := protoregistry.GlobalFiles.FindDescriptorByName(abiPackageName.Append(protoreflect.Name(name)))
	if err != nil {
		t.Fatalf("ABI enum %s not found: %v", name, err)
	}
	ed, ok := d.(protoreflect.EnumDescriptor)
	if !ok {
		t.Fatalf("%s is not an enum", name)
	}
	zero := ed.Values().Get(0)
	prefix, found := strings.CutSuffix(string(zero.Name()), "UNSPECIFIED")
	if !found {
		t.Fatalf("enum %s zero value %q does not end in UNSPECIFIED", name, zero.Name())
	}
	return prefix
}

func abiEnumNames(t *testing.T, name string) map[string]bool {
	t.Helper()
	d, err := protoregistry.GlobalFiles.FindDescriptorByName(abiPackageName.Append(protoreflect.Name(name)))
	if err != nil {
		t.Fatalf("ABI enum %s not found: %v", name, err)
	}
	ed, ok := d.(protoreflect.EnumDescriptor)
	if !ok {
		t.Fatalf("%s is not an enum", name)
	}
	out := map[string]bool{}
	for i := 0; i < ed.Values().Len(); i++ {
		vd := ed.Values().Get(i)
		if vd.Number() == 0 {
			continue
		}
		out[string(vd.Name())] = true
	}
	return out
}
