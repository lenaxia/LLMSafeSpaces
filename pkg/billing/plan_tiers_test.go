// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package billing

import (
	"testing"

	"github.com/lenaxia/llmsafespaces/pkg/types"
)

func TestMaxPersonalMcpServers_PerTier(t *testing.T) {
	cases := []struct {
		plan types.OrgPlan
		want int
	}{
		{types.PlanFree, 5},
		{types.PlanTeam, -1},
		{types.PlanBusiness, -1},
		{types.PlanEnterprise, -1},
	}
	for _, c := range cases {
		got := GetPlanFeatures(c.plan).MaxPersonalMcpServers
		if got != c.want {
			t.Errorf("plan=%s: MaxPersonalMcpServers=%d, want %d", c.plan, got, c.want)
		}
	}
}

func TestIsFeatureAllowed_PersonalMcpServers(t *testing.T) {
	if !IsFeatureAllowed(types.PlanFree, "personal_mcp_servers") {
		t.Error("free should be allowed (quota=5, not 0)")
	}
	for _, p := range []types.OrgPlan{types.PlanTeam, types.PlanBusiness, types.PlanEnterprise} {
		if !IsFeatureAllowed(p, "personal_mcp_servers") {
			t.Errorf("plan=%s should be allowed (unlimited)", p)
		}
	}
}

func TestIsFeatureAllowed_UnknownFeatureFailsOpen(t *testing.T) {
	if !IsFeatureAllowed(types.PlanFree, "nonexistent_feature") {
		t.Error("unknown features should fail open (return true)")
	}
}
