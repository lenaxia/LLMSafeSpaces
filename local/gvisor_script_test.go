// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package local_test

// gvisor_script_test.go — regression pin for local/lib/gvisor.sh, same
// philosophy as s5_kind_script_test.go: the script provisions a kind
// node, which unit tests cannot execute; what is pinnable
// deterministically is the structure past failures actually broke.
//
// 2026-09 release-shape incident (run 35306741294): gVisor's release
// bundle gained a gvisor-bin/ directory carrying gvisor_sentry and
// helpers, and runsc now REQUIRES the sentry sidecar at
// /usr/local/bin/gvisor-bin under --sidecar-usage-policy=STRICT. The
// install flow extracted only runsc + the shim, so every runsc pod
// failed sandbox creation with `sidecar "gvisor_sentry" not usable
// (stat /usr/local/bin/gvisor-bin/gvisor_sentry: no such file or
// directory)` — S5.6 red on main. These pins fail if the sidecar
// install is ever dropped again.

import (
	"os"
	"strings"
	"testing"
)

func gvisorShSource(t *testing.T) string {
	t.Helper()
	src, err := os.ReadFile("lib/gvisor.sh")
	if err != nil {
		t.Fatalf("read local/lib/gvisor.sh: %v", err)
	}
	return string(src)
}

func TestGvisorSh_InstallsSentrySidecarDir(t *testing.T) {
	src := gvisorShSource(t)
	// The bundle extraction must include the gvisor-bin/ directory…
	if !strings.Contains(src, "gvisor-bin") {
		t.Fatal("gvisor.sh does not extract the gvisor-bin/ sidecar directory — runsc >= 2026-09 fails sandbox creation under --sidecar-usage-policy=STRICT (run 35306741294)")
	}
	// …into the location runsc's STRICT policy expects.
	if !strings.Contains(src, "/usr/local/bin/gvisor-bin") {
		t.Fatal("gvisor.sh does not install the sidecar into /usr/local/bin/gvisor-bin — runsc >= 2026-09 requires gvisor_sentry there (run 35306741294)")
	}
	if !strings.Contains(src, "gvisor_sentry") {
		t.Fatal("gvisor.sh comment/pin must name gvisor_sentry so the release-shape contract is greppable")
	}
}
