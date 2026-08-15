import { describe, it, expect } from "vitest";
import { normalizeSettingValue } from "./settingsNormalize";

// Mirror of pkg/settings/normalize_test.go on the backend. If you
// change the canonicalization rules in either place, update both —
// the wire payload from a curl client and from the UI must agree.
describe("normalizeSettingValue", () => {
  describe("memory", () => {
    const key = "workspace.defaultResources.memory";

    it("auto-corrects lowercase units (the production bug)", () => {
      expect(normalizeSettingValue(key, "8gi")).toBe("8Gi");
      expect(normalizeSettingValue(key, "8mi")).toBe("8Mi");
      expect(normalizeSettingValue(key, "8ki")).toBe("8Ki");
    });

    it("auto-corrects GB → Gi (binary unit assumed)", () => {
      expect(normalizeSettingValue(key, "8GB")).toBe("8Gi");
      expect(normalizeSettingValue(key, "8gb")).toBe("8Gi");
      expect(normalizeSettingValue(key, "8gB")).toBe("8Gi");
      expect(normalizeSettingValue(key, "8MB")).toBe("8Mi");
      expect(normalizeSettingValue(key, "8KB")).toBe("8Ki");
    });

    it("trims whitespace and tolerates internal spacing", () => {
      expect(normalizeSettingValue(key, "  8Gi  ")).toBe("8Gi");
      expect(normalizeSettingValue(key, "8 Gi")).toBe("8Gi");
      expect(normalizeSettingValue(key, "8 GB")).toBe("8Gi");
      expect(normalizeSettingValue(key, "8\tGi")).toBe("8Gi");
    });

    it("is idempotent on canonical input", () => {
      for (const v of ["512Mi", "1Gi", "8Gi", "16Gi", "1024Ki"]) {
        expect(normalizeSettingValue(key, v)).toBe(v);
      }
    });

    it("passes ambiguous and broken inputs through unchanged", () => {
      // These all fall through to the pattern check, which rejects
      // them with aria-invalid + helpful error.
      const passthrough = [
        "banana",      // not a quantity
        "",            // empty
        "-1Gi",        // negative
        "8.5Gi",       // fractional
        "8gigabyte",   // word, not a unit token
        "8 G",         // bare G is ambiguous (decimal vs binary)
      ];
      for (const v of passthrough) {
        expect(normalizeSettingValue(key, v)).toBe(v);
      }
    });
  });

  describe("CPU", () => {
    const key = "workspace.defaultResources.cpu";

    it("lowercases millicore suffix", () => {
      expect(normalizeSettingValue(key, "500M")).toBe("500m");
      expect(normalizeSettingValue(key, "1000M")).toBe("1000m");
      expect(normalizeSettingValue(key, "500m")).toBe("500m");
    });

    it("trims whitespace", () => {
      expect(normalizeSettingValue(key, "  500m  ")).toBe("500m");
      expect(normalizeSettingValue(key, "500 m")).toBe("500m");
    });

    it("leaves cpu-fraction form unchanged", () => {
      // "1.0", "0.5" — the other valid CPU shape; nothing to fix.
      expect(normalizeSettingValue(key, "1.0")).toBe("1.0");
      expect(normalizeSettingValue(key, "0.5")).toBe("0.5");
    });
  });

  describe("storage", () => {
    const key = "workspace.defaultStorageSize";

    it("auto-corrects lowercase and GB → Gi", () => {
      expect(normalizeSettingValue(key, "15gi")).toBe("15Gi");
      expect(normalizeSettingValue(key, "15GB")).toBe("15Gi");
      expect(normalizeSettingValue(key, " 15Gi ")).toBe("15Gi");
    });
  });

  describe("default image", () => {
    const key = "workspace.defaultImage";

    it("trims surrounding whitespace", () => {
      expect(normalizeSettingValue(key, "  ghcr.io/x/y:1.0.0 ")).toBe(
        "ghcr.io/x/y:1.0.0",
      );
    });
  });

  describe("non-resource settings", () => {
    it("does not touch instance.name", () => {
      // Free-form name field — auto-trimming would be surprising.
      expect(normalizeSettingValue("instance.name", "  Mixed   Spaces  ")).toBe(
        "  Mixed   Spaces  ",
      );
    });

    it("does not touch arbitrary unknown keys", () => {
      expect(normalizeSettingValue("some.random.key", "8gi")).toBe("8gi");
    });
  });
});
