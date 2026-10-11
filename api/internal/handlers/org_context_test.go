// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/types"
)

// Design 0064 §6.1/§6.2 (#1669): GET /auth/config gains an optional
// orgContext {slug, name} when the request arrives on an org subdomain
// (<slug>.<baseDomain>) for an org that exists AND has an SSO config. The
// existence contract is the pin: orgContext is present iff host matches ∧ org
// exists ∧ SSO configured — unknown-slug and org-without-SSO are
// indistinguishable (both omit the field), every miss branch omits (fail-soft,
// never 5xx), and the display name is intended payload under the "C" owner
// selection (pinned as payload, NOT as an oracle-parity claim — the name is a
// disclosed metadata delta over the start-URL oracle; see design §4C).

func TestOrgSlugFromHost(t *testing.T) {
	tests := []struct {
		name string
		host string
		base string
		want string
	}{
		{"plain subdomain", "acme.app.example.com", "app.example.com", "acme"},
		{"port stripped", "acme.app.example.com:8443", "app.example.com", "acme"},
		{"case-insensitive host and slug lowercased", "ACME.APP.EXAMPLE.COM", "app.example.com", "acme"},
		{"base normalized with leading dot", "acme.app.example.com", ".app.example.com", "acme"},
		{"exact base host rejected (no slug)", "app.example.com", "app.example.com", ""},
		{"deeper host rejected", "a.b.app.example.com", "app.example.com", ""},
		{"suffix mismatch rejected", "acme.other.example.com", "app.example.com", ""},
		{"different TLD rejected", "acme.app.example.org", "app.example.com", ""},
		{"empty base means routing disabled", "acme.app.example.com", "", ""},
		{"empty host", "", "app.example.com", ""},
		{"bare hostname not a subdomain of base", "localhost", "app.example.com", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, OrgSlugFromHost(tt.host, tt.base))
		})
	}
}

// newOrgContextTestHandler builds a real SSOHandler over the shared mock store
// (one store serves service + handler — the org_sso_test.go pattern).
func newOrgContextTestHandler(t *testing.T) (*SSOHandler, *mockSSOStore) {
	t.Helper()
	store := newMockSSOStore()
	users := newMockSSOHandlerUserStore()
	store.slugToOrg["acme"] = &types.Organization{ID: "org-1", Name: "Acme Corp", Slug: "acme"}
	store.slugToOrg["beta"] = &types.Organization{ID: "org-2", Name: "Beta Ltd", Slug: "beta"}
	store.configs["org-1"] = &types.OrgSSOConfig{OrgID: "org-1", AutoProvision: true}
	// org-2 (beta) deliberately has NO SSO config.
	svc := newSSOServiceForHandler(t, store, users, "https://api.test.local")
	return NewSSOHandler(svc, store, &mockOrgAuthService{userID: "admin-1"}, "lsp_session", "", "https://app.test.local", nil), store
}

func TestOrgContextForHost(t *testing.T) {
	t.Run("subdomain host with SSO org yields slug and name", func(t *testing.T) {
		h, _ := newOrgContextTestHandler(t)
		got := h.OrgContextForHost(context.Background(), "acme.app.example.com", "app.example.com")
		require.NotNil(t, got)
		assert.Equal(t, "acme", got.Slug)
		assert.Equal(t, "Acme Corp", got.Name)
	})

	t.Run("org without SSO config is indistinguishable from unknown slug", func(t *testing.T) {
		h, _ := newOrgContextTestHandler(t)
		assert.Nil(t, h.OrgContextForHost(context.Background(), "beta.app.example.com", "app.example.com"))
		assert.Nil(t, h.OrgContextForHost(context.Background(), "nope.app.example.com", "app.example.com"))
	})

	t.Run("host not on base omitted", func(t *testing.T) {
		h, _ := newOrgContextTestHandler(t)
		assert.Nil(t, h.OrgContextForHost(context.Background(), "acme.other.example.com", "app.example.com"))
		assert.Nil(t, h.OrgContextForHost(context.Background(), "app.example.com", "app.example.com"))
	})

	t.Run("empty base means routing disabled — omitted", func(t *testing.T) {
		h, _ := newOrgContextTestHandler(t)
		assert.Nil(t, h.OrgContextForHost(context.Background(), "acme.app.example.com", ""))
	})

	t.Run("DB error fails soft to nil", func(t *testing.T) {
		h, store := newOrgContextTestHandler(t)
		store.slugErr = errors.New("db down")
		assert.Nil(t, h.OrgContextForHost(context.Background(), "acme.app.example.com", "app.example.com"))
	})

	t.Run("zero-value handler (contract-test wiring) is nil-safe", func(t *testing.T) {
		h := &SSOHandler{}
		assert.Nil(t, h.OrgContextForHost(context.Background(), "acme.app.example.com", "app.example.com"))
	})
}
