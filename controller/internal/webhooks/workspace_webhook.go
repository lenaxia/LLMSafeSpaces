// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package webhooks

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"reflect"
	"regexp"
	"strconv"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
)

// +kubebuilder:webhook:path=/validate-llmsafespaces-dev-v1-workspace,mutating=false,failurePolicy=fail,groups=llmsafespaces.dev,resources=workspaces,verbs=create;update,versions=v1,name=vworkspace.kb.io,sideEffects=None,admissionReviewVersions=v1

// allowRuntimeClassOverrideAnnotation is the admin-gating annotation for
// spec.runtimeClass (Epic 51 S51.1 design: opt-out must be admin-gated,
// not tenant-selectable). Operators apply this annotation via direct
// kubectl/cluster-admin RBAC; tenant API users (who don't have cluster
// RBAC) cannot.
//
// Closes the deferral noted on the CRD field comment at
// pkg/apis/llmsafespaces/v1/workspace_types.go:158-161:
// "webhook validation to prevent tenants from setting this field via
// direct kubectl is deferred to S51.2."
const allowRuntimeClassOverrideAnnotation = "llmsafespaces.dev/allow-runtime-class-override"

// adminAllowsRuntimeClassOverride reports whether the workspace's
// annotations grant admin blessing for a non-default spec.runtimeClass.
// The annotation value must be the literal "true" (case-sensitive) — any
// other value is treated as absent. This avoids the classic YAML-truthy
// footgun where "yes"/"on"/"1" would all be misread as true.
func adminAllowsRuntimeClassOverride(annotations map[string]string) bool {
	if annotations == nil {
		return false
	}
	return annotations[allowRuntimeClassOverrideAnnotation] == "true"
}

// WorkspaceValidator is a ValidatingAdmissionWebhook for Workspace resources.
//
// It closes the following pentest findings (Epic 17):
//   - F1.2.1 / RT-2.18 / RT-6.10 (Critical): Spec.Runtime arbitrary image
//     pull. Without an allow-list, a user could create a Workspace with
//     `runtime: "evil.example.com/malicious:latest"` and the controller
//     would pull and run that image.
//   - F1.2.2 (Critical): Status forge. On CREATE the kube-apiserver does
//     NOT yet apply status-subresource semantics, so a malicious user
//     can stamp `status.podIP` / `status.podName` / `status.endpoint`
//     and the API proxy will route requests to the attacker-supplied
//     pod-IP. Defense in depth: also reject status mutations on UPDATE
//     through the spec endpoint (the kube-apiserver subresource split
//     normally enforces this, but failure-modes during CRD upgrades
//     have surfaced it as a real risk).
//   - F1.2.9 (Medium): Spec.Storage.StorageClassName had no allow-list,
//     letting users target hostPath / NFS / arbitrary CSIs.
//   - RT-6.1 (High): Webhook accepted `runtime: "../../etc/passwd"` and
//     `storage.size: "999999Gi"` (CRD pattern allowed any digit count).
//
// The validator is configurable so the same chart works for every
// deployment topology — operators decide which registries and storage
// classes are safe in their environment.
//
// Field reference (set by the controller manager at construction):
//   - Decoder: required; nil decoder makes Handle deny with a clear
//     error rather than panic on nil-pointer-deref.
//   - AllowedImageRegistries: list of registry prefixes (e.g.
//     "ghcr.io/lenaxia/", "registry.k8s.io/"). A Workspace whose Runtime
//     contains "/" (i.e. is shaped like an explicit image reference)
//     must match at least one prefix.
//   - AllowedStorageClassNames: optional. If non-nil, the Spec.Storage.
//     StorageClassName must be in this list (empty StorageClassName
//     always passes — that means "use cluster default").
//   - MaxStorageGi: maximum requested workspace storage in GiB. Any
//     storage size above this is rejected. Set 0 to disable.
//   - MaxCPUMillicores / MaxMemoryMi: maximum
//     spec.resources.{cpu,memory} accepted at
//     admission. Closes F1.2.3 secondary surface (a user can declare
//     999999999m CPU; the CRD pattern allows it; the pod stays Pending
//     forever, wasting tenant quota — DoS at the API/etcd layer).
//     Set 0 to disable each cap individually.
type WorkspaceValidator struct {
	Decoder                  admission.Decoder
	AllowedImageRegistries   []string
	AllowedStorageClassNames []string
	MaxStorageGi             int64
	MaxCPUMillicores         int64
	MaxMemoryMi              int64
}

// runtimeRefIsImage reports whether the runtime string looks like an
// explicit container image reference. The convention used by
// `runtime_resolver.go` is: presence of '/' triggers image-pull rather
// than RuntimeEnvironment lookup.
func runtimeRefIsImage(s string) bool {
	return strings.Contains(s, "/")
}

// runtimeRunSafePattern accepts:
//   - DNS-style names with optional ':tag' or '@digest' suffix.
//   - Lowercase letters, digits, dots, slashes, dashes, underscores,
//     colon, and '@'.
//
// Anything else is a NAK.
var runtimeRunSafePattern = regexp.MustCompile(`^[a-zA-Z0-9._/:@-]+$`)

// runtimeRefHasTraversal flags path-traversal / NUL / backslash payloads.
// These never appear in legitimate image references.
func runtimeRefHasTraversal(s string) bool {
	if strings.Contains(s, "..") {
		return true
	}
	if strings.ContainsAny(s, "\x00\\ \t\n\r") {
		return true
	}
	return false
}

// storageSizePattern is a stricter form of the CRD pattern. The CRD
// allows any number of digits; we additionally enforce magnitude > 0
// (matching settings.StorageQuantityPattern) and an upper bound in
// storageSizeGi. The accept-set must equal settings.StorageQuantityPattern;
// drift is caught by TestWebhookRegexAcceptsSameInputsAsSettingsPattern.
var storageSizePattern = regexp.MustCompile(`^([1-9][0-9]*)(Gi|Mi)$`)

// cpuPattern matches the CRD's spec.resources.cpu pattern. Accept-set
// equals settings.CPUQuantityPattern: positive millicores or positive
// fractional cores. Three alternations: [1-9][0-9]*m,
// [1-9][0-9]*\.[0-9]+, and 0\.[0-9]*[1-9][0-9]*. The parser-side
// capture groups are an implementation detail; the inputs accepted
// are identical to the canonical pattern. Drift caught by
// TestWebhookRegexAcceptsSameInputsAsSettingsPattern.
var cpuPattern = regexp.MustCompile(`^([1-9][0-9]*)m$|^([1-9][0-9]*\.[0-9]+|0\.[0-9]*[1-9][0-9]*)$`)

// memoryPattern matches the CRD's spec.resources.memory pattern
// (Ki|Mi|Gi) with magnitude > 0. Accept-set equals
// settings.MemoryQuantityPattern.
var memoryPattern = regexp.MustCompile(`^([1-9][0-9]*)(Ki|Mi|Gi)$`)

// parseCPUMillis converts a CPU string ("500m" or "1.5") to integer
// millicores. Returns (-1, err) on malformed input.
func parseCPUMillis(s string) (int64, error) {
	m := cpuPattern.FindStringSubmatch(s)
	if m == nil {
		return -1, fmt.Errorf("cpu %q does not match ^([1-9][0-9]*m|[1-9][0-9]*\\.[0-9]+|0\\.[0-9]*[1-9][0-9]*)$ (positive only)", s)
	}
	if m[1] != "" {
		n, err := strconv.ParseInt(m[1], 10, 64)
		if err != nil {
			return -1, fmt.Errorf("cpu %q has invalid magnitude", s)
		}
		return n, nil
	}
	// Decimal form: parse "1.5" → 1500 millis.
	whole, frac, _ := strings.Cut(m[2], ".")
	w, err := strconv.ParseInt(whole, 10, 64)
	if err != nil {
		return -1, fmt.Errorf("cpu %q has invalid whole part", s)
	}
	if len(frac) > 3 {
		frac = frac[:3]
	}
	for len(frac) < 3 {
		frac += "0"
	}
	f, err := strconv.ParseInt(frac, 10, 64)
	if err != nil {
		return -1, fmt.Errorf("cpu %q has invalid fractional part", s)
	}
	return w*1000 + f, nil
}

// parseMemoryMi converts a memory string (Ki/Mi/Gi suffix) to integer
// MiB, rounding sub-MiB allocations up to 1.
func parseMemoryMi(s string) (int64, error) {
	m := memoryPattern.FindStringSubmatch(s)
	if m == nil {
		return -1, fmt.Errorf("memory %q does not match ^[1-9][0-9]*(Ki|Mi|Gi)$", s)
	}
	n, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil || n < 1 {
		return -1, fmt.Errorf("memory %q has invalid magnitude", s)
	}
	switch m[2] {
	case "Ki":
		mi := (n + 1023) / 1024
		if mi < 1 {
			mi = 1
		}
		return mi, nil
	case "Mi":
		return n, nil
	case "Gi":
		return n * 1024, nil
	}
	return -1, fmt.Errorf("memory %q has unrecognized suffix", s)
}

// storageSizeGi parses the spec.storage.size string and returns the
// value in GiB, rounding Mi up to 1Gi (so 256Mi reports as 1Gi). Returns
// (-1, error) on malformed input.
func storageSizeGi(s string) (int64, error) {
	m := storageSizePattern.FindStringSubmatch(s)
	if m == nil {
		return -1, fmt.Errorf("storage.size %q does not match ^[1-9][0-9]*(Gi|Mi)$", s)
	}
	n, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil || n < 1 {
		return -1, fmt.Errorf("storage.size %q has invalid magnitude", s)
	}
	if m[2] == "Mi" {
		// Round up: any Mi value occupies at most 1Gi from the quota's
		// perspective. Avoids spurious "0Gi" results for small allocations.
		gi := (n + 1023) / 1024
		if gi < 1 {
			gi = 1
		}
		return gi, nil
	}
	return n, nil
}

// statusIsZero reports whether the Status block contains only zero
// values. A user creating a Workspace must not set any Status field;
// only the controller writes Status (via the status subresource).
func statusIsZero(s v1.WorkspaceStatus) bool {
	return reflect.DeepEqual(s, v1.WorkspaceStatus{})
}

// packageRequirementSafePattern is a CONSERVATIVE positive allow-list
// for package-manager identifiers. It accepts ONLY:
//
//   - a leading alphabetic char (rejects leading `-` argv-injection
//     attempts like `--index-url=...`, `-r/etc/passwd`, `-toolexec=...`),
//     OR `@` for npm-scoped packages.
//   - then alphanumerics, dot, dash, underscore.
//   - optionally pip extras `[name1,name2]`.
//   - optionally version constraints
//     (`==`/`>=`/`<=`/`>`/`<`/`~=`/`!=` followed by a version expr,
//     comma-separated for compound constraints).
//   - optionally `@` followed by a version label (npm `@scope/pkg@1.0`,
//     pip git ref `pkg@v1.0`).
//
// Notably FORBIDS:
//   - leading dash (argv injection: `--index-url=`, `-r`, `-toolexec=`).
//   - URL shapes (`git+https://`, `https://`, `file://`, `./path`, `/abs`)
//     — these would let pip / npm / go install pull and execute
//     attacker-controlled content (RCE).
//   - whitespace and shell metacharacters (defended elsewhere too).
//   - `..` path-traversal sequences.
//
// PEP 508 features deliberately NOT supported (operator must extend
// the regex if they need them, accepting the trade-off):
//   - environment markers (`pkg; python_version<3.10`).
//   - URL specifiers (`pkg @ https://...`).
//   - hash pinning (`pkg --hash=sha256:...`) — flags are blocked
//     entirely.
//
// The trade-off is documented in `design/stories/epic-17-security-review/
// remediation/MASTER-TRACKER.md` so a future PR can widen this with
// eyes open.
var packageRequirementSafePattern = regexp.MustCompile(
	`^@?[a-zA-Z][a-zA-Z0-9._/-]*` + // name (optional npm scope, no leading dash, slashes for npm-scope/go-modules)
		`(\[[a-zA-Z0-9._,-]+\])?` + // optional pip extras
		`(@[a-zA-Z0-9._+/-]+)?` + // optional `@version` for npm or pip-git ref
		`(([<>=!~]=?)[a-zA-Z0-9._+-]+(,([<>=!~]=?)[a-zA-Z0-9._+-]+)*)?` + // optional version constraints, comma-separated
		`$`)

// urlSchemePattern detects URL-shaped requirements that would let pip,
// npm, or go install pull from an attacker-supplied location. Even if
// the chars are otherwise safe, a payload like
// `git+https://attacker.com/repo.git` is RCE via attacker `setup.py`.
var urlSchemePattern = regexp.MustCompile(
	`(?i)^(git\+|https?:|ssh:|ftp:|file:|svn\+|hg\+|bzr\+|\.\.?/|/)`)

// validatePackageRequirement enforces a strict allow-list on each
// Spec.Packages[].Requirements[] entry. Closes F1.2.5 (Epic 17).
func validatePackageRequirement(req string) error {
	req = strings.TrimSpace(req)
	if req == "" {
		return fmt.Errorf("requirement must not be empty")
	}
	if len(req) > 256 {
		return fmt.Errorf("requirement exceeds the 256-character length limit")
	}
	if strings.HasPrefix(req, "-") {
		return fmt.Errorf(
			"requirement starts with '-' which would be interpreted as a flag " +
				"by pip / npm / go install (argv injection)")
	}
	if urlSchemePattern.MatchString(req) {
		return fmt.Errorf(
			"requirement looks like a URL or path; URL/path installs are blocked " +
				"to prevent RCE via attacker-controlled package sources")
	}
	if strings.Contains(req, "..") {
		return fmt.Errorf("requirement contains forbidden '..' (path traversal)")
	}
	if !packageRequirementSafePattern.MatchString(req) {
		return fmt.Errorf(
			"requirement contains characters outside the conservative allow-list; " +
				"only package-name + version-constraint syntax is permitted")
	}
	return nil
}

// internalDomainSuffixes is the cluster-internal suffix block-list
// for spec.networkAccess.egress[].domain. Closes the F1.2.4 bypass
// where a user could declare `kubernetes.default.svc.cluster.local`
// and the controller would resolve it to the apiserver ClusterIP and
// emit a /32 NetPol allow that defeats the chart's `blockedEgressCIDRs`
// via NetworkPolicy union semantics.
var internalDomainSuffixes = []string{
	".cluster.local",
	".svc",
	".svc.cluster.local",
	".local",
	".internal",
}

// domainSafePattern accepts ASCII LDH (letters/digits/hyphen) labels
// separated by dots, with an optional leading wildcard `*.`. Length
// caps per RFC: 253 total, 63 per label.
var domainSafePattern = regexp.MustCompile(
	`^(\*\.)?([a-zA-Z0-9]([a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?\.)+[a-zA-Z]{2,63}$`)

// validateEgressDomain enforces the user-declared FQDN allow-list to
// (a) be syntactically a hostname, (b) not target cluster-internal
// suffixes, (c) not be an IP literal (which would bypass DNS-side
// guarantees), (d) not exceed 253 chars total. Closes F1.2.4 bypass
// class found by validator pass 2.
func validateEgressDomain(d string) error {
	d = strings.TrimSpace(d)
	if d == "" {
		return fmt.Errorf("domain must not be empty")
	}
	if len(d) > 253 {
		return fmt.Errorf("domain exceeds the 253-character RFC limit")
	}
	if ip := net.ParseIP(d); ip != nil {
		_ = ip
		return fmt.Errorf(
			"domain must be a hostname, not a literal IP address " +
				"(IP literals bypass the cluster-internal suffix filter)")
	}
	low := strings.ToLower(strings.TrimSuffix(d, "."))
	for _, suf := range internalDomainSuffixes {
		if strings.HasSuffix(low, suf) {
			return fmt.Errorf(
				"domain ends in cluster-internal suffix %q; in-cluster destinations "+
					"must be reached via a public FQDN behind ingress, not via "+
					"spec.networkAccess.egress (would defeat chart-wide blocked CIDRs)",
				suf)
		}
	}
	if !domainSafePattern.MatchString(d) {
		return fmt.Errorf(
			"domain %q is not a valid hostname (label LDH grammar; "+
				"max 63 chars per label; must end with a TLD letter)", d)
	}
	return nil
}

// Handle validates the Workspace resource. Errors are returned as
// admission.Denied with a human-readable message rather than as 5xx
// admission errors so kubectl shows the operator the precise reason.
func (v *WorkspaceValidator) Handle(ctx context.Context, req admission.Request) admission.Response {
	if v.Decoder == nil {
		return admission.Errored(http.StatusInternalServerError,
			fmt.Errorf("workspace webhook: decoder is not configured"))
	}

	ws := &v1.Workspace{}
	if err := v.Decoder.Decode(req, ws); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}

	// 1. Runtime is required.
	if strings.TrimSpace(ws.Spec.Runtime) == "" {
		return admission.Denied("spec.runtime is required")
	}

	// 1a. Length cap — RE2 is linear so no ReDoS, but unbounded strings
	//     are still wasteful and unrealistic. Container image references
	//     never exceed 255 chars (registry+repo+tag/digest); a 512-char
	//     ceiling is generous.
	if len(ws.Spec.Runtime) > 512 {
		return admission.Denied(
			"spec.runtime exceeds the 512-character length limit")
	}
	// Same for storage class name. Kubernetes object names cap at 253.
	if len(ws.Spec.Storage.StorageClassName) > 253 {
		return admission.Denied(
			"spec.storage.storageClassName exceeds the 253-character limit")
	}

	// 2. Runtime must not contain path-traversal / NUL / whitespace.
	if runtimeRefHasTraversal(ws.Spec.Runtime) {
		return admission.Denied(
			"spec.runtime contains forbidden characters (path-traversal, whitespace, NUL or backslash)")
	}
	if !runtimeRunSafePattern.MatchString(ws.Spec.Runtime) {
		return admission.Denied(
			"spec.runtime contains characters outside the allowed set [a-zA-Z0-9._/:@-]")
	}

	// 3. If runtime references an explicit image (contains '/'), it MUST
	//    match a configured registry allow-list prefix. Reject otherwise.
	//    A reference without '/' (e.g. "python-3.11") is a
	//    RuntimeEnvironment name lookup; the controller validates the
	//    target exists at reconcile time.
	//
	//    Allow-list prefix safety: each prefix MUST end with '/' so
	//    `HasPrefix("ghcr.io/lenaxia.attacker.com/...", "ghcr.io/lenaxia/")`
	//    cannot accidentally match. We enforce that here rather than
	//    trust the operator to remember the trailing slash. Prefixes
	//    without a trailing '/' are silently treated as `prefix + "/"`.
	if runtimeRefIsImage(ws.Spec.Runtime) {
		matched := false
		for _, prefix := range v.AllowedImageRegistries {
			if prefix == "" {
				continue
			}
			normalized := prefix
			if !strings.HasSuffix(normalized, "/") {
				normalized += "/"
			}
			if strings.HasPrefix(ws.Spec.Runtime, normalized) {
				matched = true
				break
			}
		}
		if !matched {
			allowed := strings.Join(v.AllowedImageRegistries, ", ")
			if allowed == "" {
				allowed = "(none — operator must populate webhooks.allowedImageRegistries)"
			}
			return admission.Denied(fmt.Sprintf(
				"spec.runtime %q is an explicit image reference but its registry is not in the allow-list. Allowed registry prefixes: %s",
				ws.Spec.Runtime, allowed))
		}
	}

	// 4. Storage size: enforce the CRD pattern AND an upper bound.
	if strings.TrimSpace(ws.Spec.Storage.Size) == "" {
		return admission.Denied("spec.storage.size is required")
	}
	gi, err := storageSizeGi(ws.Spec.Storage.Size)
	if err != nil {
		return admission.Denied(err.Error())
	}
	if v.MaxStorageGi > 0 && gi > v.MaxStorageGi {
		return admission.Denied(fmt.Sprintf(
			"spec.storage.size %s (%d Gi) exceeds the maximum %d Gi configured for this cluster",
			ws.Spec.Storage.Size, gi, v.MaxStorageGi))
	}

	// 4a. F1.2.3 — Spec.Resources.{cpu,memory} caps.
	//     The CRD's pattern validation lets through values like
	//     "999999999m" CPU; admission can pin CPU+memory but pods
	//     stay Pending forever, wasting tenant quota and confusing
	//     operators. Reject before the workspace ever reaches the
	//     scheduler.
	if r := ws.Spec.Resources; r != nil {
		if v.MaxCPUMillicores > 0 && r.CPU != "" {
			millis, err := parseCPUMillis(r.CPU)
			if err != nil {
				return admission.Denied(fmt.Sprintf(
					"spec.resources.cpu %q: %s", r.CPU, err.Error()))
			}
			if millis > v.MaxCPUMillicores {
				return admission.Denied(fmt.Sprintf(
					"spec.resources.cpu %q (%d millicores) exceeds the maximum %d millicores",
					r.CPU, millis, v.MaxCPUMillicores))
			}
		}
		if v.MaxMemoryMi > 0 && r.Memory != "" {
			mi, err := parseMemoryMi(r.Memory)
			if err != nil {
				return admission.Denied(fmt.Sprintf(
					"spec.resources.memory %q: %s", r.Memory, err.Error()))
			}
			if mi > v.MaxMemoryMi {
				return admission.Denied(fmt.Sprintf(
					"spec.resources.memory %q (%d Mi) exceeds the maximum %d Mi",
					r.Memory, mi, v.MaxMemoryMi))
			}
		}
		// 4b. Limit field caps (US-24.3): MaxCPUMillicores/MaxMemoryMi apply to limit fields.
		if v.MaxCPUMillicores > 0 && r.CPULimit != "" {
			millis, err := parseCPUMillis(r.CPULimit)
			if err != nil {
				return admission.Denied(fmt.Sprintf(
					"spec.resources.cpuLimit %q: %s", r.CPULimit, err.Error()))
			}
			if millis > v.MaxCPUMillicores {
				return admission.Denied(fmt.Sprintf(
					"spec.resources.cpuLimit %q (%d millicores) exceeds the maximum %d millicores",
					r.CPULimit, millis, v.MaxCPUMillicores))
			}
		}
		if v.MaxMemoryMi > 0 && r.MemoryLimit != "" {
			mi, err := parseMemoryMi(r.MemoryLimit)
			if err != nil {
				return admission.Denied(fmt.Sprintf(
					"spec.resources.memoryLimit %q: %s", r.MemoryLimit, err.Error()))
			}
			if mi > v.MaxMemoryMi {
				return admission.Denied(fmt.Sprintf(
					"spec.resources.memoryLimit %q (%d Mi) exceeds the maximum %d Mi",
					r.MemoryLimit, mi, v.MaxMemoryMi))
			}
		}
		// 4c. Limit >= request validation (US-24.3).
		if r.CPULimit != "" && r.CPU != "" {
			reqMillis, reqErr := parseCPUMillis(r.CPU)
			limMillis, limErr := parseCPUMillis(r.CPULimit)
			if reqErr == nil && limErr == nil && limMillis < reqMillis {
				return admission.Denied(fmt.Sprintf(
					"spec.resources.cpuLimit %q (%d millicores) must be >= cpu request %q (%d millicores)",
					r.CPULimit, limMillis, r.CPU, reqMillis))
			}
		}
		if r.MemoryLimit != "" && r.Memory != "" {
			reqMi, reqErr := parseMemoryMi(r.Memory)
			limMi, limErr := parseMemoryMi(r.MemoryLimit)
			if reqErr == nil && limErr == nil && limMi < reqMi {
				return admission.Denied(fmt.Sprintf(
					"spec.resources.memoryLimit %q (%d Mi) must be >= memory request %q (%d Mi)",
					r.MemoryLimit, limMi, r.Memory, reqMi))
			}
		}
	}

	// 5. StorageClassName allow-list (optional). Empty = use cluster
	//    default and is always permitted.
	if v.AllowedStorageClassNames != nil && ws.Spec.Storage.StorageClassName != "" {
		matched := false
		for _, sc := range v.AllowedStorageClassNames {
			if sc == ws.Spec.Storage.StorageClassName {
				matched = true
				break
			}
		}
		if !matched {
			return admission.Denied(fmt.Sprintf(
				"spec.storage.storageClassName %q is not in the allow-list %v",
				ws.Spec.Storage.StorageClassName, v.AllowedStorageClassNames))
		}
	}

	// 5a. F1.2.5 — Spec.Packages[].Requirements[] shell-injection guard.
	//     Pre-fix the controller built `pip install --target=... <req>`
	//     by string concatenation; an adversarial requirement like
	//     "pkg; rm -rf /" got code-execution in the init container.
	//     Defense in depth: the controller now shell-quotes every
	//     requirement (see buildWorkspaceSetupScript). Belt-and-braces:
	//     reject syntactically-impossible requirements at admission so
	//     even a misconfigured cluster (failurePolicy=Ignore) gets
	//     primary control.
	for pkgIdx, pkgSet := range ws.Spec.Packages {
		for reqIdx, req := range pkgSet.Requirements {
			if err := validatePackageRequirement(req); err != nil {
				return admission.Denied(fmt.Sprintf(
					"spec.packages[%d].requirements[%d] %q: %s",
					pkgIdx, reqIdx, req, err.Error()))
			}
		}
	}

	// 5b. F1.2.4 — Spec.NetworkAccess.Egress[].Domain validation.
	//     Pre-fix the controller would resolve any user-declared
	//     domain (including `kubernetes.default.svc.cluster.local`)
	//     and emit a /32 NetPol allow that defeats the chart's
	//     `blockedEgressCIDRs` exclusion via NetPol union semantics.
	//     Reject cluster-internal suffixes, IP literals, and
	//     malformed hostnames at admission. The controller-side
	//     filter (network_policy.go) is defense-in-depth.
	if ws.Spec.NetworkAccess != nil {
		for i, rule := range ws.Spec.NetworkAccess.Egress {
			if err := validateEgressDomain(rule.Domain); err != nil {
				return admission.Denied(fmt.Sprintf(
					"spec.networkAccess.egress[%d].domain %q: %s",
					i, rule.Domain, err.Error()))
			}
		}
	}

	// 7. S51.1 admin gate — spec.runtimeClass is admin-gated, not
	//    tenant-selectable. Without this check, any user with workspace
	//    create/update RBAC can set spec.runtimeClass="runc" to escape
	//    gVisor via direct kubectl, defeating the kernel-level isolation
	//    layer for their own pods. The CRD comment at
	//    pkg/apis/llmsafespaces/v1/workspace_types.go:158-161 documents
	//    this as a deferred S51.2 follow-up; this PR closes that gap.
	//
	//    Scheme: reject spec.runtimeClass unless the object carries the
	//    admin annotation llmsafespaces.dev/allow-runtime-class-override=true.
	//    Operators with cluster-admin RBAC apply the annotation; tenant
	//    RBAC scopes cannot.
	if ws.Spec.RuntimeClass != nil && strings.TrimSpace(*ws.Spec.RuntimeClass) != "" {
		if !adminAllowsRuntimeClassOverride(ws.Annotations) {
			return admission.Denied(fmt.Sprintf(
				"spec.runtimeClass %q is admin-gated; apply annotation %q=\"true\" "+
					"via cluster-admin RBAC to opt this workspace out of the default runtime class",
				*ws.Spec.RuntimeClass, allowRuntimeClassOverrideAnnotation))
		}
	}

	// 8. F1.2.2 — Status must not be set by the user. On CREATE only the
	//    controller (via status subresource) is allowed to populate the
	//    block.
	if req.Operation == "CREATE" && !statusIsZero(ws.Status) {
		return admission.Denied(
			"spec.status fields must not be set on CREATE; the controller writes status via the status subresource")
	}

	// 9. F1.2.2 (defense in depth) — On UPDATE, refuse to mutate Status
	//    via the spec endpoint. The kube-apiserver normally enforces
	//    this via the subresource split (writes to /workspaces ignore
	//    Status), but a CRD-upgrade race or a misconfigured aggregator
	//    could lift that enforcement; we re-check at admission.
	//
	//    Validator-bypass note (worklog 0096): an empty
	//    `req.OldObject.Raw` was previously silently allowing through
	//    UPDATEs because the comparison had nothing to compare against.
	//    We now treat that case as "old status was zero" — i.e. any
	//    non-zero status on the new object is rejected. AdmissionReview
	//    v1 does not strictly require OldObject on UPDATE, so we fail
	//    closed when it is missing.
	if req.Operation == "UPDATE" {
		var oldStatus v1.WorkspaceStatus
		if len(req.OldObject.Raw) > 0 {
			old := &v1.Workspace{}
			if err := v.Decoder.DecodeRaw(req.OldObject, old); err != nil {
				return admission.Errored(http.StatusBadRequest,
					fmt.Errorf("decoding old workspace for status comparison: %w", err))
			}
			oldStatus = old.Status
		}
		if !reflect.DeepEqual(ws.Status, oldStatus) {
			return admission.Denied(
				"spec.status mutations through the workspaces endpoint are not allowed; use the workspaces/status subresource")
		}
	}

	return admission.Allowed("workspace is valid")
}

// InjectDecoder retained for backwards compatibility (see WorkspaceValidator).
func (v *WorkspaceValidator) InjectDecoder(d admission.Decoder) error {
	v.Decoder = d
	return nil
}
