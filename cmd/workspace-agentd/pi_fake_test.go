// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// pi_fake_test.go — the scripted RPC fixture: a REAL subprocess
// speaking pi's RPC-mode JSONL protocol (scoping §1.2), re-exec'd from
// the test binary (the TestHelperProcess pattern this package already
// uses for supervise-opencode). The S2 acceptance criteria are pinned
// against this fixture, not against the aggregator's internals — the
// wire shapes it emits are the ones validated against the pinned real
// binary (v1.5.2): responses nest payload under data; session events
// carry payload at the envelope TOP level; agent_end does not settle;
// usage rides message_update/message_end.
//
// Behavior is FILE-DRIVEN so one fake binary serves every scenario and
// per-session variation comes free (each child resolves its knobs from
// its own --session-id):
//
//	PI_FAKE_STATE_DIR   get_state reads  <dir>/<sid>.state.json
//	PI_FAKE_STATS_DIR   get_session_stats reads <dir>/<sid>.stats.json
//	PI_FAKE_SCRIPT_DIR  prompt emits the lines in <dir>/<sid>.script
//	PI_FAKE_PERSIST_DIR prompt appends one line to
//	                     <dir>/<ts>_<sid>.jsonl (persisted-session truth)
//	PI_FAKE_BOOTLOG_DIR one argv line per boot (generation counter)
//	PI_FAKE_CMDLOG_DIR  one "<type> <args...>" line per received command
//
//	PI_FAKE_STOP_READ_AFTER=N  stop reading stdin after N commands (wedge)
//	PI_FAKE_IGNORE_EOF=1       survive stdin EOF (SIGTERM ladder)
//	PI_FAKE_TRAP_TERM=1        ignore SIGTERM too (SIGKILL ladder)
//	PI_FAKE_SUICIDE_ON_PROMPT  emit agent_start then exit(9) mid-turn
//	PI_FAKE_HUGE_ON_BOOT       emit one >64 KiB agent_end record first

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestPiFakeProcess is the re-exec entry point for the fake pi child.
// It returns immediately (a no-op test) unless PI_FAKE=1 is set in its
// environment; the fake loop then never returns.
func TestPiFakeProcess(t *testing.T) {
	if os.Getenv("PI_FAKE") != "1" {
		return
	}
	os.Exit(piFakeRun())
}

// piFakeSessionID resolves the child's session identity from argv
// (--session-id <id>), defaulting to the no-session shape.
func piFakeSessionID() string {
	args := os.Args
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "--session-id" {
			return args[i+1]
		}
	}
	return "_nosession_"
}

// piFakeReadFile returns the file's content or "" when absent.
func piFakeReadFile(path string) string {
	b, err := os.ReadFile(path) //nolint:gosec // test fixture path
	if err != nil {
		return ""
	}
	return string(b)
}

// piFakeAppend appends one line to dir/name, creating as needed.
func piFakeAppend(dir, name, line string) {
	if dir == "" {
		return
	}
	_ = os.MkdirAll(dir, 0o755)
	f, err := os.OpenFile(filepath.Join(dir, name), //nolint:gosec // test fixture path
		os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()
	_, _ = f.WriteString(line + "\n")
}

// piFakeEmit writes one protocol line to stdout (flushed per line —
// the parent reads continuously).
func piFakeEmit(line string) {
	_, _ = os.Stdout.WriteString(line + "\n")
}

// piFakeRespond answers one command with the data payload (nil data →
// success with no payload; ok=false → a failed response).
func piFakeRespond(id, command string, data any, ok bool, errMsg string) {
	resp := map[string]any{
		"type":    "response",
		"id":      id,
		"command": command,
		"success": ok,
	}
	if data != nil {
		resp["data"] = data
	}
	if errMsg != "" {
		resp["error"] = errMsg
	}
	b, err := json.Marshal(resp)
	if err != nil {
		return
	}
	piFakeEmit(string(b))
}

// piFakeStateData renders get_state's data payload, defaulting to the
// idle in-memory session; PI_FAKE_STATE_DIR/<sid>.state.json overrides
// every field the supervisor consumes (isStreaming/isCompacting/model/
// sessionName).
func piFakeStateData(sid string) any {
	if raw := piFakeReadFile(filepath.Join(os.Getenv("PI_FAKE_STATE_DIR"), sid+".state.json")); raw != "" {
		var data any
		if json.Unmarshal([]byte(raw), &data) == nil {
			return data
		}
	}
	return map[string]any{
		"sessionId":    sid,
		"sessionName":  "Session " + sid,
		"isStreaming":  false,
		"isCompacting": false,
		"model": map[string]any{
			"id":            "test-model",
			"provider":      "fixture",
			"contextWindow": 128000,
		},
	}
}

// piFakeStatsData renders get_session_stats' data payload; the dir
// override carries tokens/contextUsage.
func piFakeStatsData(sid string) any {
	if raw := piFakeReadFile(filepath.Join(os.Getenv("PI_FAKE_STATS_DIR"), sid+".stats.json")); raw != "" {
		var data any
		if json.Unmarshal([]byte(raw), &data) == nil {
			return data
		}
	}
	return map[string]any{
		"tokens": map[string]any{"input": 1, "output": 2, "cacheRead": 0, "cacheWrite": 0},
		"contextUsage": map[string]any{
			"tokens": 100, "contextWindow": 128000, "percent": 0.1,
		},
	}
}

// piFakeRunScript emits the prompt's scripted event lines (one JSON
// record per line; blank lines and # comments allowed for readability).
func piFakeRunScript(sid string) {
	raw := piFakeReadFile(filepath.Join(os.Getenv("PI_FAKE_SCRIPT_DIR"), sid+".script"))
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		piFakeEmit(line)
	}
}

// piFakePersist appends the session-transcript stand-in: one line per
// prompt, under the persisted <timestamp>_<sid>.jsonl name shape.
func piFakePersist(sid string) {
	dir := os.Getenv("PI_FAKE_PERSIST_DIR")
	if dir == "" {
		return
	}
	ts := time.Now().UTC().Format("20060102T150405")
	piFakeAppend(dir, ts+"_"+sid+".jsonl",
		`{"kind":"message","role":"user","summary":"prompt"}`)
}

// piFakeHugeRecord builds one agent_end record whose message array
// blows past bufio.Scanner's 64 KiB cap — the framing contract pinned
// the hard way (real agent_end records carry the full message array).
func piFakeHugeRecord(sid string) string {
	filler := strings.Repeat("x", 96*1024)
	return fmt.Sprintf(`{"type":"agent_end","willRetry":false,"message":[{"role":"assistant","text":"%s"}],"session":"%s"}`, filler, sid)
}

// piFakeRun is the fake's main loop: read commands, act, never return
// unless exiting. Exit paths: stdin EOF (default disposition), SIGTERM
// (default disposition; trapped when PI_FAKE_TRAP_TERM=1), SIGKILL
// (from the outside), or the scripted suicide.
func piFakeRun() int {
	sid := piFakeSessionID()
	piFakeAppend(os.Getenv("PI_FAKE_BOOTLOG_DIR"), sid+".boot",
		strings.Join(os.Args, " "))

	if os.Getenv("PI_FAKE_TRAP_TERM") == "1" {
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, syscall.SIGTERM) // swallow: SIGKILL is the only exit
		go func() {
			for range ch {
			}
		}() //nolint:revive // deliberate swallow
	}
	if os.Getenv("PI_FAKE_HUGE_ON_BOOT") == "1" {
		piFakeEmit(piFakeHugeRecord(sid))
	}

	stopReadAfter := -1
	if v := os.Getenv("PI_FAKE_STOP_READ_AFTER"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			stopReadAfter = n
		}
	}

	br := bufio.NewReaderSize(os.Stdin, 64*1024)
	read := 0
	for {
		line, err := br.ReadString('\n')
		if strings.TrimSpace(line) == "" && err != nil {
			// EOF (or read error): default is a clean exit — rpc.md's
			// "stdin EOF starts shutdown"; PI_FAKE_IGNORE_EOF keeps the
			// process alive so the signal ladder is exercised.
			if os.Getenv("PI_FAKE_IGNORE_EOF") == "1" {
				piFakeBlockForever()
			}
			return 0
		}
		var cmd struct {
			Type        string `json:"type"`
			ID          string `json:"id"`
			SessionPath string `json:"sessionPath"`
		}
		if json.Unmarshal([]byte(strings.TrimSpace(line)), &cmd) != nil || cmd.Type == "" {
			// Malformed stdin JSON: the no-id parse-error response family.
			piFakeRespond("", "parse", nil, false, "invalid JSON")
			continue
		}
		read++
		if stopReadAfter >= 0 && read > stopReadAfter {
			// Wedge posture: stop consuming stdin, stay alive.
			piFakeBlockForever()
		}
		if cmd.SessionPath != "" {
			piFakeAppend(os.Getenv("PI_FAKE_CMDLOG_DIR"), sid+".cmds", cmd.Type+" "+cmd.SessionPath)
		} else {
			piFakeAppend(os.Getenv("PI_FAKE_CMDLOG_DIR"), sid+".cmds", cmd.Type)
		}
		switch cmd.Type {
		case "prompt":
			piFakeRespond(cmd.ID, cmd.Type, map[string]any{"disposition": "started"}, true, "")
			piFakePersist(sid)
			if os.Getenv("PI_FAKE_SUICIDE_ON_PROMPT") == "1" {
				piFakeEmit(`{"type":"agent_start"}`)
				return 9 // died mid-turn, no settle
			}
			piFakeRunScript(sid)
		case "get_state":
			piFakeRespond(cmd.ID, cmd.Type, piFakeStateData(sid), true, "")
		case "get_session_stats":
			piFakeRespond(cmd.ID, cmd.Type, piFakeStatsData(sid), true, "")
		case "switch_session", "set_session_name", "clear_queue", "abort", "set_model":
			piFakeRespond(cmd.ID, cmd.Type, map[string]any{"ok": true}, true, "")
		default:
			piFakeRespond(cmd.ID, cmd.Type, nil, false, "unknown command "+cmd.Type)
		}
		if err != nil {
			if os.Getenv("PI_FAKE_IGNORE_EOF") == "1" {
				piFakeBlockForever()
			}
			return 0
		}
	}
}

// piFakeBinary writes the wrapper script that execs THIS test binary
// in fake mode, passing its own argv through (so piChildCmd's
// `--mode rpc …` flags land in the fake's os.Args for the boot log).
// The script is the spawn config's Binary; the protocol logic lives in
// the re-exec'd test binary above.
func piFakeBinary(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	script := filepath.Join(dir, "pi")
	//nolint:gosec // os.Args[0] is the trusted test binary path
	content := "#!/bin/sh\nexec \"" + os.Args[0] + "\" -test.run=^TestPiFakeProcess$ -- \"$@\"\n"
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		panic(fmt.Sprintf("fake pi wrapper: %v", err))
	}
	return script
}

// piFakeBaseConfig is the shared spawn config for fake-driven tests:
// the wrapper binary, file-driven knobs under fixtureDir, and the env
// the fake reads.
func piFakeBaseConfig(t *testing.T, fixtureDir string, extraEnv ...string) piSpawnConfig {
	t.Helper()
	for _, sub := range []string{"state", "stats", "script", "persist", "boot", "cmds"} {
		if err := os.MkdirAll(filepath.Join(fixtureDir, sub), 0o755); err != nil {
			panic(err)
		}
	}
	env := append([]string{
		"PI_FAKE=1",
		"PI_FAKE_STATE_DIR=" + filepath.Join(fixtureDir, "state"),
		"PI_FAKE_STATS_DIR=" + filepath.Join(fixtureDir, "stats"),
		"PI_FAKE_SCRIPT_DIR=" + filepath.Join(fixtureDir, "script"),
		"PI_FAKE_PERSIST_DIR=" + filepath.Join(fixtureDir, "persist"),
		"PI_FAKE_BOOTLOG_DIR=" + filepath.Join(fixtureDir, "boot"),
		"PI_FAKE_CMDLOG_DIR=" + filepath.Join(fixtureDir, "cmds"),
	}, extraEnv...)
	return piSpawnConfig{
		Binary:     piFakeBinary(t),
		SessionDir: filepath.Join(fixtureDir, "sessions"),
		AgentDir:   filepath.Join(fixtureDir, "agent"),
		Env:        env,
	}
}

// piFakeWriteFile is filepath.WriteFile with the fixture dir made.
func piFakeWriteFile(dir, name, content string) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		panic(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil { //nolint:gosec // test fixture
		panic(err)
	}
}

// piFakeLines reads a fixture file's lines (empty slice when absent).
func piFakeLines(path string) []string {
	b, err := os.ReadFile(path) //nolint:gosec // test fixture path
	if err != nil {
		return nil
	}
	var out []string
	for _, l := range strings.Split(string(b), "\n") {
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}

// piFakeBlockForever parks without tripping the all-asleep deadlock
// detector (a bare select{} can fatal when every goroutine is parked;
// a timer sleep never does).
func piFakeBlockForever() {
	for {
		time.Sleep(time.Hour)
	}
}
