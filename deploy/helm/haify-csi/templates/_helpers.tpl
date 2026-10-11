{{- define "haify-csi.driver" -}}haify.csi.liliang-cn.com{{- end -}}

{{- define "haify-csi.labels" -}}
app.kubernetes.io/name: haify-csi
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ .Chart.Name }}-{{ .Chart.Version }}
{{- end -}}

{{- define "haify-csi.tokenSecret" -}}
{{- if .Values.token.existingSecret -}}{{ .Values.token.existingSecret }}{{- else -}}haify-api-token{{- end -}}
{{- end -}}

{{- define "haify-csi.image" -}}{{ .Values.image.repository }}:{{ .Values.image.tag }}{{- end -}}

{{/* The token, from the Secret; optional, so a controller without auth works. */}}
{{- define "haify-csi.tokenEnv" -}}
- name: HAIFY_TOKEN
  valueFrom:
    secretKeyRef: { name: {{ include "haify-csi.tokenSecret" . }}, key: token, optional: true }
{{- end -}}
