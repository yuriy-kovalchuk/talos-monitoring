{{/*
Expand the name of the chart.
*/}}
{{- define "talos-monitoring.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Create a default fully qualified app name (truncated at 63 chars, which is the
Kubernetes limit for most resource names).
*/}}
{{- define "talos-monitoring.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $name := default .Chart.Name .Values.nameOverride -}}
{{- if contains $name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/*
Chart label value.
*/}}
{{- define "talos-monitoring.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
App version label value.
*/}}
{{- define "talos-monitoring.version" -}}
{{- .Chart.AppVersion | quote -}}
{{- end -}}

{{/*
Common labels.
*/}}
{{- define "talos-monitoring.labels" -}}
helm.sh/chart: {{ include "talos-monitoring.chart" . }}
{{ include "talos-monitoring.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ include "talos-monitoring.version" . }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{/*
Selector labels.
*/}}
{{- define "talos-monitoring.selectorLabels" -}}
app.kubernetes.io/name: {{ include "talos-monitoring.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{/*
Kubernetes ServiceAccount name.
*/}}
{{- define "talos-monitoring.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "talos-monitoring.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{/*
ClusterRole the binding points at.
*/}}
{{- define "talos-monitoring.clusterRoleName" -}}
{{- if .Values.rbac.create -}}
{{- printf "%s-reader" (include "talos-monitoring.fullname" .) -}}
{{- else -}}
{{- required "rbac.clusterRoleName is required when rbac.create is false" .Values.rbac.clusterRoleName -}}
{{- end -}}
{{- end -}}

{{/*
Container image reference.
*/}}
{{- define "talos-monitoring.image" -}}
{{- $tag := default .Chart.AppVersion .Values.image.tag -}}
{{- printf "%s%s:%s" .Values.image.registry .Values.image.repository $tag -}}
{{- end -}}
