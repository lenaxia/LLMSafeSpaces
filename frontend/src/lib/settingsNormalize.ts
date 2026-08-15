// Setting value normalization for the admin/user settings UI.
//
// Mirrors pkg/settings/normalize.go on the backend so the user sees
// the canonical value land in the input on commit, and the wire
// payload matches what a curl client would send. Two-stage policy:
//
//   1. normalizeSettingValue() rewrites unambiguous near-misses to
//      canonical form ("8gi" → "8Gi", "8GB" → "8Gi", "  500m  " →
//      "500m").
//   2. The caller validates the normalized value against def.pattern.
//      Inputs the normalizer can't safely correct ("banana", "8 G",
//      "8.5Gi") pass through unchanged so the pattern check rejects
//      them with aria-invalid + helpful error.
//
// Resource-quantity settings are unit-normalized; the default image is
// trimmed. Other patterned strings (instance.name, MOTD) pass through
// verbatim — auto-trimming a name field would be surprising.

const memoryUnitMap: Record<string, string> = {
  ki: "Ki",
  mi: "Mi",
  gi: "Gi",
  kb: "Ki",
  mb: "Mi",
  gb: "Gi",
  // Bare K/M/G are deliberately NOT mapped. In Kubernetes Quantity
  // grammar they mean decimal multiples (10^3, 10^6, 10^9), distinct
  // from Ki/Mi/Gi (binary). A user typing "8G" might mean either —
  // pass through to the pattern rejection so they pick consciously.
};

const memoryNormalizeRe = /^([0-9]+)\s*([A-Za-z]+)$/;
const cpuNormalizeRe = /^([0-9]+)\s*[Mm]$/;

function normalizeMemory(value: string): string {
  const trimmed = value.trim();
  const m = memoryNormalizeRe.exec(trimmed);
  if (!m) return value;
  const digits = m[1];
  const unitRaw = m[2];
  if (!digits || !unitRaw) return value;
  const canonical = memoryUnitMap[unitRaw.toLowerCase()];
  if (!canonical) return value;
  return digits + canonical;
}

function normalizeCPU(value: string): string {
  const trimmed = value.trim();
  const m = cpuNormalizeRe.exec(trimmed);
  if (!m || !m[1]) return value;
  return m[1] + "m";
}

/** Canonicalize a settings input value before pattern validation.
 * Returns the input unchanged for any setting that doesn't have a
 * known canonical form, or for inputs the normalizer can't safely
 * correct. */
export function normalizeSettingValue(key: string, value: string): string {
  switch (key) {
    case "workspace.defaultResources.memory":
    case "workspace.defaultStorageSize":
      return normalizeMemory(value);
    case "workspace.defaultResources.cpu":
      return normalizeCPU(value);
    case "workspace.defaultImage":
      // Image refs tolerate no whitespace; trim before the server's
      // pinning validation rejects them. Mirrors normalize.go.
      return value.trim();
    default:
      return value;
  }
}
