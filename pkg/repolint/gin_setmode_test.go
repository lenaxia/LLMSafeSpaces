// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package repolint

import (
	"path/filepath"
	"testing"
)

// TestGinSetModeCheck_BadPattern verifies the check flags a file that
// calls gin.SetMode from a t.Parallel() test body — the exact pattern
// that broke main CI on 2026-08-02 (worklog 0663).
func TestGinSetModeCheck_BadPattern(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "racy_test.go"), `package example

import (
	"testing"

	"github.com/gin-gonic/gin"
)

func TestRacy(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	_ = r
}
`)

	rep, err := GinSetModeCheck(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rep.OK() {
		t.Fatalf("expected violation, got OK: %s", rep.String())
	}
	if len(rep.Violations) != 1 || rep.Violations[0] != "racy_test.go" {
		t.Fatalf("expected racy_test.go, got %v", rep.Violations)
	}
}

// TestGinSetModeCheck_HelperPattern verifies the check catches the
// indirect variant: gin.SetMode inside a helper function that a
// t.Parallel() test invokes (this was the actual incident shape — the
// call lived in newCallbackRouter, not in the test body itself).
func TestGinSetModeCheck_HelperPattern(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "helper_test.go"), `package example

import (
	"testing"

	"github.com/gin-gonic/gin"
)

func newRouter(t *testing.T) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	return gin.New()
}

func TestUsesHelper(t *testing.T) {
	t.Parallel()
	_ = newRouter(t)
}
`)

	rep, err := GinSetModeCheck(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rep.OK() {
		t.Fatalf("expected violation for helper pattern, got OK")
	}
}

// TestGinSetModeCheck_InitPattern verifies the safe pattern passes: the
// mode is set once from a package-level init(), tests use t.Parallel()
// freely.
func TestGinSetModeCheck_InitPattern(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "safe_test.go"), `package example

import (
	"testing"

	"github.com/gin-gonic/gin"
)

func init() {
	gin.SetMode(gin.TestMode)
}

func TestSafe(t *testing.T) {
	t.Parallel()
	r := gin.New()
	_ = r
}
`)

	rep, err := GinSetModeCheck(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !rep.OK() {
		t.Fatalf("expected OK for init() pattern, got: %s", rep.String())
	}
}

// TestGinSetModeCheck_TestMainPattern verifies TestMain is an accepted
// safe location for setting the mode once.
func TestGinSetModeCheck_TestMainPattern(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "main_test.go"), `package example

import (
	"os"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestMain(m *testing.M) {
	gin.SetMode(gin.TestMode)
	os.Exit(m.Run())
}

func TestSafe(t *testing.T) {
	t.Parallel()
	_ = gin.New()
}
`)

	rep, err := GinSetModeCheck(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !rep.OK() {
		t.Fatalf("expected OK for TestMain pattern, got: %s", rep.String())
	}
}

// TestGinSetModeCheck_SerialOnly verifies files that call gin.SetMode
// per-test but never use t.Parallel() pass (serial writes to the global
// cannot race).
func TestGinSetModeCheck_SerialOnly(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "serial_test.go"), `package example

import (
	"testing"

	"github.com/gin-gonic/gin"
)

func TestSerial(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	_ = r
}
`)

	rep, err := GinSetModeCheck(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !rep.OK() {
		t.Fatalf("expected OK for serial-only file, got: %s", rep.String())
	}
}

// TestGinSetModeCheck_MixedSerialAndParallel verifies the per-function
// reachability semantics: a file where an unrelated serial test calls
// gin.SetMode while a different test uses t.Parallel must NOT be flagged —
// serial test bodies never overlap parallel ones within a test binary, so
// the write cannot race. This is the case that distinguishes reachability
// analysis from file-level co-occurrence.
func TestGinSetModeCheck_MixedSerialAndParallel(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "mixed_test.go"), `package example

import (
	"testing"

	"github.com/gin-gonic/gin"
)

// TestSerial uses gin.SetMode but never t.Parallel.
func TestSerial(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	_ = r
}

// TestParallel uses t.Parallel but never gin.SetMode, and does not call
// TestSerial or any shared helper.
func TestParallel(t *testing.T) {
	t.Parallel()
	_ = gin.New()
}
`)

	rep, err := GinSetModeCheck(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !rep.OK() {
		t.Fatalf("expected OK for mixed serial+parallel file (no shared reachability), got: %s", rep.String())
	}
}

// TestGinSetModeCheck_NonGinReceiver verifies the receiver check: an
// unrelated foo.SetMode() call must not be flagged, even when the file
// contains gin.SetMode (inside init, which is safe) and a t.Parallel test.
func TestGinSetModeCheck_NonGinReceiver(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "receiver_test.go"), `package example

import (
	"testing"

	"github.com/gin-gonic/gin"
)

type logger struct{}

func (l *logger) SetMode(string) {}

func init() {
	gin.SetMode(gin.TestMode)
}

func TestUsesParallel(t *testing.T) {
	t.Parallel()
	l := &logger{}
	l.SetMode("loud")
	_ = gin.New()
}
`)

	rep, err := GinSetModeCheck(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !rep.OK() {
		t.Fatalf("expected OK for non-gin SetMode receiver, got: %s", rep.String())
	}
}

// TestGinSetModeCheck_TransitiveHelper verifies reachability is transitive:
// a helper invoked by another helper that a t.Parallel test calls is still
// flagged.
func TestGinSetModeCheck_TransitiveHelper(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "transitive_test.go"), `package example

import (
	"testing"

	"github.com/gin-gonic/gin"
)

func newRouter(t *testing.T) *gin.Engine {
	t.Helper()
	return buildRouter()
}

func buildRouter() *gin.Engine {
	gin.SetMode(gin.TestMode)
	return gin.New()
}

func TestUsesTransitiveHelper(t *testing.T) {
	t.Parallel()
	_ = newRouter(t)
}
`)

	rep, err := GinSetModeCheck(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rep.OK() {
		t.Fatalf("expected violation for transitive helper, got OK")
	}
}
