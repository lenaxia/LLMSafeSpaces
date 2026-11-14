// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package pi

// driftgate_test.go — the S6 drift gates (scoping report §7, story
// split S6): the surface manifest (testdata/rpc_surface.json) is the
// recorded RPC surface of the PINNED pi version, and these tests make
// it load-bearing in BOTH directions:
//
//   - TOLERANCE (adapter direction): unknown fields on every recorded
//     response decode cleanly through the real decode paths — pi's own
//     consumer posture ("treat unknown values as normal usage rather
//     than rejecting them", session-format.md), which the absence of a
//     protocol-version handshake makes mandatory (a newer pinned
//     binary adds fields; this reader must not care).
//   - DRIFT DETECTION (fixture direction): a refreshed fixture that
//     drops, adds, or renames a surface FAILS here — the manifest must
//     be updated consciously in the same change. This settles U-8
//     (upstream removals/renames become loud, not silent zeroing).
//
// The mechanical half of the gate (a PI_VERSION bump without ANY
// testdata change fails CI before these tests even run) lives in
// scripts/pi-bump-gate.sh, wired into ci.yml next to the opencode
// gate; pi-version-bump.yml opens bump PRs pointing at REFRESH.md.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/lenaxia/llmsafespaces/pkg/session"
)

// --- Manifest model ------------------------------------------------------

type surfaceManifest struct {
	PiVersion string           `json:"piVersion"`
	Captured  string           `json:"capturedAt"`
	Commands  []surfaceCommand `json:"commands"`
	Responses []surfaceResp    `json:"responses"`
	Events    []surfaceEvent   `json:"events"`
}

type surfaceCommand struct {
	Name           string   `json:"name"`
	Fields         []string `json:"fields"`
	OptionalFields []string `json:"optionalFields,omitempty"`
}

type surfaceResp struct {
	Command            string   `json:"command"`
	Recorded           bool     `json:"recorded"`
	DataFields         []string `json:"dataFields"`
	OptionalDataFields []string `json:"optionalDataFields,omitempty"`
}

type surfaceEvent struct {
	Type           string   `json:"type"`
	FixturePresent bool     `json:"fixturePresent"`
	RequiredFields []string `json:"requiredFields"`
}

func loadSurfaceManifest(t *testing.T) *surfaceManifest {
	t.Helper()
	raw, err := os.ReadFile("testdata/rpc_surface.json")
	if err != nil {
		t.Fatalf("read rpc_surface.json: %v", err)
	}
	m := &surfaceManifest{}
	if err := json.Unmarshal(raw, m); err != nil {
		t.Fatalf("rpc_surface.json is not valid JSON: %v", err)
	}
	if len(m.Commands) == 0 || len(m.Events) == 0 || len(m.Responses) == 0 {
		t.Fatal("rpc_surface.json missing commands/responses/events")
	}
	return m
}

// TestDriftGate_ManifestPinnedToOverlay — the manifest's piVersion is
// the overlay pin (runtimes/pi/Dockerfile ARG PI_VERSION). A bump that
// refreshes fixtures but not the manifest (or vice versa) fails here,
// in-package, even when the CI diff-gate was satisfied by an unrelated
// testdata touch.
func TestDriftGate_ManifestPinnedToOverlay(t *testing.T) {
	m := loadSurfaceManifest(t)
	raw, err := os.ReadFile("../../../runtimes/pi/Dockerfile")
	if err != nil {
		t.Fatalf("read runtimes/pi/Dockerfile (the pin the manifest must track): %v", err)
	}
	pin := ""
	for _, line := range strings.Split(string(raw), "\n") {
		if v, ok := strings.CutPrefix(line, "ARG PI_VERSION="); ok {
			pin = strings.TrimSpace(v)
			break
		}
	}
	if pin == "" {
		t.Fatal("no ARG PI_VERSION in runtimes/pi/Dockerfile")
	}
	if m.PiVersion != pin {
		t.Fatalf("rpc_surface.json piVersion %q != overlay pin %q — the manifest and the pin must move together (REFRESH.md)", m.PiVersion, pin)
	}
}

// TestDriftGate_ManifestSyncedWithTaxonomy — the manifest's event set
// IS knownEventTypes: a taxonomy entry without a manifest row (or the
// reverse) is an unrecorded surface decision.
func TestDriftGate_ManifestSyncedWithTaxonomy(t *testing.T) {
	m := loadSurfaceManifest(t)
	manifest := map[string]bool{}
	for _, e := range m.Events {
		if manifest[e.Type] {
			t.Errorf("duplicate manifest event %q", e.Type)
		}
		manifest[e.Type] = true
	}
	for typ := range knownEventTypes {
		if !manifest[typ] {
			t.Errorf("knownEventTypes entry %q missing from rpc_surface.json — record it (fixturePresent per the capture)", typ)
		}
	}
	for typ := range manifest {
		if !knownEventTypes[typ] {
			t.Errorf("manifest event %q not in knownEventTypes — the taxonomy and the manifest must move together", typ)
		}
	}
}

// TestDriftGate_FixtureCoverageBidirectional — the recorded event
// stream and the manifest agree both ways: every fixture type is
// recorded, every fixturePresent:true type is actually captured
// (a refreshed capture that LOST a type fails until the row is
// consciously flipped — upstream removal, the U-8 case), and a
// fixturePresent:false row stays doc-derived until a capture proves
// it (flipping the flag on is the conscious act).
func TestDriftGate_FixtureCoverageBidirectional(t *testing.T) {
	m := loadSurfaceManifest(t)
	fixtureTypes := fixtureEventTypeSet(t)
	for typ := range fixtureTypes {
		if !knownEventTypes[typ] {
			t.Errorf("fixture event type %q absent from rpc_surface.json/knownEventTypes — a new upstream type must be recorded in the same refresh (drift gate)", typ)
		}
	}
	for _, e := range m.Events {
		captured := fixtureTypes[e.Type]
		if e.FixturePresent && !captured {
			t.Errorf("manifest event %q is fixturePresent:true but the capture no longer carries it — upstream REMOVED it (or renamed): retire the row consciously and note it in REFRESH.md (drift gate)", e.Type)
		}
		if !e.FixturePresent && captured {
			t.Errorf("manifest event %q is fixturePresent:false but the capture carries it — flip the flag and pin its required fields (drift gate)", e.Type)
		}
	}
}

// TestDriftGate_EventRequiredFieldsPresent — every required field the
// platform consumes must be present (non-null) on every captured
// record of that type: an upstream RENAME (usage -> tokenUsage,
// disposition -> delivery, …) surfaces as a missing field here instead
// of silently zeroing the decode (the #739 class).
func TestDriftGate_EventRequiredFieldsPresent(t *testing.T) {
	m := loadSurfaceManifest(t)
	f, err := os.Open("testdata/events_stream.jsonl")
	if err != nil {
		t.Fatalf("open events fixture: %v", err)
	}
	defer f.Close()
	rows := map[string]surfaceEvent{}
	for _, e := range m.Events {
		rows[e.Type] = e
	}
	checked := 0
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var rec map[string]json.RawMessage
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("fixture line not JSON: %s", line)
		}
		var typ string
		if err := json.Unmarshal(rec["type"], &typ); err != nil {
			t.Fatalf("fixture record without a type: %s", line)
		}
		row, ok := rows[typ]
		if !ok {
			continue // coverage test reports the unrecorded type
		}
		for _, field := range row.RequiredFields {
			raw, present := rec[field]
			if !present || string(raw) == "null" {
				t.Errorf("%s record missing required field %q (rename upstream?) — record: %.200s", typ, field, line)
			}
		}
		if len(row.RequiredFields) > 0 {
			checked++
		}
	}
	if checked == 0 {
		t.Fatal("no required-field assertions ran — the manifest must list required fields for consumed event types")
	}
}

// TestDriftGate_RecordedResponsesMatchManifest — the three recorded
// response envelopes (real capture) echo the manifest's command name
// and carry every dataField the typed decoders consume.
func TestDriftGate_RecordedResponsesMatchManifest(t *testing.T) {
	m := loadSurfaceManifest(t)
	byCommand := map[string]surfaceResp{}
	for _, r := range m.Responses {
		byCommand[r.Command] = r
	}
	recorded := map[string]string{
		"get_state":            "testdata/get_state_response.json",
		"get_session_stats":    "testdata/get_session_stats_response.json",
		"get_available_models": "testdata/get_available_models_response.json",
	}
	for cmd, path := range recorded {
		row, ok := byCommand[cmd]
		if !ok {
			t.Errorf("recorded response %s has no rpc_surface.json row — record it", cmd)
			continue
		}
		if !row.Recorded {
			t.Errorf("recorded response %s is marked recorded:false in rpc_surface.json — flip it", cmd)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		var env struct {
			Command string          `json:"command"`
			Success bool            `json:"success"`
			Data    json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(raw, &env); err != nil {
			t.Fatalf("%s not JSON: %v", path, err)
		}
		if env.Command != cmd {
			t.Errorf("%s echoes command %q — upstream renamed the command (the U-8 case): update manifest + adapter consciously", path, env.Command)
		}
		if !env.Success {
			t.Errorf("%s is not a success envelope — the refresh procedure captures success shapes", path)
		}
		var data map[string]json.RawMessage
		if err := json.Unmarshal(env.Data, &data); err != nil {
			t.Fatalf("%s data not an object: %v", path, err)
		}
		for _, field := range row.DataFields {
			raw, present := data[field]
			if !present || string(raw) == "null" {
				t.Errorf("%s data missing field %q (rename upstream?) — update the manifest and the typed decoder together", path, field)
			}
		}
	}
}

// TestDriftGate_ResponseTolerance_UnknownFields — direction one of the
// tolerance contract: every recorded response, with unknown fields
// injected at EVERY level (envelope, data, nested model/tokens), still
// decodes through the real decode paths with identical values. This is
// the runtime posture a protocol-version handshake would otherwise
// guarantee: a newer pinned binary adding fields must not break this
// reader.
func TestDriftGate_ResponseTolerance_UnknownFields(t *testing.T) {
	raw, err := os.ReadFile("testdata/get_state_response.json")
	if err != nil {
		t.Fatal(err)
	}
	// Inject unknowns into the recorded get_state envelope at three
	// levels and decode via the shared envelope + typed structs.
	var env map[string]any
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	env["futureEnvelopeField"] = map[string]any{"x": 1}
	data := env["data"].(map[string]any)
	data["futureDataField"] = []any{"a"}
	if model, ok := data["model"].(map[string]any); ok {
		model["futureModelField"] = 7
	}
	injected, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}

	res := piCallResult{}
	if err := json.Unmarshal(injected, &res); err != nil {
		t.Fatalf("envelope decode rejected unknown fields: %v", err)
	}
	if !res.Success || res.Error != "" {
		t.Fatalf("envelope mis-decoded: %+v", res)
	}
	st := piStateData{}
	if err := decodeInto(res.Data, &st); err != nil {
		t.Fatalf("get_state decode rejected unknown fields: %v", err)
	}
	if st.SessionID != fixtureSessionID || st.MessageCount == 0 || st.Model == nil {
		t.Fatalf("get_state values lost under injection: %+v", st)
	}

	// get_session_stats: unknown nested usage fields (the recorded
	// session file already carries reasoning/totalFromProvider — this
	// pins the same posture on the RESPONSE path).
	raw, err = os.ReadFile("testdata/get_session_stats_response.json")
	if err != nil {
		t.Fatal(err)
	}
	env = map[string]any{}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	env["data"].(map[string]any)["tokens"].(map[string]any)["futureUsageField"] = true
	injected, _ = json.Marshal(env)
	res = piCallResult{}
	if err := json.Unmarshal(injected, &res); err != nil {
		t.Fatal(err)
	}
	stats := piStatsData{}
	if err := decodeInto(res.Data, &stats); err != nil {
		t.Fatalf("get_session_stats decode rejected unknown fields: %v", err)
	}
	if stats.Tokens == nil || stats.Tokens.promptTokens() == 0 || stats.ContextUsage == nil {
		t.Fatalf("stats values lost under injection: %+v", stats)
	}

	// Disposition responses: an unknown sibling of `disposition` (the
	// 2026-09-25 disposition ADDED a field to this response — pi's own
	// history proves the addition class).
	disp := []byte(`{"type":"response","command":"prompt","success":true,"data":{"disposition":"started","futureField":1}}`)
	res = piCallResult{}
	if err := json.Unmarshal(disp, &res); err != nil {
		t.Fatal(err)
	}
	d := piDispositionData{}
	if err := decodeInto(res.Data, &d); err != nil || d.Disposition != "started" {
		t.Fatalf("disposition decode under unknown sibling: %+v err=%v", d, err)
	}
}

// --- Golden command wire -------------------------------------------------

// goldenCommand is one recorded outgoing RPC command with the adapter
// operation that issued it (self-documenting golden).
type goldenCommand struct {
	Op      string         `json:"op"`
	Command map[string]any `json:"command"`
}

// driveGoldenScenario exercises every command builder through the
// public Adapter surface against the fixture transport, in an order
// that keeps cursors deterministic (every get_entries cursor derives
// from the seeded fixture file's leaf ids, never from the global
// fixture id counter).
func driveGoldenScenario(t *testing.T, ft *fixtureTransport) {
	t.Helper()
	a := NewAdapter(ft, nil)
	ctx := context.Background()
	must := func(op string, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", op, err)
		}
	}

	_, err := a.GetSession(ctx, fUserID, fWorkspaceID, fixtureSessionID)
	must("GetSession", err)

	_, err = a.GetHistoryPage(ctx, fUserID, fWorkspaceID, fixtureSessionID, 10)
	must("GetHistoryPage", err)

	msg, err := a.Send(ctx, fUserID, fWorkspaceID, fixtureSessionID, "golden prompt", session.SendOpts{})
	must("Send", err)
	_ = msg

	_, err = a.SendAsync(ctx, fUserID, fWorkspaceID, fixtureSessionID, "golden steer", session.SendOpts{Admission: session.AdmissionSteer})
	must("SendAsync steer", err)

	_, err = a.SendAsync(ctx, fUserID, fWorkspaceID, fixtureSessionID, "golden queue", session.SendOpts{Admission: session.AdmissionQueue})
	must("SendAsync queue", err)

	err = a.SetSessionModel(ctx, fUserID, fWorkspaceID, fixtureSessionID, &session.ModelRef{ID: "fixture/fixture-model"})
	must("SetSessionModel", err)

	ft.mu.Lock()
	ft.drainResponse = []string{"golden requeue"}
	ft.mu.Unlock()
	must("Abort", a.Abort(ctx, fUserID, fWorkspaceID, fixtureSessionID))

	must("RenameSession", a.RenameSession(ctx, fUserID, fWorkspaceID, fixtureSessionID, "golden rename"))

	_, err = a.ListAvailableModels(ctx, fUserID, fWorkspaceID)
	must("ListAvailableModels", err)

	created, err := a.CreateSession(ctx, fUserID, fWorkspaceID, "golden create")
	must("CreateSession", err)
	_ = created
}

// goldenScenarioOps labels each recorded call with the adapter op that
// issued it (the scenario is strictly sequential; the label makes the
// golden reviewable without reading the scenario code).
func goldenScenarioOps(ft *fixtureTransport, calls []fixtureCall) []goldenCommand {
	ops := []string{
		"GetSession.state", "GetSession.stats", "GetHistoryPage.entries",
		"Send.prompt", "Send.entries",
		"SendAsync.steer.steer", "SendAsync.steer.entries",
		"SendAsync.queue.followUp", "SendAsync.queue.entries",
		"SetSessionModel.set_model",
		// Abort drains the queue FIRST (clear_queue), then aborts the
		// in-flight run, then re-queues the drained texts (adapter.go
		// Abort) — labels follow the wire order.
		"Abort.clear_queue", "Abort.abort", "Abort.requeue",
		"RenameSession.set_session_name", "ListAvailableModels.get_available_models",
		"CreateSession.set_session_name", "CreateSession.state",
	}
	out := make([]goldenCommand, 0, len(calls))
	for i, c := range calls {
		op := "unknown"
		if i < len(ops) {
			op = ops[i]
		}
		out = append(out, goldenCommand{Op: op, Command: c.Cmd})
	}
	return out
}

// TestDriftGate_GoldenCommands — the outgoing wire: every RPC command
// the adapter emits across a scripted full-surface scenario, pinned
// exactly (map-order-normalized). A change here is a platform wire
// change OR (after a refresh reveals pi rejected one) an upstream
// command rename — both are review-worthy diffs. Regenerate:
//
//	REGEN_GOLDEN=1 go test -run TestDriftGate_GoldenCommands ./pkg/agent/pi/
func TestDriftGate_GoldenCommands(t *testing.T) {
	ft := newFixtureTransport(t)
	driveGoldenScenario(t, ft)

	ft.mu.Lock()
	calls := append([]fixtureCall(nil), ft.calls...)
	ft.mu.Unlock()
	if len(calls) == 0 {
		t.Fatal("scenario recorded no commands")
	}
	got := goldenScenarioOps(ft, calls)

	goldenPath := "testdata/commands_golden.jsonl"
	if os.Getenv("REGEN_GOLDEN") == "1" {
		var b strings.Builder
		for _, g := range got {
			raw, _ := json.Marshal(g)
			b.Write(raw)
			b.WriteByte('\n')
		}
		if err := os.WriteFile(goldenPath, []byte(b.String()), 0o644); err != nil {
			t.Fatalf("regen golden: %v", err)
		}
		t.Logf("regenerated %s (%d commands)", goldenPath, len(got))
	}

	raw, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden (run with REGEN_GOLDEN=1 to create): %v", err)
	}
	var want []goldenCommand
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var g goldenCommand
		if err := json.Unmarshal([]byte(line), &g); err != nil {
			t.Fatalf("golden line not JSON: %s", line)
		}
		want = append(want, g)
	}
	if len(want) == 0 {
		t.Fatal("golden fixture is empty")
	}
	if !reflect.DeepEqual(normalizeGolden(want), normalizeGolden(got)) {
		gotPretty, _ := json.MarshalIndent(got, "", "  ")
		wantPretty, _ := json.MarshalIndent(want, "", "  ")
		t.Fatalf("outgoing command wire drifted from commands_golden.jsonl.\nwant:\n%s\ngot:\n%s\nIf intentional: REGEN_GOLDEN=1 go test -run TestDriftGate_GoldenCommands ./pkg/agent/pi/ and update rpc_surface.json in the same change.", wantPretty, gotPretty)
	}
}

func normalizeGolden(in []goldenCommand) []goldenCommand {
	out := append([]goldenCommand(nil), in...)
	for i := range out {
		out[i].Command = normalizeMap(out[i].Command)
	}
	return out
}

// normalizeMap copies nested maps so DeepEqual is key-order-insensitive
// (it already is for maps; this future-proofs slice-of-map payloads).
func normalizeMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		if nested, ok := v.(map[string]any); ok {
			out[k] = normalizeMap(nested)
			continue
		}
		out[k] = v
	}
	return out
}

// TestDriftGate_GoldenCommandsMatchManifest — the golden's outgoing
// commands are exactly the manifest's recorded command set: names
// match one-to-one, and every golden field is a declared (required or
// optional) manifest field. A pi-side command rename discovered at
// refresh time lands here as a manifest mismatch.
func TestDriftGate_GoldenCommandsMatchManifest(t *testing.T) {
	m := loadSurfaceManifest(t)
	rows := map[string]surfaceCommand{}
	for _, c := range m.Commands {
		rows[c.Name] = c
	}

	raw, err := os.ReadFile("testdata/commands_golden.jsonl")
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	seen := map[string]bool{}
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var g goldenCommand
		if err := json.Unmarshal([]byte(line), &g); err != nil {
			t.Fatalf("golden line not JSON: %s", line)
		}
		typ, _ := g.Command["type"].(string)
		if typ == "" {
			t.Fatalf("golden command without type: %s", line)
		}
		row, ok := rows[typ]
		if !ok {
			t.Errorf("golden command %q missing from rpc_surface.json — record it (drift gate)", typ)
			continue
		}
		seen[typ] = true
		allowed := map[string]bool{"type": true}
		for _, f := range row.Fields {
			allowed[f] = true
		}
		for _, f := range row.OptionalFields {
			allowed[f] = true
		}
		for field := range g.Command {
			if !allowed[field] {
				t.Errorf("golden %s carries field %q not declared in rpc_surface.json — extend the manifest in the same change (drift gate)", typ, field)
			}
		}
		for _, f := range row.Fields {
			if _, present := g.Command[f]; !present {
				t.Errorf("manifest declares %s required field %q but the golden never carries it — either the field is optional or the scenario must exercise it", typ, f)
			}
		}
	}
	for name := range rows {
		if !seen[name] {
			t.Errorf("manifest command %q never exercised by commands_golden.jsonl — the golden scenario must cover the full surface", name)
		}
	}
}

// --- helpers -------------------------------------------------------------

func fixtureEventTypeSet(t *testing.T) map[string]bool {
	t.Helper()
	f, err := os.Open("testdata/events_stream.jsonl")
	if err != nil {
		t.Fatalf("open events fixture: %v", err)
	}
	defer f.Close()
	seen := map[string]bool{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var probe struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal([]byte(line), &probe); err != nil || probe.Type == "" {
			t.Fatalf("bad fixture line: %s", line)
		}
		seen[probe.Type] = true
	}
	if len(seen) == 0 {
		t.Fatal("empty events fixture")
	}
	return seen
}
