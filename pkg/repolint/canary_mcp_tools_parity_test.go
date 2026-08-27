// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package repolint

// #880/#905: pin the canary's MCP tool contract to the REAL registry.
// The canary lives in a separate Go module (unimportable); its expected
// subset + floor are parsed from source and validated against an
// in-process pkg/mcp.NewServer registry. A rename/removal in the server
// without a canary update fails HERE — the drift-class guard the issue
// demanded (same file-reading pattern as TestCanary_SchemaVersion_TwinParity).

import (
	"context"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	mcpserver "github.com/lenaxia/llmsafespaces/pkg/mcp"
)

func TestCanary_MCPTools_Parity(t *testing.T) {
	root := repoRoot(t)
	src, err := os.ReadFile(root + "/sdks/canary/mcp/main.go")
	require.NoError(t, err)

	// Parse the named subset.
	listRe := regexp.MustCompile(`(?s)canaryExpectedTools = \[\]string\{(.*?)\}`)
	m := listRe.FindStringSubmatch(string(src))
	require.NotNil(t, m, "canaryExpectedTools must exist in the canary source")
	nameRe := regexp.MustCompile(`"([a-z_]+)"`)
	var expected []string
	for _, line := range strings.Split(m[1], "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "//") {
			continue
		}
		for _, n := range nameRe.FindAllStringSubmatch(line, -1) {
			expected = append(expected, n[1])
		}
	}
	require.NotEmpty(t, expected, "canary expected subset must not be empty")

	// Parse the floor.
	floorRe := regexp.MustCompile(`canaryToolFloor = (\d+)`)
	mf := floorRe.FindStringSubmatch(string(src))
	require.NotNil(t, mf, "canaryToolFloor must exist")
	floor := 0
	for _, c := range mf[1] {
		floor = floor*10 + int(c-'0')
	}

	// Build the REAL registry and enumerate it through the same
	// in-process client the integration test uses — no duplication of
	// the tool list on this side.
	srv := mcpserver.NewServer(nil, 30*time.Second)
	cl, err := client.NewInProcessClient(srv)
	require.NoError(t, err)
	defer func() { _ = cl.Close() }()
	ctx := context.Background()
	require.NoError(t, cl.Start(ctx))
	initReq := mcp.InitializeRequest{}
	initReq.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	initReq.Params.ClientInfo = mcp.Implementation{Name: "parity", Version: "1.0"}
	_, err = cl.Initialize(ctx, initReq)
	require.NoError(t, err)

	resp, err := cl.ListTools(ctx, mcp.ListToolsRequest{})
	require.NoError(t, err)
	names := map[string]bool{}
	for _, tool := range resp.Tools {
		names[tool.Name] = true
	}

	for _, e := range expected {
		require.True(t, names[e],
			"canary expects tool %q which the real registry does not serve — update sdks/canary/mcp/main.go (or the rename was unintentional)", e)
	}
	require.GreaterOrEqual(t, len(resp.Tools), floor,
		"canary floor (%d) exceeds the real registry size (%d) — tools were removed; update the canary", floor, len(resp.Tools))
}

// TestDocs_MCPTools_Parity pins docs/api/mcp.md to the real tool
// registry (#1038): every registered tool must be documented and the
// doc must not list removed/renamed tools. The doc may describe a tool
// in prose, but any backticked `tool_name` in a table row is part of
// the documented contract.
func TestDocs_MCPTools_Parity(t *testing.T) {
	doc, err := os.ReadFile(repoRoot(t) + "/docs/api/mcp.md")
	require.NoError(t, err)

	// Tool tables: rows shaped "| `name` | ... |".
	rowRe := regexp.MustCompile("(?m)^\\| `([a-z_]+)` \\|")
	documented := map[string]bool{}
	for _, m := range rowRe.FindAllStringSubmatch(string(doc), -1) {
		documented[m[1]] = true
	}
	require.NotEmpty(t, documented, "docs/api/mcp.md must document tools in tables")

	srv := mcpserver.NewServer(nil, 30*time.Second)
	cl, err := client.NewInProcessClient(srv)
	require.NoError(t, err)
	defer func() { _ = cl.Close() }()
	ctx := context.Background()
	require.NoError(t, cl.Start(ctx))
	initReq := mcp.InitializeRequest{}
	initReq.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	initReq.Params.ClientInfo = mcp.Implementation{Name: "docs-parity", Version: "1.0"}
	_, err = cl.Initialize(ctx, initReq)
	require.NoError(t, err)

	resp, err := cl.ListTools(ctx, mcp.ListToolsRequest{})
	require.NoError(t, err)
	registered := map[string]bool{}
	for _, tool := range resp.Tools {
		registered[tool.Name] = true
	}

	for name := range documented {
		require.True(t, registered[name],
			"docs/api/mcp.md documents tool %q which the registry does not serve — remove or rename the doc entry", name)
	}
	for name := range registered {
		require.True(t, documented[name],
			"registry serves tool %q which docs/api/mcp.md does not document — add it to the tool tables", name)
	}
}
