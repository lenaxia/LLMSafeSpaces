/**
 * sanitiseReturnTo guards against open redirect: only same-app paths
 * starting with "/" are allowed. Protocol-relative, absolute URLs, and
 * paths with userinfo/host components are rejected.
 */
export function sanitiseReturnTo(raw: string): string {
  if (!raw || !raw.startsWith("/")) return "";
  // Reject protocol-relative and UNC-style paths.
  if (raw.startsWith("//") || raw.startsWith("\\\\")) return "";
  // Reject anything with a userinfo or host component.
  try {
    // Strip fragment before comparison — the URL parser drops it from
    // pathname, so comparing against the full raw would false-negative
    // on legitimate same-app paths like /chat#section.
    const pathOnly = raw.split("#")[0]!;
    const u = new URL(raw, "https://localhost");
    if (u.username || u.password || u.host !== "localhost") return "";
    if (u.pathname !== pathOnly.split("?")[0]) return "";
  } catch {
    return "";
  }
  return raw;
}
