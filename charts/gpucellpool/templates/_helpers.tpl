{{- define "gpucellpool.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "gpucellpool.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name (include "gpucellpool.name" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{- define "gpucellpool.labels" -}}
app.kubernetes.io/name: {{ include "gpucellpool.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" }}
{{- end -}}

{{- define "gpucellpool.selectorLabels" -}}
app.kubernetes.io/name: {{ include "gpucellpool.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "gpucellpool.serviceAccountName" -}}
{{ include "gpucellpool.fullname" . }}
{{- end -}}

{{/*
Serving certificate for the webhook.

Reuses the existing Secret when there is one, so an upgrade does not mint a new
certificate while the ValidatingWebhookConfiguration still carries the old
caBundle — which would fail every pool create until the next reconcile.
*/}}
{{- define "gpucellpool.webhookCert" -}}
{{- $fullname := include "gpucellpool.fullname" . -}}
{{- $svc := printf "%s-webhook" $fullname -}}
{{- $altNames := list (printf "%s.%s.svc" $svc .Release.Namespace) (printf "%s.%s.svc.cluster.local" $svc .Release.Namespace) -}}
{{- $existing := lookup "v1" "Secret" .Release.Namespace (printf "%s-webhook-cert" $fullname) -}}
{{- if and $existing $existing.data (index $existing.data "tls.crt") -}}
tls.crt: {{ index $existing.data "tls.crt" }}
tls.key: {{ index $existing.data "tls.key" }}
ca.crt: {{ index $existing.data "ca.crt" }}
{{- else -}}
{{- $ca := genCA (printf "%s-ca" $fullname) 3650 -}}
{{- $cert := genSignedCert $svc nil $altNames 3650 $ca -}}
tls.crt: {{ $cert.Cert | b64enc }}
tls.key: {{ $cert.Key | b64enc }}
ca.crt: {{ $ca.Cert | b64enc }}
{{- end -}}
{{- end -}}
