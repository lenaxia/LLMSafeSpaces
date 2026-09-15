// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package repolint

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeSpecFile(t *testing.T, dir, content string) {
	t.Helper()
	p := filepath.Join(dir, "sdks", "openapi.yaml")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

const cleanSpec = `openapi: "3.0.3"
info:
  title: LLMSafeSpaces API
  description: |
    LLMSafeSpaces is a Kubernetes-first platform for running AI agents securely in isolated sandboxes.
    Every sandbox runs ` + "`opencode serve`" + ` as a persistent HTTP server with a PVC-backed persistent workspace.
  version: "1.0.0"
servers:
  - url: /api/v1
paths:
  /workspaces/{id}/sessions:
    post:
      operationId: sendMessage
      description: Send a message to the session. Disposes the current opencode process and starts a fresh one when asked to restart.
      responses:
        "200":
          description: The session messages
          content:
            application/json:
              schema:
                $ref: "#/components/schemas/Session"
components:
  schemas:
    Session:
      type: object
      description: A conversation session with the workspace agent.
      properties:
        id:
          type: string
          description: Session identifier.
`

func TestSpecCouplingMarkerCheck_CleanSpecPasses(t *testing.T) {
	dir := t.TempDir()
	writeSpecFile(t, dir, cleanSpec)
	rep, err := SpecCouplingMarkerCheck(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Violations) != 0 {
		t.Fatalf("clean spec (incl. allowlisted info.description mention + factual opencode-process prose) must pass; got %+v", rep.Violations)
	}
}

func TestSpecCouplingMarkerCheck_FlagsProxyKey(t *testing.T) {
	dir := t.TempDir()
	spec := cleanSpec + `      x-opencode-proxy: true
`
	writeSpecFile(t, dir, spec)
	rep, err := SpecCouplingMarkerCheck(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Violations) != 1 {
		t.Fatalf("x-opencode-proxy key must be flagged; got %+v", rep.Violations)
	}
	if rep.Violations[0].Kind != SpecViolationMarkerKey {
		t.Fatalf("wrong kind: %+v", rep.Violations[0])
	}
	if rep.Violations[0].Line == 0 || !strings.Contains(rep.Violations[0].Excerpt, "x-opencode-proxy") {
		t.Fatalf("violation must carry line + excerpt; got %+v", rep.Violations[0])
	}
}

func TestSpecCouplingMarkerCheck_FlagsQuotedProxyKey(t *testing.T) {
	dir := t.TempDir()
	spec := strings.Replace(cleanSpec, `      description: The session messages`, `      description: The session messages
      x-opencode-proxy: "true"`, 1)
	writeSpecFile(t, dir, spec)
	rep, err := SpecCouplingMarkerCheck(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Violations) != 1 {
		t.Fatalf(`quoted "x-opencode-proxy" key must be flagged; got %+v`, rep.Violations)
	}
}

func TestSpecCouplingMarkerCheck_PhraseMatrix(t *testing.T) {
	cases := []struct {
		name   string
		phrase string
	}{
		{"tracks upstream", "This schema tracks upstream opencode."},
		{"from opencode", "Mirrored from opencode's session payload."},
		{"opencode session object", "Shape of the opencode session object."},
		{"case-insensitive", "This schema TRACKS UPSTREAM the agent."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			spec := strings.Replace(cleanSpec,
				`          description: The session messages`,
				`          description: `+tc.phrase, 1)
			writeSpecFile(t, dir, spec)
			rep, err := SpecCouplingMarkerCheck(dir)
			if err != nil {
				t.Fatal(err)
			}
			if len(rep.Violations) != 1 || rep.Violations[0].Kind != SpecViolationCouplingPhrase {
				t.Fatalf("coupling-phrase description must be flagged; got %+v", rep.Violations)
			}
		})
	}
}

func TestSpecCouplingMarkerCheck_PhraseInComponentDescription(t *testing.T) {
	dir := t.TempDir()
	spec := strings.Replace(cleanSpec,
		`      description: A conversation session with the workspace agent.`,
		`      description: A conversation session; schema tracks upstream.`, 1)
	writeSpecFile(t, dir, spec)
	rep, err := SpecCouplingMarkerCheck(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Violations) != 1 {
		t.Fatalf("component schema description phrase must be flagged; got %+v", rep.Violations)
	}
}

func TestSpecCouplingMarkerCheck_PhraseInBlockScalarContinuation(t *testing.T) {
	dir := t.TempDir()
	spec := strings.Replace(cleanSpec,
		`      description: Send a message to the session. Disposes the current opencode process and starts a fresh one when asked to restart.`,
		`      description: |
        Sends the message.
        The response schema tracks upstream opencode
        and is mirrored from opencode's wire shape.`, 1)
	writeSpecFile(t, dir, spec)
	rep, err := SpecCouplingMarkerCheck(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Violations) != 2 {
		t.Fatalf("both phrase lines inside the block scalar must be flagged; got %+v", rep.Violations)
	}
}

func TestSpecCouplingMarkerCheck_InfoDescriptionAllowlisted(t *testing.T) {
	dir := t.TempDir()
	// The single anchored allowlist: the top-level info.description may
	// mention opencode (platform overview) — even phrases there are the
	// sanctioned site, and nothing else is.
	spec := strings.Replace(cleanSpec,
		`    Every sandbox runs `+"`opencode serve`"+` as a persistent HTTP server with a PVC-backed persistent workspace.`,
		`    Every sandbox runs `+"`opencode serve`"+` as a persistent HTTP server.
    The API proxies to opencode session objects.`+"\n    Fields mirrored from opencode.", 1)
	writeSpecFile(t, dir, spec)
	rep, err := SpecCouplingMarkerCheck(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Violations) != 0 {
		t.Fatalf("info.description is the anchored allowlisted site; got %+v", rep.Violations)
	}
}

func TestSpecCouplingMarkerCheck_InfoBlockBoundary(t *testing.T) {
	// The allowlist ends with the info: block — a description AFTER the
	// next top-level key is not covered by it.
	dir := t.TempDir()
	spec := `openapi: "3.0.3"
info:
  title: T
  version: "1.0.0"
tags:
  - name: sessions
    description: Session surface; tracks upstream opencode.
`
	writeSpecFile(t, dir, spec)
	rep, err := SpecCouplingMarkerCheck(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Violations) != 1 {
		t.Fatalf("phrase outside the info block must be flagged (anchored allowlist); got %+v", rep.Violations)
	}
}

func TestSpecCouplingMarkerCheck_InfoSubBlockDescriptionsNotExempt(t *testing.T) {
	// The allowlist is the info.description KEY itself — not the whole
	// info: block. Phrases under info.license/contact/etc. fail.
	dir := t.TempDir()
	spec := `openapi: "3.0.3"
info:
  title: T
  version: "1.0.0"
  description: |
    Platform overview mentioning opencode serve; even prose like
    "mirrored from opencode" stays allowlisted at this one site.
  license:
    name: AGPL
    description: Schema tracks upstream opencode and is mirrored from opencode.
  contact:
    name: x
    description: Mirrored from opencode's payloads.
paths: {}
`
	writeSpecFile(t, dir, spec)
	rep, err := SpecCouplingMarkerCheck(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Violations) != 3 {
		t.Fatalf("license (2 phrases) + contact description phrases must be flagged (only info.description is allowlisted); got %+v", rep.Violations)
	}
	lines := map[int]bool{}
	for _, v := range rep.Violations {
		if v.IsLeaked {
			t.Fatalf("violations must not be tolerated; got %+v", rep.Violations)
		}
		lines[v.Line] = true
	}
	if !lines[10] || !lines[13] {
		t.Fatalf("violations must cover license (10) and contact (13) lines; got %+v", rep.Violations)
	}
}

func TestSpecCouplingMarkerCheck_RequiredSpecFieldsNotExempt(t *testing.T) {
	// info.description itself carries a coupling phrase: still the
	// anchored allowlisted site (documented in #1305's note).
	dir := t.TempDir()
	spec := `openapi: "3.0.3"
info:
  title: T
  description: Overview; the session schema tracks upstream opencode.
  version: "1.0.0"
paths: {}
`
	writeSpecFile(t, dir, spec)
	rep, err := SpecCouplingMarkerCheck(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Violations) != 0 {
		t.Fatalf("info.description is the anchored allowlist; got %+v", rep.Violations)
	}
}

func TestSpecCouplingMarkerCheck_KnownLeakTolerated(t *testing.T) {
	restore := specCouplingKnownLeaks
	specCouplingKnownLeaks = map[string]string{
		"x-opencode-proxy: true": "residual marker pending spec cleanup (#9999)",
	}
	defer func() { specCouplingKnownLeaks = restore }()

	dir := t.TempDir()
	spec := cleanSpec + `      x-opencode-proxy: true
`
	writeSpecFile(t, dir, spec)
	rep, err := SpecCouplingMarkerCheck(dir)
	if err != nil {
		t.Fatal(err)
	}
	if rep.HasNew() {
		t.Fatalf("known leak line must be tolerated; got %+v", rep.Violations)
	}
	if len(rep.Violations) != 1 || !rep.Violations[0].IsLeaked {
		t.Fatalf("leak must still be REPORTED as tolerated; got %+v", rep.Violations)
	}
}

func TestSpecCouplingMarkerCheck_NewLeakFails(t *testing.T) {
	restore := specCouplingKnownLeaks
	specCouplingKnownLeaks = map[string]string{
		"x-opencode-proxy: true": "tolerates one exact marker line (#9999)",
	}
	defer func() { specCouplingKnownLeaks = restore }()

	dir := t.TempDir()
	spec := cleanSpec + `      x-opencode-proxy: true
      x-opencode-proxy: false
`
	writeSpecFile(t, dir, spec)
	rep, err := SpecCouplingMarkerCheck(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.HasNew() {
		t.Fatalf("a DIFFERENT marker line is a new leak and must fail; got %+v", rep.Violations)
	}
}

func TestSpecCouplingMarkerCheck_MissingSpecFails(t *testing.T) {
	dir := t.TempDir()
	if _, err := SpecCouplingMarkerCheck(dir); err == nil {
		t.Fatal("missing sdks/openapi.yaml must be an error, not a silent pass")
	}
}

func TestSpecCouplingMarkerCheck_RealRepo_BirthStateClean(t *testing.T) {
	// #1304 deleted every marker; #1305's rule is born clean on the
	// real spec.
	rep, err := SpecCouplingMarkerCheck(repoRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Violations) != 0 {
		t.Fatalf("rule ships with zero tolerated leaks at birth (#1305); got %+v", rep.Violations)
	}
}

func TestSpecCouplingKnownLeaks_MetaValidation(t *testing.T) {
	if len(specCouplingKnownLeaks) != 0 {
		t.Fatalf("specCouplingKnownLeaks must be empty at birth (#1305); got %+v", specCouplingKnownLeaks)
	}
	if err := validateKnownLeaksEntries("specCouplingKnownLeaks", specCouplingKnownLeaks); err != nil {
		t.Fatalf("shipped map must validate: %v", err)
	}
}
