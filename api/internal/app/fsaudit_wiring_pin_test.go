// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"os"
	"strings"
	"testing"
)

// PIN (#1808 review r1 F1 — "built but never wired"): the fs-audit
// facade must be wired to its password provider in app.go's
// construction block. The handler is optional-wired like disk-recover,
// but its dispatch leg REQUIRES the getter: an unwired FsAuditHandler
// passes authz → phase gate → pod resolution and then 500s at the nil
// getter guard — a dead facade that every handler-level test was blind
// to (the tests wire the getter themselves). This source-presence pin
// reddens the moment the wiring line is deleted, which is precisely
// the regression class it exists for (same mechanics as the repolint
// source gates).
func TestFsAuditFacadePasswordGetterWired(t *testing.T) {
	raw, err := os.ReadFile("app.go")
	if err != nil {
		t.Fatalf("read app.go: %v", err)
	}
	src := string(raw)

	// The construction must exist…
	if !strings.Contains(src, "fsAuditHandler = handlers.NewFsAuditHandler(") {
		t.Fatal("FsAuditHandler construction missing from app.go (the facade is not built)")
	}
	// …AND the password getter wiring must exist, inside the same
	// pwGetter block as the disk-recover twin (construction alone was
	// the r1 F1 bug).
	if !strings.Contains(src, "fsAuditHandler.SetPasswordGetter(pwGetter)") {
		t.Fatal("fsAuditHandler.SetPasswordGetter(pwGetter) missing from app.go — every production /fs-audit call would 500 at the nil getter guard (review r1 F1 regression)")
	}
	// And the router must receive the handler.
	if !strings.Contains(src, "FsAuditHandler:                  fsAuditHandler,") {
		t.Fatal("FsAuditHandler not passed to the router config — the route would 404 in production")
	}
}
