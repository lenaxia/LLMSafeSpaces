// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package sse

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"

	pkginterfaces "github.com/lenaxia/llmsafespaces/pkg/interfaces"
)

type testLogger struct{}

func (l *testLogger) Debug(msg string, kv ...interface{})                  {}
func (l *testLogger) Info(msg string, kv ...interface{})                   {}
func (l *testLogger) Warn(msg string, kv ...interface{})                   {}
func (l *testLogger) Error(msg string, err error, kv ...interface{})       {}
func (l *testLogger) Fatal(msg string, err error, kv ...interface{})       {}
func (l *testLogger) With(kv ...interface{}) pkginterfaces.LoggerInterface { return l }
func (l *testLogger) Sync() error                                          { return nil }

type capturingLogger struct {
	mu    sync.Mutex
	warns []string
}

func (l *capturingLogger) Debug(msg string, kv ...interface{}) {}
func (l *capturingLogger) Info(msg string, kv ...interface{})  {}
func (l *capturingLogger) Warn(msg string, kv ...interface{}) {
	l.mu.Lock()
	l.warns = append(l.warns, msg)
	l.mu.Unlock()
}
func (l *capturingLogger) Error(msg string, err error, kv ...interface{})       {}
func (l *capturingLogger) Fatal(msg string, err error, kv ...interface{})       {}
func (l *capturingLogger) With(kv ...interface{}) pkginterfaces.LoggerInterface { return l }
func (l *capturingLogger) Sync() error                                          { return nil }

func (l *capturingLogger) Warns() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	cp := make([]string, len(l.warns))
	copy(cp, l.warns)
	return cp
}

type redirectTransport struct {
	server *httptest.Server
}

func (t *redirectTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req.URL.Scheme = "http"
	req.URL.Host = strings.TrimPrefix(t.server.URL, "http://")
	return http.DefaultTransport.RoundTrip(req)
}
