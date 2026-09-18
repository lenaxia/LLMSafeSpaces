{{/*
Expand the name of the chart.
*/}}
{{- define "llmsafespaces.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
*/}}
{{- define "llmsafespaces.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{/*
Common labels.
*/}}
{{- define "llmsafespaces.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{ include "llmsafespaces.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels.
*/}}
{{- define "llmsafespaces.selectorLabels" -}}
app.kubernetes.io/name: {{ include "llmsafespaces.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Component-specific labels.
*/}}
{{- define "llmsafespaces.api.labels" -}}
{{ include "llmsafespaces.labels" . }}
app.kubernetes.io/component: api
{{- end }}

{{- define "llmsafespaces.api.selectorLabels" -}}
{{ include "llmsafespaces.selectorLabels" . }}
app.kubernetes.io/component: api
{{- end }}

{{- define "llmsafespaces.controller.labels" -}}
{{ include "llmsafespaces.labels" . }}
app.kubernetes.io/component: controller
{{- end }}

{{- define "llmsafespaces.controller.selectorLabels" -}}
{{ include "llmsafespaces.selectorLabels" . }}
app.kubernetes.io/component: controller
{{- end }}

{{/*
Service account names.
*/}}
{{- define "llmsafespaces.api.serviceAccountName" -}}
{{- if .Values.serviceAccount.api.create }}
{{- default (printf "%s-api" (include "llmsafespaces.fullname" .)) .Values.serviceAccount.api.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.api.name }}
{{- end }}
{{- end }}

{{- define "llmsafespaces.controller.serviceAccountName" -}}
{{- if .Values.serviceAccount.controller.create }}
{{- default (printf "%s-controller" (include "llmsafespaces.fullname" .)) .Values.serviceAccount.controller.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.controller.name }}
{{- end }}
{{- end }}

{{/*
Resolve the name of the credentials secret.
*/}}
{{- define "llmsafespaces.secretName" -}}
{{- if .Values.externalSecret.existingSecret }}
{{- .Values.externalSecret.existingSecret }}
{{- else }}
{{- printf "%s-credentials" (include "llmsafespaces.fullname" .) }}
{{- end }}
{{- end }}

{{/*
Resolve the namespace where sandbox/workspace CRDs are created. Falls back to
the release namespace if not explicitly set.
*/}}
{{- define "llmsafespaces.workspaceNamespace" -}}
{{- default .Release.Namespace .Values.api.config.kubernetes.namespace }}
{{- end }}

{{/*
Resolve image references — defaults the tag to .Chart.AppVersion if omitted.
When .Values.<svc>.image.digest is set, the image is pinned to
repo@digest (immutable content-addressable ref, ignoring tag). Operators
use this to avoid GHCR tag GC issues (#454, #476).
*/}}
{{- define "llmsafespaces.api.image" -}}
{{- if .Values.api.image.digest -}}
{{- printf "%s@%s" .Values.api.image.repository .Values.api.image.digest -}}
{{- else -}}
{{- $tag := default .Chart.AppVersion .Values.api.image.tag -}}
{{- printf "%s:%s" .Values.api.image.repository $tag -}}
{{- end -}}
{{- end }}

{{/*
Pod selector labels for workspace (sandbox) pods. The controller's
pod-builder applies these labels at controller/internal/workspace/
controller.go:566 so this helper MUST stay in sync.

Used by the workspace NetworkPolicy templates (Epic 17 G16) to scope
default-deny ingress and egress allow-list rules.
*/}}
{{- define "llmsafespaces.workspacePodSelectorLabels" -}}
app: llmsafespaces
component: workspace
{{- end }}

{{- define "llmsafespaces.controller.image" -}}
{{- if .Values.controller.image.digest -}}
{{- printf "%s@%s" .Values.controller.image.repository .Values.controller.image.digest -}}
{{- else -}}
{{- $tag := default .Chart.AppVersion .Values.controller.image.tag -}}
{{- printf "%s:%s" .Values.controller.image.repository $tag -}}
{{- end -}}
{{- end }}

{{- define "llmsafespaces.relayRouter.labels" -}}
{{ include "llmsafespaces.labels" . }}
app.kubernetes.io/component: relay-router
{{- end }}

{{- define "llmsafespaces.relayRouter.selectorLabels" -}}
{{ include "llmsafespaces.selectorLabels" . }}
app.kubernetes.io/component: relay-router
{{- end }}

{{- define "llmsafespaces.llmRelayRouter.selectorLabels" -}}
app.kubernetes.io/name: llm-relay-router
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{- define "llmsafespaces.llmRelayRouter.labels" -}}
{{ include "llmsafespaces.llmRelayRouter.selectorLabels" . }}
app.kubernetes.io/component: relay-router
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
llmsafespaces.positiveIntEnv coerces a numeric value (values-file int,
values-file float, --set string in decimal/exponential notation) to an
exact decimal string for env consumption (strconv.Atoi on the router side).
Fails the render for negative or non-numeric results (a silent 0 would disable the bound): the byte quota and body caps are
security-adjacent §4.7 bounds, and a silent "0" would disable them
(quota 0 = unlimited) just as silently as the scientific-notation class
this replaces.
*/}}
{{- define "llmsafespaces.positiveIntEnv" -}}
{{- $n := (. | float64 | int64) -}}
{{- if lt $n 1 -}}
{{- fail (printf "value %v must coerce to an integer >= 1 (got %d)" . $n) -}}
{{- end -}}
{{- $n | quote -}}
{{- end }}

{{- define "llmsafespaces.relayRouter.image" -}}
{{- if .Values.controller.inferenceRelay.router.image.digest -}}
{{- printf "%s@%s" .Values.controller.inferenceRelay.router.image.repository .Values.controller.inferenceRelay.router.image.digest -}}
{{- else -}}
{{- $tag := default .Chart.AppVersion .Values.controller.inferenceRelay.router.image.tag -}}
{{- printf "%s:%s" .Values.controller.inferenceRelay.router.image.repository $tag -}}
{{- end -}}
{{- end }}

{{/*
Resolve the frontend image. Same digest-pinning semantics as the other
image helpers (#476): when .Values.frontend.image.digest is set, produces
repo@digest (ignoring tag); otherwise repo:tag with AppVersion fallback.
*/}}
{{- define "llmsafespaces.frontend.image" -}}
{{- if .Values.frontend.image.digest -}}
{{- printf "%s@%s" .Values.frontend.image.repository .Values.frontend.image.digest -}}
{{- else -}}
{{- $tag := default .Chart.AppVersion .Values.frontend.image.tag -}}
{{- printf "%s:%s" .Values.frontend.image.repository $tag -}}
{{- end -}}
{{- end }}

{{/*
#821: report whether an IPv4 or IPv6 CIDR string falls inside the
private / internal ranges covered by the blockedEgressCIDRs defaults
(IPv4: RFC1918, CGNAT, link-local/metadata, loopback, multicast;
IPv6: ULA fc00::/7, link-local fe80::/10, multicast ff00::/8, loopback
::1). Lexical first/second-octet (v4) / prefix (v6, lowercased) check —
anything that does not parse as a dotted quad or IPv6 literal reports
false and is left to the API server's own ipBlock validation at apply
time.

Used as a render-time footgun guard: allowlist group entries and narrow
allowedEgressCIDRs entries render WITHOUT the blockedEgressCIDRs
subtraction (Kubernetes rejects ipBlock except: entries that are not
subnets of the cidr), so a private entry there would silently reopen
in-cluster or metadata ranges. Callers fail the render instead and
direct the operator to networkPolicy.extraEgressCIDRs, which is the
deliberate no-subtraction escape hatch for internal destinations.

Guard scope (documented honestly): catches well-formed private entries —
the realistic footgun class, IPv4 AND IPv6 (well-formed IPv6-internal
CIDRs are API-valid, so no apply-time backstop exists for them — the
round-3 review finding). Malformed spellings report false here but are
rejected loudly at apply time by the API server's ipBlock validation;
an aggregate CIDR spanning private space (e.g. 0.0.0.0/1, ::/0) is a
residual silent case (contrived input).

Pinned by TestEgress_Allowlist_PrivateGroupCIDRFailsRender,
TestEgress_PublicMode_PrivateNarrowAllowedCIDRFailsRender, and
TestEgress_Allowlist_PublicGroupCIDRsStillRender (over-match guard).
*/}}
{{- define "llmsafespaces.isPrivateInternalCIDR" -}}
{{- $parts := splitList "/" . -}}
{{- $octets := splitList "." (index $parts 0) -}}
{{- if eq (len $octets) 4 -}}
{{- $a := index $octets 0 | int -}}
{{- $b := index $octets 1 | int -}}
{{- if or (eq $a 10) (eq $a 127) -}}
true
{{- else if and (eq $a 172) (ge $b 16) (le $b 31) -}}
true
{{- else if and (eq $a 192) (eq $b 168) -}}
true
{{- else if and (eq $a 169) (eq $b 254) -}}
true
{{- else if and (eq $a 100) (ge $b 64) (le $b 127) -}}
true
{{- else if and (ge $a 224) (le $a 239) -}}
true
{{- else -}}
false
{{- end -}}
{{- else if contains ":" (index $parts 0) -}}
{{- $ip := lower (index $parts 0) -}}
{{- if or (hasPrefix "fc" $ip) (hasPrefix "fd" $ip) (hasPrefix "ff" $ip) (hasPrefix "fe8" $ip) (hasPrefix "fe9" $ip) (hasPrefix "fea" $ip) (hasPrefix "feb" $ip) (eq $ip "::1") -}}
true
{{- else -}}
false
{{- end -}}
{{- else -}}
false
{{- end -}}
{{- end -}}
