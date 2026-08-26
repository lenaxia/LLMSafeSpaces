// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package imagefactory

// base_floor_test.go — design 0051 sidecar migration, step 4: the
// runtime-base compatibility floor (TDD: authored before the
// implementation).
//
// The 2026-08-25 incident: the image factory staged builds onto
// operator-pinned base bookworm@0.8.0 (catalog row from 2026-08-03,
// drifted from the CRD's base@0.20.1). That base carried a pre-#871
// baked agentd and the heredoc init executed it — Init:Error
// crash-loop on contract-shape MCP metadata.
//
// The floor makes that staging error structurally impossible for the
// LEGACY-mode fleet (post step-2, sidecar-mode pods never execute base
// platform code — but the factory cannot know which mode a future
// workspace using this image will run in). It ships in the API's own
// release train, so bumping it is a reviewed release action, not an
// operator DB edit.
//
// Floor value rationale: v0.15.7 is the platform release carrying #871
// (dual-shape Secret metadata) — the minimum base whose baked agentd
// can parse everything the current API stages into secrets.json.

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func floorTestRV() ResolvedValues {
	return ResolvedValues{"python@3.12": {Type: ExtensionTypeApt, Value: "python3"}}
}

// TestBaseFloor_RejectsStaleBase: a base below MinBaseVersion is
// rejected at render time with the floor named — the factory refuses to
// stage onto the incident's base.
func TestBaseFloor_RejectsStaleBase(t *testing.T) {
	_, err := RenderDockerfile(floorTestRV(), Base{
		Name: "bookworm", Version: "0.8.0",
		Image: "ghcr.io/lenaxia/llmsafespaces/base", Tag: "0.8.0",
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), MinBaseVersion)
	require.Contains(t, err.Error(), "0.8.0")
	require.Contains(t, strings.ToLower(err.Error()), "base")
}

// TestBaseFloor_AllowsFixLineageAndNewer: the floor itself (the #871
// release) and everything newer renders.
func TestBaseFloor_AllowsFixLineageAndNewer(t *testing.T) {
	for _, v := range []string{MinBaseVersion, "0.16.0", "0.20.1"} {
		df, err := RenderDockerfile(floorTestRV(), Base{
			Name: "bookworm", Version: v,
			Image: "ghcr.io/lenaxia/llmsafespaces/base", Tag: v,
		})
		require.NoError(t, err, "base %s must render", v)
		require.Contains(t, df, "USER sandbox")
	}
}

// TestBaseFloor_IsTheIncidentFixVersion: the floor must be exactly the
// #871 platform release — if it drifts (accidental bump past deployed
// bases, or drop below), this pin fails and forces a conscious decision.
func TestBaseFloor_IsTheIncidentFixVersion(t *testing.T) {
	require.Equal(t, "0.15.7", MinBaseVersion)
}
