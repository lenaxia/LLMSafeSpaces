// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package settings

import (
	"testing"
)

func TestSchemaVersion(t *testing.T) {
	if SchemaVersion < 1 {
		t.Errorf("SchemaVersion must be >= 1, got %d", SchemaVersion)
	}
}

func TestInstanceSettings_AllHaveTier2(t *testing.T) {
	for _, def := range InstanceSettings() {
		if def.Tier != 2 {
			t.Errorf("instance setting %q has tier %d, expected 2", def.Key, def.Tier)
		}
	}
}

func TestUserSettings_AllHaveTier3(t *testing.T) {
	for _, def := range UserSettings() {
		if def.Tier != 3 {
			t.Errorf("user setting %q has tier %d, expected 3", def.Key, def.Tier)
		}
	}
}

func TestInstanceSettings_UniqueKeys(t *testing.T) {
	seen := make(map[string]bool)
	for _, def := range InstanceSettings() {
		if seen[def.Key] {
			t.Errorf("duplicate instance setting key: %q", def.Key)
		}
		seen[def.Key] = true
	}
}

func TestUserSettings_UniqueKeys(t *testing.T) {
	seen := make(map[string]bool)
	for _, def := range UserSettings() {
		if seen[def.Key] {
			t.Errorf("duplicate user setting key: %q", def.Key)
		}
		seen[def.Key] = true
	}
}

func TestAllSettings_NoKeyOverlap(t *testing.T) {
	seen := make(map[string]bool)
	for _, def := range AllSettings() {
		if seen[def.Key] {
			t.Errorf("key %q appears in both instance and user settings", def.Key)
		}
		seen[def.Key] = true
	}
}

func TestInstanceSettings_DefaultsPassValidation(t *testing.T) {
	for _, def := range InstanceSettings() {
		if err := Validate(def, def.Default); err != nil {
			t.Errorf("instance setting %q default fails validation: %v", def.Key, err)
		}
	}
}

func TestUserSettings_DefaultsPassValidation(t *testing.T) {
	for _, def := range UserSettings() {
		if err := Validate(def, def.Default); err != nil {
			t.Errorf("user setting %q default fails validation: %v", def.Key, err)
		}
	}
}

func TestAllSettings_HaveRequiredFields(t *testing.T) {
	for _, def := range AllSettings() {
		if def.Key == "" {
			t.Error("setting with empty key")
		}
		if def.Type == "" {
			t.Errorf("setting %q has empty type", def.Key)
		}
		if def.Category == "" {
			t.Errorf("setting %q has empty category", def.Key)
		}
		if def.Label == "" {
			t.Errorf("setting %q has empty label", def.Key)
		}
		if def.Description == "" {
			t.Errorf("setting %q has empty description", def.Key)
		}
		if def.Default == nil {
			t.Errorf("setting %q has nil default", def.Key)
		}
	}
}

func TestInstanceSettingIndex(t *testing.T) {
	idx := InstanceSettingIndex()
	if len(idx) != len(InstanceSettings()) {
		t.Errorf("index has %d entries, expected %d", len(idx), len(InstanceSettings()))
	}
	def, ok := idx["auth.registrationEnabled"]
	if !ok {
		t.Fatal("auth.registrationEnabled not in index")
	}
	if def.Type != TypeBool {
		t.Errorf("expected TypeBool, got %v", def.Type)
	}
}

func TestUserSettingIndex(t *testing.T) {
	idx := UserSettingIndex()
	if len(idx) != len(UserSettings()) {
		t.Errorf("index has %d entries, expected %d", len(idx), len(UserSettings()))
	}
	def, ok := idx["theme"]
	if !ok {
		t.Fatal("theme not in index")
	}
	if def.Type != TypeEnum {
		t.Errorf("expected TypeEnum, got %v", def.Type)
	}
}

// TestSendOnEnterDefaultFalse pins the Composer's send-key behavior contract.
// The frontend Composer reads this default via useUserSetting("sendOnEnter", false),
// and the schema default is the source of truth served to the admin UI. A silent
// flip back to true would reintroduce "Enter sends" as the default — breaking
// the desktop newline-first UX shipped in the composer-enter-history PR (#504).
// This test fails loudly if a future change reverts the default.
func TestSendOnEnterDefaultFalse(t *testing.T) {
	idx := UserSettingIndex()
	def, ok := idx["sendOnEnter"]
	if !ok {
		t.Fatal("sendOnEnter missing from UserSettings")
	}
	if def.Type != TypeBool {
		t.Fatalf("sendOnEnter type = %v, want TypeBool", def.Type)
	}
	if def.Default != false {
		t.Errorf("sendOnEnter default = %v, want false (desktop Enter is newline by default; Ctrl/Cmd+Enter sends)", def.Default)
	}
}

func TestValidate_Bool_Happy(t *testing.T) {
	def := SettingDef{Key: "test", Type: TypeBool, Default: true}
	if err := Validate(def, true); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if err := Validate(def, false); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestValidate_Bool_WrongType(t *testing.T) {
	def := SettingDef{Key: "test", Type: TypeBool, Default: true}
	if err := Validate(def, "true"); err == nil {
		t.Error("expected error for string value on bool setting")
	}
	if err := Validate(def, 1); err == nil {
		t.Error("expected error for int value on bool setting")
	}
}

func TestValidate_Int_Happy(t *testing.T) {
	def := SettingDef{Key: "test", Type: TypeInt, Default: 5, Min: intPtr(1), Max: intPtr(100)}
	if err := Validate(def, 50); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if err := Validate(def, 1); err != nil {
		t.Errorf("unexpected error for min boundary: %v", err)
	}
	if err := Validate(def, 100); err != nil {
		t.Errorf("unexpected error for max boundary: %v", err)
	}
}

func TestValidate_Int_BelowMin(t *testing.T) {
	def := SettingDef{Key: "test", Type: TypeInt, Default: 5, Min: intPtr(1), Max: intPtr(100)}
	if err := Validate(def, 0); err == nil {
		t.Error("expected error for value below min")
	}
}

func TestValidate_Int_AboveMax(t *testing.T) {
	def := SettingDef{Key: "test", Type: TypeInt, Default: 5, Min: intPtr(1), Max: intPtr(100)}
	if err := Validate(def, 101); err == nil {
		t.Error("expected error for value above max")
	}
}

func TestValidate_Int_WrongType(t *testing.T) {
	def := SettingDef{Key: "test", Type: TypeInt, Default: 5}
	if err := Validate(def, "5"); err == nil {
		t.Error("expected error for string value on int setting")
	}
}

func TestValidate_Int_Float64FromJSON(t *testing.T) {
	def := SettingDef{Key: "test", Type: TypeInt, Default: 5, Min: intPtr(1), Max: intPtr(100)}
	// JSON unmarshals numbers as float64
	if err := Validate(def, float64(50)); err != nil {
		t.Errorf("unexpected error for float64(50): %v", err)
	}
	// Non-integer float64 should fail
	if err := Validate(def, 5.5); err == nil {
		t.Error("expected error for non-integer float64")
	}
}

func TestValidate_String_Happy(t *testing.T) {
	def := SettingDef{Key: "test", Type: TypeString, Default: "1Gi", Pattern: `^[0-9]+(Gi|Mi)$`}
	if err := Validate(def, "1Gi"); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if err := Validate(def, "512Mi"); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestValidate_String_PatternMismatch(t *testing.T) {
	def := SettingDef{Key: "test", Type: TypeString, Default: "1Gi", Pattern: `^[0-9]+(Gi|Mi)$`}
	if err := Validate(def, "invalid"); err == nil {
		t.Error("expected error for pattern mismatch")
	}
}

func TestValidate_String_NoPattern(t *testing.T) {
	def := SettingDef{Key: "test", Type: TypeString, Default: ""}
	if err := Validate(def, "anything goes"); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestValidate_String_WrongType(t *testing.T) {
	def := SettingDef{Key: "test", Type: TypeString, Default: ""}
	if err := Validate(def, 123); err == nil {
		t.Error("expected error for int value on string setting")
	}
}

func TestValidate_Enum_Happy(t *testing.T) {
	def := SettingDef{Key: "test", Type: TypeEnum, Default: "a", Enum: []string{"a", "b", "c"}}
	if err := Validate(def, "a"); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if err := Validate(def, "c"); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestValidate_Enum_InvalidValue(t *testing.T) {
	def := SettingDef{Key: "test", Type: TypeEnum, Default: "a", Enum: []string{"a", "b", "c"}}
	if err := Validate(def, "d"); err == nil {
		t.Error("expected error for invalid enum value")
	}
}

func TestValidate_Enum_WrongType(t *testing.T) {
	def := SettingDef{Key: "test", Type: TypeEnum, Default: "a", Enum: []string{"a", "b"}}
	if err := Validate(def, 1); err == nil {
		t.Error("expected error for int value on enum setting")
	}
}

func TestValidate_Strings_Happy(t *testing.T) {
	def := SettingDef{Key: "test", Type: TypeStrings, Default: []string{}}
	if err := Validate(def, []string{"a", "b"}); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if err := Validate(def, []string{}); err != nil {
		t.Errorf("unexpected error for empty slice: %v", err)
	}
}

func TestValidate_Strings_AnySliceFromJSON(t *testing.T) {
	def := SettingDef{Key: "test", Type: TypeStrings, Default: []string{}}
	// JSON unmarshals to []interface{}
	if err := Validate(def, []any{"a", "b"}); err != nil {
		t.Errorf("unexpected error for []any: %v", err)
	}
}

func TestValidate_Strings_AnySliceWithNonString(t *testing.T) {
	def := SettingDef{Key: "test", Type: TypeStrings, Default: []string{}}
	if err := Validate(def, []any{"a", 123}); err == nil {
		t.Error("expected error for non-string element in []any")
	}
}

func TestValidate_Strings_WrongType(t *testing.T) {
	def := SettingDef{Key: "test", Type: TypeStrings, Default: []string{}}
	if err := Validate(def, "not a slice"); err == nil {
		t.Error("expected error for string value on strings setting")
	}
}

func TestValidate_UnknownType(t *testing.T) {
	def := SettingDef{Key: "test", Type: "unknown", Default: "x"}
	if err := Validate(def, "x"); err == nil {
		t.Error("expected error for unknown type")
	}
}

// ---------------------------------------------------------------------------
// Resource-quantity validation (regression tests for the "8gi" bug)
// ---------------------------------------------------------------------------
//
// Background: an admin saved `workspace.defaultResources.memory = "8gi"`
// (lowercase "gi") via the admin settings UI. The setting had no Pattern,
// so the value was accepted into the database. On the next workspace
// creation, the API service piped the lowercase value into the
// Workspace CRD's spec.resources.memory, which the validating webhook
// rejected with the cryptic message:
//
//   spec.resources.memory "8gi": memory "8gi" does not match ^[0-9]+(Ki|Mi|Gi)$
//
// User-visible: the "Create workspace" button stopped working for
// every user, with an internal_error toast that didn't say what was
// wrong. Diagnosis required tracing all the way to the webhook.
//
// Root cause: the schema in pkg/settings/schema.go declared the
// `workspace.defaultResources.{cpu,memory}` settings with no Pattern,
// so any string the admin typed was accepted at save time. The
// constraint was discovered eight code paths downstream.
//
// Fix: every setting that ends up as a Kubernetes Quantity-shaped
// field on the Workspace CRD must declare a Pattern that matches the
// CRD/webhook regex. These tests pin that contract.

// Pattern source-of-truth verification. The canonical patterns live
// in pkg/settings/quantity_patterns.go and are referenced by:
//
//   - The settings schema (pkg/settings/schema.go) — verified here.
//   - The validating webhook (controller/internal/webhooks/workspace_webhook.go)
//     — verified by TestWebhookRegexAcceptsSameInputsAsSettingsPattern
//     in that package, which imports this package's constants and
//     probes both regexes with the same inputs.
//
// Together they ensure that anything the admin can save through the
// settings UI will also pass the webhook. The original bug ("8gi"
// passes settings, fails webhook) cannot recur as long as both tests
// pass.

// resourceQuantitySettings are the keys that end up on a CRD as a
// Kubernetes Quantity. Each MUST declare a Pattern sourced from the
// canonical pkg/settings constants. If you add a new resource setting
// (e.g. workspace.defaultResources.ephemeralStorage) add it here so
// the contract is enforced.
var resourceQuantitySettings = map[string]string{
	"workspace.defaultResources.memory": MemoryQuantityPattern,
	"workspace.defaultResources.cpu":    CPUQuantityPattern,
	"workspace.defaultStorageSize":      StorageQuantityPattern,
}

func TestInstanceSettings_ResourceQuantitiesHavePatterns(t *testing.T) {
	idx := InstanceSettingIndex()
	for key, expected := range resourceQuantitySettings {
		def, ok := idx[key]
		if !ok {
			t.Errorf("setting %q in resourceQuantitySettings but not in InstanceSettings", key)
			continue
		}
		if def.Pattern == "" {
			t.Errorf("setting %q has no Pattern; admins can save invalid values "+
				"that the validating webhook will reject at workspace-create time. "+
				"Expected pattern %q.", key, expected)
		}
	}
}

func TestInstanceSettings_ResourcePatternsUseCanonicalConstants(t *testing.T) {
	// Drift guard A (schema ↔ canonical): the schema must reference
	// the constants from quantity_patterns.go, not literal strings.
	// If a developer changes either the canonical constant or the
	// schema pattern in isolation, this test fires.
	//
	// Drift guard B (canonical ↔ webhook) is enforced in the
	// webhook's own test file by importing this package's constants
	// and asserting the webhook's regex variables accept the same
	// set of inputs.
	idx := InstanceSettingIndex()
	for key, expected := range resourceQuantitySettings {
		def, ok := idx[key]
		if !ok {
			continue // covered by TestInstanceSettings_ResourceQuantitiesHavePatterns
		}
		if def.Pattern != expected {
			t.Errorf("setting %q pattern %q does not equal canonical constant %q. "+
				"Use the constant from pkg/settings/quantity_patterns.go directly so the "+
				"webhook, schema, and frontend all share one source of truth.",
				key, def.Pattern, expected)
		}
	}
}

func TestValidate_Memory_RejectsLowercaseUnit(t *testing.T) {
	// Direct regression test for the "8gi" bug. The setting Validate
	// path must reject lowercase unit suffixes — they're not valid
	// Kubernetes Quantity strings and the apiserver/webhook rejects
	// them downstream.
	idx := InstanceSettingIndex()
	def, ok := idx["workspace.defaultResources.memory"]
	if !ok {
		t.Fatal("workspace.defaultResources.memory missing from InstanceSettings")
	}
	if def.Pattern == "" {
		t.Fatal("workspace.defaultResources.memory has no Pattern (covered by " +
			"TestInstanceSettings_ResourceQuantitiesHavePatterns; " +
			"this test cannot proceed without the fix)")
	}

	rejected := []string{
		"8gi",    // the actual bug
		"8GB",    // common mistake; not a k8s suffix
		"8 Gi",   // whitespace
		"8.5Gi",  // floating point not allowed by k8s memory suffixes
		"8gib",   // lowercase, full word
		"banana", // anything goes when there's no pattern
		"",       // empty string accepted by no-pattern; should not be saved as a default
		"-1Gi",   // negative
		"8",      // bare number, no unit
		// Zero-magnitude values are rejected by the webhook's
		// parseMemoryMi (which requires n >= 1) but a naive regex
		// `^[0-9]+(Ki|Mi|Gi)$` accepts them. Same failure class as
		// the original "8gi" bug: passes settings, breaks workspace
		// creation. The schema pattern must reject them too.
		"0Gi",
		"0Mi",
		"0Ki",
		"00Gi", // leading zeros — "[1-9][0-9]*" rejects
	}
	for _, v := range rejected {
		if err := Validate(def, v); err == nil {
			t.Errorf("Validate accepted invalid memory value %q; webhook will reject "+
				"it later, breaking workspace creation for every user", v)
		}
	}

	accepted := []string{"512Mi", "1Gi", "8Gi", "16Gi", "1024Ki"}
	for _, v := range accepted {
		if err := Validate(def, v); err != nil {
			t.Errorf("Validate rejected valid memory value %q: %v", v, err)
		}
	}
}

func TestValidate_CPU_RejectsBogusValues(t *testing.T) {
	idx := InstanceSettingIndex()
	def, ok := idx["workspace.defaultResources.cpu"]
	if !ok {
		t.Fatal("workspace.defaultResources.cpu missing from InstanceSettings")
	}
	if def.Pattern == "" {
		t.Fatal("workspace.defaultResources.cpu has no Pattern")
	}

	rejected := []string{
		"banana",
		"1 core",
		"1000M", // capital M is not millicores
		"",
		"-500m",
		"100%",
		// Zero-magnitude values: closes the parallel gap that the
		// memory/storage tightening missed. Webhook's parseCPUMillis
		// has no n<1 check (unlike parseMemoryMi/storageSizeGi), so
		// "0m" and "0.0" would parse to 0 millicores and reach
		// Kubernetes apiserver, which rejects requests.cpu == 0 with
		// a less-helpful error than our admission webhook.
		"0m",
		"0.0",
		"0.00",
		"0",  // bare 0
		"0.", // dot but no fractional digits
	}
	for _, v := range rejected {
		if err := Validate(def, v); err == nil {
			t.Errorf("Validate accepted invalid CPU value %q", v)
		}
	}

	accepted := []string{
		"500m", "1000m", "1.0", "0.5", "16.0",
		// Edge cases that look zero-ish but aren't:
		"0.001", // 1 millicore expressed as decimal
		"1m",    // smallest millicore
	}
	for _, v := range accepted {
		if err := Validate(def, v); err != nil {
			t.Errorf("Validate rejected valid CPU value %q: %v", v, err)
		}
	}
}

func TestValidate_StorageSize_RejectsBogusValues(t *testing.T) {
	idx := InstanceSettingIndex()
	def, ok := idx["workspace.defaultStorageSize"]
	if !ok {
		t.Fatal("workspace.defaultStorageSize missing from InstanceSettings")
	}
	// Pattern was already present pre-fix; treat this as a drift guard.
	// Zero-magnitude values added because the webhook's storageSizeGi
	// rejects n < 1.
	rejected := []string{"15gi", "15Ti", "15GB", "banana", "", "-1Gi", "0Gi", "0Mi"}
	for _, v := range rejected {
		if err := Validate(def, v); err == nil {
			t.Errorf("Validate accepted invalid storage value %q", v)
		}
	}
	accepted := []string{"15Gi", "100Gi", "1024Mi"}
	for _, v := range accepted {
		if err := Validate(def, v); err != nil {
			t.Errorf("Validate rejected valid storage value %q: %v", v, err)
		}
	}
}
