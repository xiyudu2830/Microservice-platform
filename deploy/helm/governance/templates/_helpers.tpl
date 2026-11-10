{{- define "governance.prefix" -}}
{{- .Release.Name | trunc 44 | trimSuffix "-" -}}
{{- end -}}

{{- define "governance.labels" -}}
app.kubernetes.io/name: governance
app.kubernetes.io/instance: {{ .Release.Name | quote }}
{{- end -}}

{{- define "governance.image" -}}
{{- if .Values.image.digest -}}
{{ .Values.image.repository }}@{{ .Values.image.digest }}
{{- else -}}
{{ .Values.image.repository }}:{{ .Values.image.tag }}
{{- end -}}
{{- end -}}
