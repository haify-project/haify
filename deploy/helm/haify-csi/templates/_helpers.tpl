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

{{/* "true" when the chart puts DRBD on the nodes, by the loader or by KMM. */}}
{{- define "haify-csi.drbdManaged" -}}
{{- if or .Values.drbdModuleLoader.enabled .Values.drbdModuleLoader.kmm.enabled }}true{{ end -}}
{{- end -}}

{{/* The token, from the Secret; optional, so a controller without auth works. */}}
{{- define "haify-csi.tokenEnv" -}}
- name: HAIFY_TOKEN
  valueFrom:
    secretKeyRef: { name: {{ include "haify-csi.tokenSecret" . }}, key: token, optional: true }
{{- end -}}
