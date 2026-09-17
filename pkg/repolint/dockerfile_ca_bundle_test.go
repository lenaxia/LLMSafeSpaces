// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package repolint

import (
	"os"
	"strings"
	"testing"
)

// TestAgentdDockerfile_CABundleDelivered is the regression test for
// #1416: the agentd image is FROM scratch, so without an explicit COPY
// it ships no trust store and every TLS fetch from the sidecar (workflow
// http nodes, any https egress) fails with "certificate signed by
// unknown authority". The fix is one COPY line from the builder at Go's
// first default system-roots probe path (crypto/x509/root_linux.go
// certFiles[0]). This pin fails if that line is dropped, the path
// drifts, or the file is made executable (the "nothing executable"
// contract of the delivery image).
func TestAgentdDockerfile_CABundleDelivered(t *testing.T) {
	raw, err := os.ReadFile("../../cmd/workspace-agentd/Dockerfile")
	if err != nil {
		raw, err = os.ReadFile("cmd/workspace-agentd/Dockerfile")
	}
	if err != nil {
		t.Fatalf("agentd Dockerfile unreadable — the #1416 pins must fail loud, not skip: %v", err)
	}
	df := string(raw)

	const want = "COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt"
	if !strings.Contains(df, want) {
		t.Fatalf("#1416 regression: agentd Dockerfile must COPY the CA bundle at Go's default system-roots path; want line:\n  %s", want)
	}
}

func TestAgentdDockerfile_CABundleNotExecutable(t *testing.T) {
	raw, err := os.ReadFile("../../cmd/workspace-agentd/Dockerfile")
	if err != nil {
		raw, err = os.ReadFile("cmd/workspace-agentd/Dockerfile")
	}
	if err != nil {
		t.Fatalf("agentd Dockerfile unreadable — the #1416 pins must fail loud, not skip: %v", err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.Contains(line, "ca-certificates.crt") && strings.Contains(line, "--chmod=755") {
			t.Fatalf("the CA bundle is DATA (0644 from the builder); --chmod=755 on it breaks the delivery image's nothing-executable contract: %s", line)
		}
	}
}

func TestAgentdDockerfile_BinaryStillExecutable(t *testing.T) {
	raw, err := os.ReadFile("../../cmd/workspace-agentd/Dockerfile")
	if err != nil {
		raw, err = os.ReadFile("cmd/workspace-agentd/Dockerfile")
	}
	if err != nil {
		t.Fatalf("agentd Dockerfile unreadable — the #1416 pins must fail loud, not skip: %v", err)
	}
	if !strings.Contains(string(raw), "COPY --from=builder --chmod=755 /out/workspace-agentd") {
		t.Fatalf("the agentd BINARY must remain --chmod=755 — the kubelet image volume execs it")
	}
}
