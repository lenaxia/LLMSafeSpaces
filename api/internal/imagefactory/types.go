// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package imagefactory

import (
	"sort"
	"time"
)

// ExtensionType enumerates the catalog extension kinds. "run" and "env"
// are deliberately absent — they were the injection vectors (design/0046 #5).
type ExtensionType string

const (
	ExtensionTypeApt  ExtensionType = "apt"
	ExtensionTypeMise ExtensionType = "mise"
	ExtensionTypeFile ExtensionType = "file"
)

// FileSpec is the target for a type=file extension. Path must be absolute
// (no traversal); Mode defaults to "0644" when empty.
type FileSpec struct {
	Path string `json:"path"`
	Mode string `json:"mode,omitempty"` // octal string "0755"; empty → "0644"
}

// Extension is a catalog row. Immutable-once-published (design/0046 #7):
// the build-relevant fields (Type, Value, FileSpec, SupportedBases) do not
// change after creation. Only Retired, ReviewRequested, Description mutate.
type Extension struct {
	ID              string        `json:"id"`
	Type            ExtensionType `json:"type"`
	Value           string        `json:"value"`
	FileSpec        *FileSpec     `json:"fileSpec,omitempty"`
	SupportedBases  []string      `json:"supportedBases"`
	Retired         bool          `json:"retired"`
	ReviewRequested bool          `json:"reviewRequested"`
	Description     string        `json:"description,omitempty"`
}

// Base is a (name, version) row of an operator-approved base image.
// Composite-keyed: old versions persist (design/0046 #8).
type Base struct {
	Name      string `json:"name" yaml:"name"`
	Version   string `json:"version" yaml:"version"`
	Image     string `json:"image" yaml:"image"`
	Tag       string `json:"tag,omitempty" yaml:"tag,omitempty"`
	Digest    string `json:"digest,omitempty" yaml:"digest,omitempty"`
	IsDefault bool   `json:"isDefault" yaml:"isDefault"`
}

// Ref returns the pullable reference. Digest wins over tag; if neither is
// set, just the bare image (caller's bug, but don't panic).
func (b Base) Ref() string {
	if b.Digest != "" {
		return b.Image + "@" + b.Digest
	}
	if b.Tag != "" {
		return b.Image + ":" + b.Tag
	}
	return b.Image
}

// PlatformConfig is the single-row platform-level factory config.
type PlatformConfig struct {
	Architectures []string `json:"architectures"`
}

// ResolvedValue is one entry in the resolved_values JSONB. It is the
// cached projection of an Extension's build fields, frozen at config-save
// time. Shape pinned per design/0047.
type ResolvedValue struct {
	Type     ExtensionType `json:"type"`
	Value    string        `json:"value"`
	FileSpec *FileSpec     `json:"fileSpec,omitempty"`
}

// ResolvedValues maps extension ID → frozen resolved value. This is the
// exact JSONB shape stored on image_factory_configs.resolved_values and
// image_factory_builds.resolved_values.
type ResolvedValues map[string]ResolvedValue

// Selection recovers the extension IDs from the resolved values map, sorted.
// Used by the callback handler to reconstruct the selection for the
// known_failures row (the build stores resolved_values, not the selection).
func (rv ResolvedValues) Selection() Selection {
	ids := make(Selection, 0, len(rv))
	for id := range rv {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// Selection is the user's pick: an unordered set of extension IDs. Hashing
// sorts, so input order does not affect the result.
type Selection []string

// ConfigScope enumerates the three friendly-name scopes (design/0046 #25).
type ConfigScope string

const (
	ScopeMember   ConfigScope = "member"
	ScopeOrg      ConfigScope = "org"
	ScopePlatform ConfigScope = "platform"
)

// ConfigStatus is the config lifecycle pill (design/0046 #20).
type ConfigStatus string

const (
	StatusBuilding ConfigStatus = "building"
	StatusReady    ConfigStatus = "ready"
	StatusRejected ConfigStatus = "rejected"
)

// Config is a saved user/org/platform config.
type Config struct {
	ID             string         `json:"id"`
	Hash           string         `json:"hash"`
	Name           string         `json:"name"`
	Selection      Selection      `json:"selection"`
	ResolvedValues ResolvedValues `json:"resolvedValues"`
	BaseName       string         `json:"baseName"`
	BaseVersion    string         `json:"baseVersion"`
	Scope          ConfigScope    `json:"scope"`
	OwnerID        *string        `json:"ownerId,omitempty"`
	OrgID          *string        `json:"orgId,omitempty"`
	Status         ConfigStatus   `json:"status"`
}

// KnownFailure is a row of image_factory_known_failures — the blocklist.
type KnownFailure struct {
	SelectionHash string    `json:"selectionHash"`
	Selection     Selection `json:"selection"`
	BaseName      string    `json:"baseName"`
	Explanation   string    `json:"explanation,omitempty"`
	FailureReason string    `json:"-"` // admin-only; never serialized to non-admins
	DetectedAt    time.Time `json:"detectedAt"`
	Retriable     bool      `json:"retriable"`
}

// BuildStatus enumerates the build lifecycle.
type BuildStatus string

const (
	BuildDispatched BuildStatus = "dispatched"
	BuildSucceeded  BuildStatus = "succeeded"
	BuildFailed     BuildStatus = "failed"
)

// Build is one row of image_factory_builds. One row per API dispatch —
// transient retry happens inside the GH Actions workflow (design/0046 #12),
// so the API sees exactly one dispatch + one final result.
type Build struct {
	ID             string         `json:"id"`
	ConfigID       string         `json:"configId"`
	Hash           string         `json:"hash"`
	BaseName       string         `json:"baseName"`
	BaseVersion    string         `json:"baseVersion"`
	ResolvedValues ResolvedValues `json:"resolvedValues"`
	Architectures  []string       `json:"architectures"`
	ImageRef       string         `json:"imageRef,omitempty"`
	Digest         string         `json:"digest,omitempty"`
	Status         BuildStatus    `json:"status"`
	GHRunID        *int64         `json:"ghRunId,omitempty"`
	FailureReason  string         `json:"-"` // admin-only
	Explanation    string         `json:"explanation,omitempty"`
	TriggeredBy    *string        `json:"triggeredBy,omitempty"`
	StartedAt      time.Time      `json:"startedAt"`
	FinishedAt     *time.Time     `json:"finishedAt,omitempty"`
	CallbackToken  string         `json:"-"` // per-build secret; ConstantTimeCompare on callback
}
