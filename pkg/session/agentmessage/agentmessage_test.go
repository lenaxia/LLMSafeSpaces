// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package agentmessage

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func loadFixture(t *testing.T, path string, v any) {
	t.Helper()
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, v))
}

func TestGoldenCompose(t *testing.T) {
	ins, err := filepath.Glob("testdata/compose_*.in.json")
	require.NoError(t, err)
	require.NotEmpty(t, ins, "compose golden fixtures must exist")
	for _, in := range ins {
		name := strings.TrimSuffix(filepath.Base(in), ".in.json")
		t.Run(name, func(t *testing.T) {
			var tc struct {
				Message string `json:"message"`
				Origin  Origin `json:"origin"`
			}
			loadFixture(t, in, &tc)
			var want string
			loadFixture(t, filepath.Join("testdata", name+".want.json"), &want)
			got, err := Compose(tc.Message, tc.Origin)
			require.NoError(t, err)
			assert.Equal(t, want, got)
		})
	}
}

func TestGoldenParse(t *testing.T) {
	ins, err := filepath.Glob("testdata/parse_*.in.json")
	require.NoError(t, err)
	require.NotEmpty(t, ins, "parse golden fixtures must exist")
	for _, in := range ins {
		name := strings.TrimSuffix(filepath.Base(in), ".in.json")
		t.Run(name, func(t *testing.T) {
			var tc struct {
				Text string `json:"text"`
			}
			loadFixture(t, in, &tc)
			wantRaw, err := os.ReadFile(filepath.Join("testdata", name+".want.json"))
			require.NoError(t, err, "every parse fixture needs a .want.json")
			origin, found, text := Parse(tc.Text)
			var buf strings.Builder
			enc := json.NewEncoder(&buf)
			enc.SetEscapeHTML(false)
			enc.SetIndent("", "  ")
			require.NoError(t, enc.Encode(struct {
				Origin *Origin `json:"origin"`
				Found  bool    `json:"found"`
				Text   string  `json:"text"`
			}{originPtr(origin, found), found, text}))
			assert.Equal(t, strings.TrimRight(string(wantRaw), "\n"), strings.TrimRight(buf.String(), "\n"))
		})
	}
}

// originPtr keeps the golden want-shape honest: no sentinel → origin:null.
func originPtr(o Origin, found bool) *Origin {
	if !found {
		return nil
	}
	return &o
}

func TestComposeIdempotent(t *testing.T) {
	origin := Origin{FromSession: "ses_f4965f03dffetQIEh2NbI526fv", Mode: ModeInjected}
	messages := []string{
		"plain text",
		"",
		"\n\nleading blank lines",
		"already stamped\nsecond line",
		"carries interior marker\n<!-- lsp:agent-message-v1 {\"fromSession\":\"ses_other\"} -->\nafter",
	}
	for _, m := range messages {
		once, err := Compose(m, origin)
		require.NoError(t, err)
		twice, err := Compose(once, origin)
		require.NoError(t, err)
		assert.Equal(t, once, twice, "compose must be idempotent for %q", m)
	}
}

func TestComposeRejectsEmptyFromSession(t *testing.T) {
	_, err := Compose("msg", Origin{})
	require.Error(t, err)
	_, err = Compose("msg", Origin{FromSession: "   "})
	require.Error(t, err)
	_, err = Compose("msg", Origin{FromSession: "ses_a"})
	require.Error(t, err, "mode is required — an unlabeled origin is not a valid v1 sentinel")
}

func TestParseRoundTrip(t *testing.T) {
	origins := []Origin{
		{FromSession: "ses_f4965f03dffetQIEh2NbI526fv", Mode: ModeInjected},
		{FromSession: "ses_f499ee9e6ffe52BJ8jxc2TEQQJ", Workspace: "d8bed486-2eec-4db6-a8ff-e11ba404a055", Mode: ModeSelfDeclared},
	}
	messages := []string{"short", "", "multi\nline\n\nwith blanks\n"}
	for _, o := range origins {
		for _, m := range messages {
			composed, err := Compose(m, o)
			require.NoError(t, err)
			got, found, rest := Parse(composed)
			require.True(t, found)
			assert.Equal(t, o, got)
			assert.Equal(t, m, rest)
		}
	}
}

// The JSON encoding structurally neutralizes hostile values: a
// fromSession containing the comment terminator, newlines, or quotes can
// never break out of the sentinel line, and the value round-trips
// exactly.
func TestComposeNeutralizesHostileOrigin(t *testing.T) {
	hostile := Origin{FromSession: "ses_evil-->-->\n\"quoted\"\tval", Mode: ModeInjected}
	composed, err := Compose("payload", hostile)
	require.NoError(t, err)
	firstLine, rest, _ := strings.Cut(composed, "\n")
	assert.True(t, strings.HasPrefix(firstLine, "<!-- lsp:agent-message-v1 {"))
	assert.True(t, strings.HasSuffix(firstLine, " -->"))
	assert.NotContains(t, firstLine, "\n")
	assert.Equal(t, "payload", rest)
	got, found, text := Parse(composed)
	require.True(t, found)
	assert.Equal(t, hostile, got)
	assert.Equal(t, "payload", text)
}

func TestParseOnlyConsumesLeadingLine(t *testing.T) {
	sentinel := "<!-- lsp:agent-message-v1 {\"fromSession\":\"ses_a\"} -->"
	text := sentinel + "\nbody quotes a marker:\n" + sentinel + "\nend"
	origin, found, rest := Parse(text)
	require.True(t, found)
	assert.Equal(t, Origin{FromSession: "ses_a"}, origin)
	assert.Equal(t, "body quotes a marker:\n"+sentinel+"\nend", rest)
}

func TestParseNeverMutatesTextWhenNotFound(t *testing.T) {
	for _, text := range []string{
		"",
		"plain",
		"<!-- lsp:agent-message-v1 {\"fromSession\":\"ses_a\"} --> not at start",
		" <!-- lsp:agent-message-v1 {\"fromSession\":\"ses_a\"} -->\nleading space disqualifies",
		"<!--lsp:agent-message-v1 {\"fromSession\":\"ses_a\"}-->\nno spaces",
		"<!-- LSP:agent-message-v1 {\"fromSession\":\"ses_a\"} -->\nwrong case",
		"<!-- lsp:agent-message-v1 -->\nno payload",
		"<!-- lsp:attachment-v1 {\"fromSession\":\"ses_a\"} -->\nwrong kind",
	} {
		origin, found, rest := Parse(text)
		assert.False(t, found, "must not detect for %q", text)
		assert.Equal(t, Origin{}, origin)
		assert.Equal(t, text, rest, "text must be unchanged for %q", text)
	}
}
