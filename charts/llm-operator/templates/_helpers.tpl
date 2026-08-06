{{/* Expand the chart name. */}}
{{- define "llm-operator.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/* Create a release-scoped, DNS-safe name. */}}
{{- define "llm-operator.fullname" -}}
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

{{/* Chart label value. */}}
{{- define "llm-operator.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/* Common metadata labels. */}}
{{- define "llm-operator.labels" -}}
helm.sh/chart: {{ include "llm-operator.chart" . }}
{{ include "llm-operator.selectorLabels" . }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}

{{/* Stable selectors. */}}
{{- define "llm-operator.selectorLabels" -}}
app.kubernetes.io/name: {{ include "llm-operator.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/* ServiceAccount used by the manager. */}}
{{- define "llm-operator.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "llm-operator.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- required "serviceAccount.name is required when serviceAccount.create=false" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/* Digest-pinned image reference. */}}
{{- define "llm-operator.image" -}}
{{- $digest := required "image.digest is required; use a reviewed sha256 digest" .Values.image.digest -}}
{{- printf "%s@%s" .Values.image.repository $digest -}}
{{- end }}

{{/* Proxy object names deliberately default to Cogito-compatible names. */}}
{{- define "llm-operator.proxy.name" -}}
{{- required "proxy.service.name is required" .Values.proxy.service.name -}}
{{- end }}

{{- define "llm-operator.proxy.serviceAccountName" -}}
{{- required "proxy.serviceAccount.name is required" .Values.proxy.serviceAccount.name -}}
{{- end }}

{{- define "llm-operator.proxy.image" -}}
{{- $digest := required "proxy.image.digest is required when proxy.enabled=true" .Values.proxy.image.digest -}}
{{- printf "%s@%s" .Values.proxy.image.repository $digest -}}
{{- end }}

{{- define "llm-operator.proxy.selectorLabels" -}}
app.kubernetes.io/controller: proxy
app.kubernetes.io/instance: llm
app.kubernetes.io/name: llm
{{- end }}
