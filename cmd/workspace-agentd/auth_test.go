package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCheckBasicAuth(t *testing.T) {
	tests := []struct {
		name     string
		header   string
		password string
		want     bool
	}{
		{"valid credentials", "Basic " + basicAuth("test-pass"), "test-pass", true},
		{"wrong password", "Basic " + basicAuth("other-pass"), "test-pass", false},
		{"missing header", "", "test-pass", false},
		{"bearer scheme", "Bearer " + basicAuth("test-pass"), "test-pass", false},
		{"raw credentials no basic prefix", basicAuth("test-pass"), "test-pass", false},
		{"malformed base64", "Basic !!!not-base64!!!", "test-pass", false},
		{"empty header value", " ", "test-pass", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/workflow/node/execute", nil)
			if strings.TrimSpace(tt.header) != "" {
				req.Header.Set("Authorization", tt.header)
			} else {
				req.Header.Del("Authorization")
			}
			if got := checkBasicAuth(req, tt.password); got != tt.want {
				t.Errorf("checkBasicAuth() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRejectUnauthorized(t *testing.T) {
	w := httptest.NewRecorder()
	rejectUnauthorized(w)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", w.Code)
	}
	if got := w.Header().Get("WWW-Authenticate"); got != `Basic realm="agentd"` {
		t.Errorf("expected WWW-Authenticate Basic realm, got %q", got)
	}
}
