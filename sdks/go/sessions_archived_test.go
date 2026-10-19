// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package llmsafespaces

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// #1735: the sessions list serialization — the default carries NO
// include_archived param (server default: exclude archived); the
// opt-in appends exactly ?include_archived=true. A param-name typo
// here would silently return the filtered default.
func TestClient_SessionsList_IncludeArchivedSerialization(t *testing.T) {
	cases := []struct {
		name            string
		includeArchived []bool
		wantQuery       string
	}{
		{"default carries no param", nil, ""},
		{"explicit false carries no param", []bool{false}, ""},
		{"opt-in appends the param", []bool{true}, "include_archived=true"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotQuery, gotPath string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath, gotQuery = r.URL.Path, r.URL.RawQuery
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`[]`))
			}))
			defer srv.Close()

			c := New(srv.URL, WithAPIKey("lsp_test"))
			if _, err := c.Sessions.List(context.Background(), "ws-1", tc.includeArchived...); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if gotPath != "/api/v1/workspaces/ws-1/sessions" {
				t.Errorf("unexpected path: %s", gotPath)
			}
			if gotQuery != tc.wantQuery {
				t.Errorf("query = %q, want %q", gotQuery, tc.wantQuery)
			}
		})
	}
}
