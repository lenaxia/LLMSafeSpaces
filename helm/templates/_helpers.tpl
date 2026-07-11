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
*/}}
{{- define "llmsafespaces.api.image" -}}
{{- $tag := default .Chart.AppVersion .Values.api.image.tag -}}
{{- printf "%s:%s" .Values.api.image.repository $tag -}}
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
{{- $tag := default .Chart.AppVersion .Values.controller.image.tag -}}
{{- printf "%s:%s" .Values.controller.image.repository $tag -}}
{{- end }}

{{- define "llmsafespaces.relayRouter.labels" -}}
{{ include "llmsafespaces.labels" . }}
app.kubernetes.io/component: relay-router
{{- end }}

{{- define "llmsafespaces.relayRouter.selectorLabels" -}}
{{ include "llmsafespaces.selectorLabels" . }}
app.kubernetes.io/component: relay-router
{{- end }}

{{- define "llmsafespaces.relayRouter.image" -}}
{{- $tag := default .Chart.AppVersion .Values.controller.inferenceRelay.router.image.tag -}}
{{- printf "%s:%s" .Values.controller.inferenceRelay.router.image.repository $tag -}}
{{- end }}
