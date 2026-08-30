// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package opencode

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	abiv1 "github.com/lenaxia/llmsafespaces/pkg/abi/v1"
	"github.com/stretchr/testify/require"
)

// TestTranslateABI_GoldenFixtures replays the pinned live SSE captures
// (testdata/sse_events_*_live.jsonl, REFRESH.md provenance) through the
// sole translation point and locks the output — the bump-gate pattern
// (agent_config_writer_schema_test.go precedent): an opencode version bump
// that changes any translated event shape fails here until the fixtures are
// refreshed and the table updated, deliberately.
//
// The golden summary is shape-stable (no timestamps; text bodies reduced to
// lengths; IDs kept whole — they are the I12 stitch surface).
func TestTranslateABI_GoldenFixtures(t *testing.T) {
	fixtures, err := filepath.Glob("testdata/sse_events_*_live.jsonl")
	require.NoError(t, err)
	require.NotEmpty(t, fixtures, "pinned live fixtures missing")

	tr := ABITranslator{Now: func() time.Time { return time.Unix(0, 0).UTC() }}
	for _, fx := range fixtures {
		name := strings.TrimSuffix(filepath.Base(fx), ".jsonl")
		t.Run(name, func(t *testing.T) {
			data, err := os.ReadFile(fx)
			require.NoError(t, err)

			var out bytes.Buffer
			enc := json.NewEncoder(&out)
			unknown, ignored := 0, 0
			for _, line := range strings.Split(string(data), "\n") {
				line = strings.TrimSpace(line)
				if line == "" {
					continue
				}
				evt, ok, terr := tr.Parse([]byte(line))
				sum := goldenSummary(evt, ok, terr)
				if ok && evt != nil && evt.Part != nil && evt.Part.GetType() == abiv1.PartType_PART_TYPE_CUSTOM {
					unknown++
				}
				if !ok {
					ignored++
				}
				require.NoError(t, enc.Encode(sum))
			}
			require.NoError(t, enc.Encode(map[string]any{"unknown": unknown, "ignored": ignored}))

			wantPath := filepath.Join("testdata", "golden", name+"_abi.want.jsonl")
			if os.Getenv("REGEN_GOLDEN") == "1" {
				require.NoError(t, os.MkdirAll(filepath.Dir(wantPath), 0o755))
				require.NoError(t, os.WriteFile(wantPath, out.Bytes(), 0o644))
				t.Logf("regenerated %s", wantPath)
				return
			}
			want, err := os.ReadFile(wantPath)
			require.NoError(t, err, "golden missing — run: REGEN_GOLDEN=1 go test -run TestTranslateABI_GoldenFixtures ./pkg/agent/opencode/ and review the diff (bump-gate: deliberate change only)")
			require.Equal(t, string(want), out.String(), "translation drift vs pinned fixtures — REFRESH.md procedure: regenerate, inspect, update the table if a shape changed")
		})
	}
}

func goldenSummary(evt *abiv1.Event, ok bool, err error) map[string]any {
	m := map[string]any{"ok": ok}
	if err != nil {
		m["err"] = err.Error()
		return m
	}
	if !ok || evt == nil {
		return m
	}
	m["type"] = int32(evt.Type)
	m["sid"] = evt.SessionId
	m["mid"] = evt.MessageId
	m["pid"] = evt.PartId
	if evt.Status != 0 {
		m["status"] = int32(evt.Status)
	}
	if evt.Delta != "" {
		m["deltaLen"] = len(evt.Delta)
	}
	if p := evt.GetPart(); p != nil {
		pm := map[string]any{"type": int32(p.GetType())}
		if p.GetId() != "" {
			pm["id"] = p.GetId()
		}
		switch p.GetType() {
		case abiv1.PartType_PART_TYPE_TEXT:
			pm["textLen"] = len(p.GetText())
		case abiv1.PartType_PART_TYPE_REASONING:
			pm["textLen"] = len(p.GetReasoning())
		case abiv1.PartType_PART_TYPE_CUSTOM:
			pm["kind"] = p.GetCustom().GetKind()
			pm["dataLen"] = len(p.GetCustom().GetData())
		case abiv1.PartType_PART_TYPE_TOOL:
			pm["name"] = p.GetTool().GetName()
			pm["status"] = int32(p.GetTool().GetState().GetStatus())
		case abiv1.PartType_PART_TYPE_FILE_CHANGE:
			pm["path"] = p.GetFileChange().GetPath()
		}
		m["part"] = pm
	}
	if s := evt.GetSession(); s != nil {
		m["session"] = map[string]any{"id": s.GetId(), "titleLen": len(s.GetTitle()), "costUsd": s.GetCost().GetCostUsd()}
	}
	if msg := evt.GetMessage(); msg != nil {
		mm := map[string]any{"id": msg.GetId(), "type": int32(msg.GetType()), "hasModel": msg.GetModel() != nil, "hasCost": msg.GetCost() != nil}
		if msg.GetModel() != nil {
			mm["model"] = msg.GetModel().GetId()
			mm["provider"] = msg.GetModel().GetProvider()
		}
		if c := msg.GetCost(); c != nil {
			mm["cost"] = map[string]int64{"in": c.GetInputTokens(), "out": c.GetOutputTokens(), "rea": c.GetReasoningTokens(), "cr": c.GetCacheReadTokens()}
		}
		if msg.GetText() != "" {
			mm["textLen"] = len(msg.GetText())
		}
		m["message"] = mm
	}
	if in := evt.GetInput(); in != nil {
		m["input"] = map[string]any{"id": in.GetId(), "kind": int32(in.GetKind()), "opts": len(in.GetOptions())}
	}
	if e := evt.GetError(); e != nil {
		m["error"] = map[string]any{"code": e.GetCode(), "msgLen": len(e.GetMessage())}
	}
	return m
}
