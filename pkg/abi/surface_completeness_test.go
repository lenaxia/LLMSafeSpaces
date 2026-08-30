// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package abi_test

import (
	"testing"

	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
)

// abiPackageName is the proto package whose surface these contract tests pin.
const abiPackageName = protoreflect.FullName("llmsafespaces.abi.v1")

// TestSchemaSurfaceCompleteness proves the ABI cannot silently shrink (issue
// #1135 test plan: schema_surface_completeness). It enumerates the generated
// descriptors and asserts they cover exactly the five design-0055 M1/M2
// operations and every supporting message, and that the closed unions (part
// types, ledger states, event types) stay closed.
func TestSchemaSurfaceCompleteness(t *testing.T) {
	files := abiFiles(t)

	wantMessages := map[string]bool{
		// Op 1 — Events: snapshot frame + ordered events + reseed notice.
		"EventsRequest": false, "StreamFrame": false, "SnapshotFrame": false,
		"PodSnapshot": false, "SequencedEvent": false, "ReseedNotice": false,
		// Op 2 — Snapshot.
		"GetSnapshotRequest": false, "SessionSnapshot": false,
		// Op 3 — Deliver (parts-capable, D3).
		"DeliveryRequest": false, "DeliveryPart": false, "FileReference": false,
		"DeliveryAck": false,
		// Op 4 — Delivery status (ledger lookup).
		"GetDeliveryStatusRequest": false, "DeliveryStatus": false,
		// Op 5 — Typed actions.
		"ActionRequest": false, "InterruptAction": false,
		"SwitchModelAction": false, "SwitchAgentAction": false,
		"AnswerInputAction": false, "CompactAction": false,
		"ActionResult": false, "InterruptResult": false,
		"SwitchModelResult": false, "SwitchAgentResult": false,
		"AnswerInputResult": false, "CompactResult": false,
		// Capability report (rides the snapshot frame; provenance per M4).
		"CapabilityReport": false, "NotSupported": false,
		// Opaque cursor + history (defined in the ABI, wired at S5).
		"Cursor": false, "HistoryRequest": false, "HistoryPage": false,
		// Epic 65 contract types (pkg/session parity).
		"Session": false, "TimeRange": false, "Cost": false,
		"ContextUsage": false, "ModelRef": false,
		"Message": false, "Error": false,
		"Part": false, "ToolPart": false, "ToolState": false,
		"FileDiff": false, "CustomPart": false,
		"Event": false, "InputRequest": false, "InputOption": false,
		"ToolRef": false,
	}

	services := map[string]bool{}
	for _, fd := range files {
		for i := 0; i < fd.Messages().Len(); i++ {
			name := string(fd.Messages().Get(i).Name())
			seen, listed := wantMessages[name]
			if !listed {
				t.Errorf("schema declares message %q outside the reviewed surface — extend the review or remove the message", name)
				continue
			}
			if seen {
				t.Errorf("message %q declared more than once in package %s", name, abiPackageName)
			}
			wantMessages[name] = true
		}
		for i := 0; i < fd.Services().Len(); i++ {
			services[string(fd.Services().Get(i).Name())] = true
		}
	}
	for name, seen := range wantMessages {
		if !seen {
			t.Errorf("schema is missing message %q required by design 0055 M1/M2 — the ABI shrank", name)
		}
	}

	if len(services) != 1 || !services["HarnessABIService"] {
		t.Errorf("package %s must declare exactly one service HarnessABIService, got %v", abiPackageName, services)
	}

	assertMethodSet(t, files)
	assertClosedUnions(t, files)
}

func abiFiles(t *testing.T) []protoreflect.FileDescriptor {
	t.Helper()
	var files []protoreflect.FileDescriptor
	protoregistry.GlobalFiles.RangeFiles(func(fd protoreflect.FileDescriptor) bool {
		if fd.Package() == abiPackageName {
			files = append(files, fd)
		}
		return true
	})
	if len(files) == 0 {
		t.Fatal("no generated descriptors registered for package llmsafespaces.abi.v1 — codegen output missing")
	}
	return files
}

// assertMethodSet pins the frozen five-operation surface: nothing more,
// nothing less, and Events is the only streaming op.
func assertMethodSet(t *testing.T, files []protoreflect.FileDescriptor) {
	t.Helper()
	svc := findService(t, files, "HarnessABIService")
	got := map[string]bool{}
	for i := 0; i < svc.Methods().Len(); i++ {
		md := svc.Methods().Get(i)
		got[string(md.Name())] = true
		switch md.Name() {
		case "Events":
			if !md.IsStreamingServer() || md.IsStreamingClient() {
				t.Errorf("Events must be server-streaming (snapshot frame, then live)")
			}
		case "GetSnapshot", "Deliver", "GetDeliveryStatus", "Act":
			if md.IsStreamingClient() || md.IsStreamingServer() {
				t.Errorf("%s must be unary", md.Name())
			}
		default:
			t.Errorf("service HarnessABIService gained method %q beyond the frozen five ops — design amendment required", md.Name())
		}
	}
	for _, want := range []string{"Events", "GetSnapshot", "Deliver", "GetDeliveryStatus", "Act"} {
		if !got[want] {
			t.Errorf("service HarnessABIService lost method %q — the ABI shrank", want)
		}
	}
}

// assertClosedUnions pins the "5 part types forever" rule and the M2 ledger
// state machine against enum drift.
func assertClosedUnions(t *testing.T, files []protoreflect.FileDescriptor) {
	t.Helper()

	partValues := enumNonZeroNames(t, files, "PartType")
	wantParts := map[string]bool{"PART_TYPE_TEXT": true, "PART_TYPE_REASONING": true, "PART_TYPE_TOOL": true, "PART_TYPE_FILE_CHANGE": true, "PART_TYPE_CUSTOM": true}
	if !equalSets(partValues, wantParts) {
		t.Errorf("PartType union must stay closed at the 5 Epic 65 part types (design 0049 §4.1 rule 1), got %v", partValues)
	}

	ledgerValues := enumNonZeroNames(t, files, "LedgerState")
	wantLedger := map[string]bool{"LEDGER_STATE_LEDGERED": true, "LEDGER_STATE_ADMITTED": true, "LEDGER_STATE_PROMOTED": true, "LEDGER_STATE_TURN_ENDED": true, "LEDGER_STATE_STALLED": true, "LEDGER_STATE_FAILED": true}
	if !equalSets(ledgerValues, wantLedger) {
		t.Errorf("LedgerState must match the M2 state machine exactly, got %v", ledgerValues)
	}

	eventValues := enumNonZeroNames(t, files, "EventType")
	wantEvents := map[string]bool{
		"EVENT_TYPE_SESSION_STATUS": true, "EVENT_TYPE_SESSION_UPDATED": true, "EVENT_TYPE_MESSAGE_START": true,
		"EVENT_TYPE_MESSAGE_END": true, "EVENT_TYPE_PART_START": true, "EVENT_TYPE_PART_DELTA": true,
		"EVENT_TYPE_PART_END": true, "EVENT_TYPE_INPUT_REQUEST": true, "EVENT_TYPE_INPUT_RESOLVED": true,
		"EVENT_TYPE_ERROR": true,
	}
	if !equalSets(eventValues, wantEvents) {
		t.Errorf("EventType must match the pinned Epic 65 event set exactly, got %v", eventValues)
	}

	actionValues := enumNonZeroNames(t, files, "ActionType")
	wantActions := map[string]bool{"ACTION_TYPE_INTERRUPT": true, "ACTION_TYPE_SWITCH_MODEL": true, "ACTION_TYPE_SWITCH_AGENT": true, "ACTION_TYPE_ANSWER_QUESTION": true, "ACTION_TYPE_COMPACT": true}
	if !equalSets(actionValues, wantActions) {
		t.Errorf("ActionType must match the M1 op-5 union exactly, got %v", actionValues)
	}
}

func findService(t *testing.T, files []protoreflect.FileDescriptor, name string) protoreflect.ServiceDescriptor {
	t.Helper()
	for _, fd := range files {
		if sd := fd.Services().ByName(protoreflect.Name(name)); sd != nil {
			return sd
		}
	}
	t.Fatalf("service %s not found in package %s", name, abiPackageName)
	return nil
}

func enumNonZeroNames(t *testing.T, files []protoreflect.FileDescriptor, name string) map[string]bool {
	t.Helper()
	for _, fd := range files {
		if ed := fd.Enums().ByName(protoreflect.Name(name)); ed != nil {
			out := map[string]bool{}
			for i := 0; i < ed.Values().Len(); i++ {
				vd := ed.Values().Get(i)
				if vd.Number() != 0 {
					out[string(vd.Name())] = true
				}
			}
			return out
		}
	}
	t.Fatalf("enum %s not found in package %s", name, abiPackageName)
	return nil
}

func equalSets(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}
