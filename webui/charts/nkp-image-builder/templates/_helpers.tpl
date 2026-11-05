{{- define "nkpib.name" -}}
{{- .Chart.Name | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "nkpib.fullname" -}}
{{- if contains .Chart.Name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name .Chart.Name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}

{{- define "nkpib.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" }}
{{ include "nkpib.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{- define "nkpib.selectorLabels" -}}
app.kubernetes.io/name: {{ include "nkpib.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{- define "nkpib.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "nkpib.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{- define "nkpib.builderServiceAccountName" -}}
{{- printf "%s-builder" (include "nkpib.fullname" .) | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "nkpib.nutanixSecretName" -}}
{{- $existing := .Values.nutanix.existingSecret }}
{{- if and $existing (not (kindIs "string" $existing)) }}
{{- fail (printf "nutanix.existingSecret must be the NAME of a Secret (got %v); leave it \"\" to have the chart create one from nutanix.credentials" $existing) }}
{{- end }}
{{- default (printf "%s-nutanix" (include "nkpib.fullname" .)) $existing }}
{{- end }}

{{- define "nkpib.authSecretName" -}}
{{- default (printf "%s-auth" (include "nkpib.fullname" .)) .Values.auth.basic.existingSecret }}
{{- end }}

{{- define "nkpib.image" -}}
{{- printf "%s:%s" .repository (default $.root.Chart.AppVersion .tag) }}
{{- end }}
