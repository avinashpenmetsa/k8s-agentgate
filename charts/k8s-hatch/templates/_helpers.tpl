{{/*
Expand the name of the chart.
*/}}
{{- define "k8s-hatch.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
*/}}
{{- define "k8s-hatch.fullname" -}}
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
Common labels
*/}}
{{- define "k8s-hatch.labels" -}}
helm.sh/chart: {{ include "k8s-hatch.name" . }}-{{ .Chart.Version }}
app.kubernetes.io/name: {{ include "k8s-hatch.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels
*/}}
{{- define "k8s-hatch.selectorLabels" -}}
app.kubernetes.io/name: {{ include "k8s-hatch.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}
