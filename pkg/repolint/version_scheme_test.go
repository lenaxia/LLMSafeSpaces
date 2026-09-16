// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package repolint

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Fixtures for RunVersionSchemeCheck: a minimal repo-shaped pair of
// helm/values.yaml + api/internal/imagefactory/catalog.seed.yaml. The
// check must find every key it compares at its real path, so the happy
// fixture mirrors the committed file's structure for the sections the
// guard consumes.

const vsHappyValues = `
api:
  image:
    repository: ghcr.io/lenaxia/llmsafespaces/api
    tag: ""
    digest: ""
controller:
  image:
    repository: ghcr.io/lenaxia/llmsafespaces/controller
    tag: ""
    digest: ""
  agentdDelivery:
    image: ""
    binarySHA256Amd64: ""
    binarySHA256Arm64: ""
  opencodeDelivery:
    image: ""
    binarySHA256Amd64: ""
    binarySHA256Arm64: ""
frontend:
  image:
    repository: ghcr.io/lenaxia/llmsafespaces/frontend
    tag: ""
    digest: ""
runtimeEnvironments:
  base:
    image:
      repository: ghcr.io/lenaxia/llmsafespaces/base
      tag: "2026.09.0"
      digest: ""
`

const vsHappySeed = `
architectures:
  - linux/amd64
bases:
  - name: bookworm
    version: "2026.09.0"
    image: ghcr.io/lenaxia/llmsafespaces/base
    tag: "2026.09.0"
    isDefault: true
extensions: []
`

// writeVersionSchemeFixture lays down the two files the check reads and
// returns the synthetic repo root.
func writeVersionSchemeFixture(t *testing.T, valuesYAML, seedYAML string) string {
	t.Helper()
	root := t.TempDir()
	valuesPath := filepath.Join(root, "helm", "values.yaml")
	seedPath := filepath.Join(root, "api", "internal", "imagefactory", "catalog.seed.yaml")
	if err := os.MkdirAll(filepath.Dir(valuesPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(seedPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(valuesPath, []byte(valuesYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(seedPath, []byte(seedYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

// vsMutate replaces exactly one occurrence of old with new in a fixture,
// failing the test when the anchor is not unique (fixture drift).
func vsMutate(t *testing.T, s, old, new string) string {
	t.Helper()
	if n := strings.Count(s, old); n != 1 {
		t.Fatalf("fixture mutation: expected 1 occurrence of %q, found %d", old, n)
	}
	return strings.Replace(s, old, new, 1)
}

func vsFails(t *testing.T, root string, wantSubstring ...string) {
	t.Helper()
	fails := RunVersionSchemeCheck(root)
	if len(fails) == 0 {
		t.Fatal("expected failures, got none")
	}
	for _, want := range wantSubstring {
		found := false
		for _, f := range fails {
			if strings.Contains(f, want) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("no failure line mentions %q; got: %v", want, fails)
		}
	}
}

func vsClean(t *testing.T, root string) {
	t.Helper()
	if fails := RunVersionSchemeCheck(root); len(fails) != 0 {
		t.Fatalf("expected clean, got: %v", fails)
	}
}

func TestVersionSchemeCheck_HappyPath(t *testing.T) {
	vsClean(t, writeVersionSchemeFixture(t, vsHappyValues, vsHappySeed))
}

func TestVersionSchemeCheck_BaseTagCalVer(t *testing.T) {
	cases := []struct {
		name string
		tag  string
	}{
		{"platform semver (incident 3)", "0.26.0"},
		{"one-digit month", "2026.9.0"},
		{"invalid month", "2026.13.0"},
		{"latest", "latest"},
		{"empty (missing single-source mirror)", `""`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			values := vsMutate(t, vsHappyValues, `tag: "2026.09.0"`, "tag: "+tc.tag)
			vsFails(t, writeVersionSchemeFixture(t, values, vsHappySeed),
				"runtimeEnvironments.base.image.tag")
		})
	}
}

func TestVersionSchemeCheck_BaseTagDriftFromSeed(t *testing.T) {
	values := vsMutate(t, vsHappyValues, `tag: "2026.09.0"`, `tag: "2026.08.0"`)
	vsFails(t, writeVersionSchemeFixture(t, values, vsHappySeed),
		"drift", "2026.08.0", "2026.09.0")
}

func TestVersionSchemeCheck_BaseTagSkippedWhenDigestPinned(t *testing.T) {
	values := vsMutate(t, vsHappyValues,
		`      tag: "2026.09.0"
      digest: ""`,
		`      tag: ""
      digest: "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"`)
	vsClean(t, writeVersionSchemeFixture(t, values, vsHappySeed))
}

func TestVersionSchemeCheck_SeedCalVer(t *testing.T) {
	cases := []struct {
		name string
		seed string
	}{
		{"platform semver version", vsMutate(t, vsHappySeed, `version: "2026.09.0"`, `version: "0.26.0"`)},
		{"one-digit month version", vsMutate(t, vsHappySeed, `version: "2026.09.0"`, `version: "2026.9.0"`)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			vsFails(t, writeVersionSchemeFixture(t, vsHappyValues, tc.seed),
				"catalog.seed.yaml", "bookworm")
		})
	}
}

func TestVersionSchemeCheck_SeedTagCalVer(t *testing.T) {
	seed := vsMutate(t, vsHappySeed, `    tag: "2026.09.0"`, `    tag: "0.26.0"`)
	vsFails(t, writeVersionSchemeFixture(t, vsHappyValues, seed),
		"catalog.seed.yaml", "tag")
}

func TestVersionSchemeCheck_SeedTagVersionMismatch(t *testing.T) {
	seed := vsMutate(t, vsHappySeed, `    tag: "2026.09.0"`, `    tag: "2026.08.0"`)
	vsFails(t, writeVersionSchemeFixture(t, vsHappyValues, seed),
		"bookworm", "tag", "version")
}

func TestVersionSchemeCheck_SeedNoDefaultRow(t *testing.T) {
	seed := vsMutate(t, vsHappySeed, "isDefault: true", "isDefault: false")
	vsFails(t, writeVersionSchemeFixture(t, vsHappyValues, seed), "default")
}

func TestVersionSchemeCheck_SeedEmpty(t *testing.T) {
	vsFails(t, writeVersionSchemeFixture(t, vsHappyValues, "bases: []\n"), "no base rows")
}

// vsDeliveryBase: values with every platform component digest pinned —
// the coordinated-bump posture in which a rote digest paste becomes
// possible. agentdDelivery carries the controller digest (incident 2).
func vsDeliveryBase(t *testing.T) string {
	t.Helper()
	return `
controller:
  image:
    repository: ghcr.io/lenaxia/llmsafespaces/controller
    tag: ""
    digest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
  agentdDelivery:
    image: ghcr.io/lenaxia/llmsafespaces/agentd@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
    binarySHA256Amd64: ""
    binarySHA256Arm64: ""
  opencodeDelivery:
    image: ""
    binarySHA256Amd64: ""
    binarySHA256Arm64: ""
api:
  image:
    repository: ghcr.io/lenaxia/llmsafespaces/api
    tag: ""
    digest: "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
frontend:
  image:
    repository: ghcr.io/lenaxia/llmsafespaces/frontend
    tag: ""
    digest: "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
runtimeEnvironments:
  base:
    image:
      repository: ghcr.io/lenaxia/llmsafespaces/base
      tag: "2026.09.0"
      digest: ""
`
}

func TestVersionSchemeCheck_AgentdImageDigestEqualsControllerDigest(t *testing.T) {
	vsFails(t, writeVersionSchemeFixture(t, vsDeliveryBase(t), vsHappySeed),
		"agentdDelivery", "controller.image.digest")
}

func TestVersionSchemeCheck_AgentdBinaryPinsEqualControllerDigest(t *testing.T) {
	values := vsMutate(t, vsDeliveryBase(t),
		`    binarySHA256Amd64: ""
    binarySHA256Arm64: ""
  opencodeDelivery:`,
		`    binarySHA256Amd64: aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
    binarySHA256Arm64: aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
  opencodeDelivery:`)
	vsFails(t, writeVersionSchemeFixture(t, values, vsHappySeed),
		"agentdDelivery", "binarySHA256Amd64", "controller.image.digest")
}

func TestVersionSchemeCheck_OpencodeImageDigestEqualsAPIDigest(t *testing.T) {
	values := vsMutate(t, vsDeliveryBase(t),
		`  opencodeDelivery:
    image: ""`,
		`  opencodeDelivery:
    image: ghcr.io/lenaxia/llmsafespaces/opencode@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb`)
	vsFails(t, writeVersionSchemeFixture(t, values, vsHappySeed),
		"opencodeDelivery", "api.image.digest")
}

func TestVersionSchemeCheck_OpencodeBinaryPinsEqualFrontendDigest(t *testing.T) {
	values := vsMutate(t, vsDeliveryBase(t),
		`  opencodeDelivery:
    image: ""
    binarySHA256Amd64: ""
    binarySHA256Arm64: ""`,
		`  opencodeDelivery:
    image: ""
    binarySHA256Amd64: cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc
    binarySHA256Arm64: cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc`)
	vsFails(t, writeVersionSchemeFixture(t, values, vsHappySeed),
		"opencodeDelivery", "frontend.image.digest")
}

func TestVersionSchemeCheck_AgentdDigestEqualsRelayRouterDigest(t *testing.T) {
	const routerDigest = "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
	values := vsMutate(t, vsDeliveryBase(t),
		`  agentdDelivery:
    image: ghcr.io/lenaxia/llmsafespaces/agentd@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa`,
		`  inferenceRelay:
    router:
      image:
        repository: ghcr.io/lenaxia/llmsafespaces/relay-router
        tag: ""
        digest: "sha256:`+routerDigest+`"
  agentdDelivery:
    image: ghcr.io/lenaxia/llmsafespaces/agentd@sha256:`+routerDigest)
	vsFails(t, writeVersionSchemeFixture(t, values, vsHappySeed),
		"agentdDelivery", "router.image.digest")
}

func TestVersionSchemeCheck_AgentdAndOpencodePinsIdentical(t *testing.T) {
	values := vsMutate(t, vsDeliveryBase(t),
		`  opencodeDelivery:
    image: ""`,
		`  opencodeDelivery:
    image: ghcr.io/lenaxia/llmsafespaces/opencode@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa`)
	vsFails(t, writeVersionSchemeFixture(t, values, vsHappySeed),
		"agentdDelivery", "opencodeDelivery")
}

func TestVersionSchemeCheck_DistinctDeliveryPinsPass(t *testing.T) {
	values := `
controller:
  image:
    repository: ghcr.io/lenaxia/llmsafespaces/controller
    tag: ""
    digest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
  agentdDelivery:
    image: "ghcr.io/lenaxia/llmsafespaces/agentd:0.30.1@sha256:1111111111111111111111111111111111111111111111111111111111111111"
    binarySHA256Amd64: "2222222222222222222222222222222222222222222222222222222222222222"
    binarySHA256Arm64: "3333333333333333333333333333333333333333333333333333333333333333"
  opencodeDelivery:
    image: "ghcr.io/lenaxia/llmsafespaces/opencode:1.18.15@sha256:4444444444444444444444444444444444444444444444444444444444444444"
    binarySHA256Amd64: "5555555555555555555555555555555555555555555555555555555555555555"
    binarySHA256Arm64: "6666666666666666666666666666666666666666666666666666666666666666"
api:
  image:
    repository: ghcr.io/lenaxia/llmsafespaces/api
    tag: ""
    digest: ""
frontend:
  image:
    repository: ghcr.io/lenaxia/llmsafespaces/frontend
    tag: ""
    digest: ""
runtimeEnvironments:
  base:
    image:
      repository: ghcr.io/lenaxia/llmsafespaces/base
      tag: "2026.09.0"
      digest: ""
`
	vsClean(t, writeVersionSchemeFixture(t, values, vsHappySeed))
}

func TestVersionSchemeCheck_MissingStructuralKeysFailLoudly(t *testing.T) {
	cases := []struct {
		name   string
		values string
		want   string
	}{
		{
			name: "controller.agentdDelivery missing",
			values: vsMutate(t, vsHappyValues, `  agentdDelivery:
    image: ""
    binarySHA256Amd64: ""
    binarySHA256Arm64: ""
`, ""),
			want: "controller.agentdDelivery",
		},
		{
			name: "controller.opencodeDelivery missing",
			values: vsMutate(t, vsHappyValues, `  opencodeDelivery:
    image: ""
    binarySHA256Amd64: ""
    binarySHA256Arm64: ""
`, ""),
			want: "controller.opencodeDelivery",
		},
		{
			name: "runtimeEnvironments missing",
			values: vsMutate(t, vsHappyValues, `runtimeEnvironments:
  base:
    image:
      repository: ghcr.io/lenaxia/llmsafespaces/base
      tag: "2026.09.0"
      digest: ""
`, ""),
			want: "runtimeEnvironments.base.image",
		},
		{
			name: "api.image missing",
			values: vsMutate(t, vsHappyValues, `api:
  image:
    repository: ghcr.io/lenaxia/llmsafespaces/api
    tag: ""
    digest: ""
`, ""),
			want: "api.image",
		},
		{
			name: "frontend.image missing",
			values: vsMutate(t, vsHappyValues, `frontend:
  image:
    repository: ghcr.io/lenaxia/llmsafespaces/frontend
    tag: ""
    digest: ""
`, ""),
			want: "frontend.image",
		},
		{
			name: "controller.image missing",
			values: vsMutate(t, vsHappyValues, `  image:
    repository: ghcr.io/lenaxia/llmsafespaces/controller
    tag: ""
    digest: ""
`, ""),
			want: "controller.image",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			vsFails(t, writeVersionSchemeFixture(t, tc.values, vsHappySeed), tc.want)
		})
	}
}

func TestVersionSchemeCheck_MissingFiles(t *testing.T) {
	fails := RunVersionSchemeCheck(t.TempDir())
	if len(fails) == 0 {
		t.Fatal("missing values.yaml/seed must fail loudly, not pass vacuously")
	}
	joined := strings.Join(fails, "\n")
	if !strings.Contains(joined, "values.yaml") && !strings.Contains(joined, "catalog.seed.yaml") {
		t.Errorf("failure should name the unreadable file; got: %v", fails)
	}
}

func TestVersionSchemeCheck_UntaggedDeliveryImageStillComparable(t *testing.T) {
	values := vsMutate(t, vsHappyValues,
		`  agentdDelivery:
    image: ""`,
		`  agentdDelivery:
    image: "ghcr.io/lenaxia/llmsafespaces/agentd@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"`)
	values = vsMutate(t, values, `    digest: ""
  agentdDelivery:`, `    digest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
  agentdDelivery:`)
	vsFails(t, writeVersionSchemeFixture(t, values, vsHappySeed),
		"agentdDelivery", "controller.image.digest")
}
