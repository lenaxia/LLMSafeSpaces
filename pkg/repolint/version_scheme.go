// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package repolint

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// Version-scheme guard (issue #1237): the repo's versioned components
// ride THREE divergent schemes — platform semver (Chart.yaml appVersion
// + release tag), digest-pinned overlay artifacts (agentdDelivery /
// opencodeDelivery), and the CalVer base (design 0053 D5/S4). Three
// incidents in four days (2026-08-29 → 2026-09-02) came from one class:
// a coordinated bump applying the platform VERSION to a component that
// is not on it. This check makes the two mechanically-checkable
// invariants fail at commit time:
//
//  (a) the base is CalVer `YYYY.MM.x` and the chart default mirrors the
//      catalog seed row — the single source (incident 2026-09-02: the
//      0.26.0 bump set base:0.26.0, a tag that never existed →
//      fleet-wide ImagePullBackOff);
//  (b) delivery pins never reuse a platform component's digest
//      (incident 2026-09-01: agentd per-arch lines refreshed with a
//      controller digest — the sed-by-rote class).
//
// The check reads the committed helm/values.yaml and the seed. It
// cannot see operator/config-repo values; those are covered by the
// procedure in docs/release-bump-checklist.md. Missing structural keys
// fail loudly (parser drift must not degrade into a vacuous pass), and
// empty digests on both sides of a comparison are skipped — chart
// defaults carry no pins.

// calverRe matches the base's content-version scheme: 4-digit year,
// 2-digit month (01-12), numeric patch — design 0053 D5/S4. Anything
// else (notably a platform semver like 0.26.0) fails.
var calverRe = regexp.MustCompile(`^\d{4}\.(0[1-9]|1[0-2])\.\d+$`)

// digestHexRe matches a sha256 digest value with or without the
// algorithm prefix, as it appears in a digest field or image ref.
var digestHexRe = regexp.MustCompile(`^(?:sha256:)?([0-9a-f]{64})$`)

type versionSchemeComponent struct {
	Repository string `yaml:"repository"`
	Tag        string `yaml:"tag"`
	Digest     string `yaml:"digest"`
}

type versionSchemeDelivery struct {
	Image             string `yaml:"image"`
	BinarySHA256Amd64 string `yaml:"binarySHA256Amd64"`
	BinarySHA256Arm64 string `yaml:"binarySHA256Arm64"`
}

type versionSchemeValues struct {
	API struct {
		Image versionSchemeComponent `yaml:"image"`
	} `yaml:"api"`
	Frontend struct {
		Image versionSchemeComponent `yaml:"image"`
	} `yaml:"frontend"`
	Controller struct {
		Image            versionSchemeComponent `yaml:"image"`
		AgentdDelivery   versionSchemeDelivery  `yaml:"agentdDelivery"`
		OpencodeDelivery versionSchemeDelivery  `yaml:"opencodeDelivery"`
		InferenceRelay   struct {
			Router struct {
				Image versionSchemeComponent `yaml:"image"`
			} `yaml:"router"`
		} `yaml:"inferenceRelay"`
	} `yaml:"controller"`
	RuntimeEnvironments struct {
		Base struct {
			Image versionSchemeComponent `yaml:"image"`
		} `yaml:"base"`
	} `yaml:"runtimeEnvironments"`
}

type versionSchemeSeedBase struct {
	Name      string `yaml:"name"`
	Version   string `yaml:"version"`
	Tag       string `yaml:"tag"`
	IsDefault bool   `yaml:"isDefault"`
}

type versionSchemeSeed struct {
	Bases []versionSchemeSeedBase `yaml:"bases"`
}

// RunVersionSchemeCheck returns one failure line per violated
// invariant. Empty = clean.
func RunVersionSchemeCheck(root string) []string {
	var fails []string

	valuesPath := filepath.Join(root, "helm", "values.yaml")
	seedPath := filepath.Join(root, "api", "internal", "imagefactory", "catalog.seed.yaml")

	valuesRaw, err := os.ReadFile(valuesPath) //nolint:gosec // repo-rooted lint scan, same trust domain as release_artifacts.go
	if err != nil {
		return []string{fmt.Sprintf("version scheme: cannot read %s: %v", valuesPath, err)}
	}
	seedRaw, err := os.ReadFile(seedPath) //nolint:gosec // repo-rooted lint scan, same trust domain as release_artifacts.go
	if err != nil {
		return []string{fmt.Sprintf("version scheme: cannot read %s: %v", seedPath, err)}
	}

	var values versionSchemeValues
	if err := yaml.Unmarshal(valuesRaw, &values); err != nil {
		return []string{fmt.Sprintf("version scheme: cannot parse %s: %v", valuesPath, err)}
	}
	var seed versionSchemeSeed
	if err := yaml.Unmarshal(seedRaw, &seed); err != nil {
		return []string{fmt.Sprintf("version scheme: cannot parse %s: %v", seedPath, err)}
	}

	// Structural presence: every key the comparisons depend on must
	// exist at its real path. A renamed key would otherwise zero-value
	// its way to a vacuous pass.
	var shape map[string]any
	if err := yaml.Unmarshal(valuesRaw, &shape); err != nil {
		return []string{fmt.Sprintf("version scheme: cannot parse %s: %v", valuesPath, err)}
	}
	for _, path := range [][]string{
		{"api", "image"},
		{"frontend", "image"},
		{"controller", "image"},
		{"controller", "agentdDelivery"},
		{"controller", "opencodeDelivery"},
		{"runtimeEnvironments", "base", "image"},
	} {
		if !hasYAMLPath(shape, path...) {
			fails = append(fails, fmt.Sprintf(
				"version scheme: %s is missing %s — the version-scheme guard cannot run (key renamed?); fix the structure",
				valuesPath, strings.Join(path, ".")))
		}
	}

	fails = append(fails, checkSeedCalVer(seedPath, seed)...)
	fails = append(fails, checkBaseTagSingleSource(valuesPath, values, seed)...)
	fails = append(fails, checkDeliveryPinDigests(valuesPath, values)...)
	return fails
}

func hasYAMLPath(m map[string]any, path ...string) bool {
	cur := m
	for i, key := range path {
		v, ok := cur[key]
		if !ok {
			return false
		}
		if i == len(path)-1 {
			return true
		}
		next, ok := v.(map[string]any)
		if !ok {
			return false
		}
		cur = next
	}
	return true
}

// checkSeedCalVer enforces the CalVer scheme on every catalog seed base
// row and the row-internal version==tag identity (base-image.yml
// publishes the version AS the tag; a divergent tag would make the
// factory reference an image the workflow never pushed).
func checkSeedCalVer(seedPath string, seed versionSchemeSeed) []string {
	var fails []string
	if len(seed.Bases) == 0 {
		return []string{fmt.Sprintf("version scheme: %s has no base rows — parser drift?", seedPath)}
	}
	for _, b := range seed.Bases {
		if !calverRe.MatchString(b.Version) {
			fails = append(fails, fmt.Sprintf(
				"version scheme: %s bases[%s].version %q is not CalVer YYYY.MM.x — the base is content-versioned off the platform train (design 0053 D5/S4, incident 2026-09-02); never a platform semver",
				seedPath, b.Name, b.Version))
		}
		if b.Tag == "" {
			continue
		}
		if !calverRe.MatchString(b.Tag) {
			fails = append(fails, fmt.Sprintf(
				"version scheme: %s bases[%s].tag %q is not CalVer YYYY.MM.x — base-image.yml publishes the version as the tag (design 0053 D5/S4)",
				seedPath, b.Name, b.Tag))
			continue
		}
		if b.Tag != b.Version {
			fails = append(fails, fmt.Sprintf(
				"version scheme: %s bases[%s] tag %q != version %q — the workflow publishes the version as the tag; a split row references an image that is never built",
				seedPath, b.Name, b.Tag, b.Version))
		}
	}
	return fails
}

// checkBaseTagSingleSource enforces the chart-default base tag mirror:
// equal to the seed's default row version. The seed row is the single
// source (base-image.yml header); values.yaml mirrors it so the
// default-rendered RuntimeEnvironment references a tag that exists.
// A digest-pinned base skips the tag checks (digest pin wins).
func checkBaseTagSingleSource(valuesPath string, values versionSchemeValues, seed versionSchemeSeed) []string {
	base := values.RuntimeEnvironments.Base.Image
	tagPath := valuesPath + " runtimeEnvironments.base.image.tag"
	if base.Digest != "" {
		return nil
	}
	var defaults []versionSchemeSeedBase
	for _, b := range seed.Bases {
		if b.IsDefault {
			defaults = append(defaults, b)
		}
	}
	switch len(defaults) {
	case 1:
	case 0:
		return []string{"version scheme: no isDefault base row in the catalog seed — the chart default has no single source to mirror"}
	default:
		return []string{fmt.Sprintf(
			"version scheme: %d isDefault base rows in the catalog seed — the single source is ambiguous", len(defaults))}
	}
	want := defaults[0].Version
	if base.Tag == "" {
		return []string{fmt.Sprintf(
			"version scheme: %s is empty — mirror the catalog seed default row (%s %q); an empty tag must never fall back to the platform appVersion (incident 2026-09-02)",
			tagPath, defaults[0].Name, want)}
	}
	if !calverRe.MatchString(base.Tag) {
		return []string{fmt.Sprintf(
			"version scheme: %s %q is not CalVer YYYY.MM.x — the base is content-versioned off the platform train (design 0053 D5/S4, incident 2026-09-02: base:0.26.0 never existed); the seed row is the source of truth",
			tagPath, base.Tag)}
	}
	if base.Tag != want {
		return []string{fmt.Sprintf(
			"version scheme: base tag drift: %s says %q, the catalog seed default row (%s) says %q — bump both in the same PR (base-image.yml publishes the seed row; the chart default must reference an existing tag)",
			tagPath, base.Tag, defaults[0].Name, want)}
	}
	return nil
}

// checkDeliveryPinDigests fails when any overlay-delivery pin (the
// image digest or a per-arch binary sha256) equals a platform
// component's image digest. The pins come from the release workflow's
// merge-agentd / merge-opencode printed values blocks — never from a
// platform component (incident 2026-09-01: agentd per-arch lines
// refreshed with a controller digest).
func checkDeliveryPinDigests(valuesPath string, values versionSchemeValues) []string {
	platform := map[string]string{
		bareDigest(values.Controller.Image.Digest):                       "controller.image.digest",
		bareDigest(values.API.Image.Digest):                              "api.image.digest",
		bareDigest(values.Frontend.Image.Digest):                         "frontend.image.digest",
		bareDigest(values.Controller.InferenceRelay.Router.Image.Digest): "controller.inferenceRelay.router.image.digest",
	}
	delete(platform, "")

	type pin struct {
		path string
		hex  string
	}
	pins := []pin{
		{"controller.agentdDelivery.image", imageRefDigest(values.Controller.AgentdDelivery.Image)},
		{"controller.opencodeDelivery.image", imageRefDigest(values.Controller.OpencodeDelivery.Image)},
		{"controller.agentdDelivery.binarySHA256Amd64", bareDigest(values.Controller.AgentdDelivery.BinarySHA256Amd64)},
		{"controller.agentdDelivery.binarySHA256Arm64", bareDigest(values.Controller.AgentdDelivery.BinarySHA256Arm64)},
		{"controller.opencodeDelivery.binarySHA256Amd64", bareDigest(values.Controller.OpencodeDelivery.BinarySHA256Amd64)},
		{"controller.opencodeDelivery.binarySHA256Arm64", bareDigest(values.Controller.OpencodeDelivery.BinarySHA256Arm64)},
	}

	var fails []string
	for _, p := range pins {
		if p.hex == "" {
			continue
		}
		if src, ok := platform[p.hex]; ok {
			fails = append(fails, fmt.Sprintf(
				"version scheme: %s pins digest sha256:%s which equals %s — delivery pins come from the merge-agentd/merge-opencode values blocks, never a platform component digest (incident 2026-09-01)",
				p.path, p.hex, src))
		}
	}

	agentd := imageRefDigest(values.Controller.AgentdDelivery.Image)
	opencode := imageRefDigest(values.Controller.OpencodeDelivery.Image)
	if agentd != "" && agentd == opencode {
		fails = append(fails, fmt.Sprintf(
			"version scheme: controller.agentdDelivery.image and controller.opencodeDelivery.image carry the same digest sha256:%s — they are separate artifacts by design (design 0053 §5: independent rollback and cadence)",
			agentd))
	}
	return fails
}

// imageRefDigest extracts the bare sha256 hex from a tag+digest image
// reference (repo:tag@sha256:hex or repo@sha256:hex). Empty when the
// ref carries no digest.
func imageRefDigest(ref string) string {
	at := strings.LastIndex(ref, "@")
	if at < 0 {
		return ""
	}
	return bareDigest(ref[at+1:])
}

// bareDigest normalizes "sha256:<hex>" and bare "<hex>" to bare hex;
// anything else (including empty) becomes "".
func bareDigest(v string) string {
	if m := digestHexRe.FindStringSubmatch(strings.TrimSpace(v)); m != nil {
		return m[1]
	}
	return ""
}
