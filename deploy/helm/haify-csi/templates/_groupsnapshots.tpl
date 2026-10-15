{{/*
"true" when the csi-snapshotter runs with group snapshots on: groupSnapshots.enabled
true, or "auto" and the cluster serves groupSnapshots.apiVersion. With the
feature gate on and the CRDs missing, the sidecar stops handling ordinary
VolumeSnapshots as well, which is why "auto" looks first.
*/}}
{{- define "haify-csi.groupSnapshots" -}}
{{- $g := .Values.groupSnapshots -}}
{{- if eq (toString $g.enabled) "auto" -}}
{{- if .Capabilities.APIVersions.Has (printf "%s/VolumeGroupSnapshotClass" $g.apiVersion) -}}true{{- end -}}
{{- else if eq (toString $g.enabled) "true" -}}true{{- end -}}
{{- end -}}
