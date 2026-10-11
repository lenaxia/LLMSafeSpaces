// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"net"
	"strings"

	"github.com/lenaxia/llmsafespaces/pkg/types"
)

// OrgSlugFromHost extracts the org slug from a request Host when the host is
// exactly one label under base (design 0064 §6.1, #1669): "<slug>.<base>".
// Returns "" for every non-matching shape — the exact base host, deeper
// hosts, different suffixes, or an empty base (subdomain routing disabled).
// Ports are stripped and comparison is case-insensitive (DNS semantics); the
// returned slug is lowercased. This is display-only routing metadata — no
// security decision may consume it; the SSO flow's org binding remains the
// path-parameterized start URL + org-bound state cookie.
func OrgSlugFromHost(host, base string) string {
	base = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(base)), ".")
	if base == "" {
		return ""
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.ToLower(strings.TrimSpace(host))
	if host == base || !strings.HasSuffix(host, "."+base) {
		return ""
	}
	slug := strings.TrimSuffix(host, "."+base)
	// Exactly one label: "acme.base" yes, "a.b.base" no. A slug cannot be
	// empty or contain dots (org slugs are single DNS labels).
	if slug == "" || strings.Contains(slug, ".") {
		return ""
	}
	return slug
}

// OrgContextForHost resolves the login surface's org context for a request
// Host (design 0064 §6.1, #1669). Returns nil for every miss — routing
// disabled, non-matching host, unknown slug, org without SSO config, or a
// store error (fail-soft: the /auth/config response omits the field and
// stays 200). The result is display-only.
func (h *SSOHandler) OrgContextForHost(ctx context.Context, host, baseDomain string) *types.AuthConfigOrgContext {
	if h == nil || h.svc == nil {
		return nil
	}
	slug := OrgSlugFromHost(host, baseDomain)
	if slug == "" {
		return nil
	}
	org, err := h.svc.ResolveSSOOrg(ctx, slug)
	if err != nil {
		if h.logger != nil {
			h.logger.Warn("sso org context resolution failed", "slug", slug, "error", err.Error())
		}
		return nil
	}
	if org == nil {
		return nil
	}
	return &types.AuthConfigOrgContext{Slug: slug, Name: org.Name}
}
