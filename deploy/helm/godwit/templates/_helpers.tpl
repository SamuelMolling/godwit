{{- define "godwit.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "godwit.fullname" -}}
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

{{- define "godwit.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "godwit.labels" -}}
helm.sh/chart: {{ include "godwit.chart" . }}
{{ include "godwit.selectorLabels" . }}
app.kubernetes.io/version: {{ include "godwit.imageTag" . | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- with .Values.commonLabels }}
{{ toYaml . }}
{{- end }}
{{- end }}

{{- define "godwit.selectorLabels" -}}
app.kubernetes.io/name: {{ include "godwit.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{- define "godwit.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "godwit.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{- define "godwit.imageTag" -}}
{{- default .Chart.AppVersion .Values.image.tag }}
{{- end }}

{{- define "godwit.image" -}}
{{- printf "%s:%s" .Values.image.repository (include "godwit.imageTag" .) }}
{{- end }}

{{- define "godwit.serverURL" -}}
{{- with .Values.targets.server }}
{{- . }}
{{- else }}
{{- printf "http://%s.%s.svc:%v" (include "godwit.fullname" .) .Release.Namespace .Values.service.port }}
{{- end }}
{{- end }}

{{- define "godwit.registerContainer" -}}
{{- $root := .root -}}
{{- $t := .target -}}
name: {{ printf "register-%s" ($t.name | lower | replace "_" "-" | replace "." "-") | trunc 63 | trimSuffix "-" }}
image: {{ include "godwit.image" $root }}
imagePullPolicy: {{ $root.Values.image.pullPolicy }}
{{- with $root.Values.securityContext }}
securityContext:
  {{- toYaml . | nindent 2 }}
{{- end }}
args:
  - target
  - add
  - {{ $t.name | quote }}
  - {{ printf "--provider=%s" (required "targets.list[].provider is required" $t.provider) | quote }}
  {{- if not $t.dsnSecret }}
  {{- with $t.dsn }}
  - {{ printf "--dsn=%s" . | quote }}
  {{- end }}
  {{- end }}
  {{- with $t.secretPath }}
  - {{ printf "--secret-path=%s" . | quote }}
  {{- end }}
  {{- with $t.vaultPath }}
  - {{ printf "--vault-path=%s" . | quote }}
  {{- end }}
  {{- with $t.vaultTemplate }}
  - {{ printf "--vault-template=%s" . | quote }}
  {{- end }}
  {{- with $t.searchPath }}
  - {{ printf "--search-path=%s" . | quote }}
  {{- end }}
  {{- with $t.lockTimeout }}
  - {{ printf "--lock-timeout=%s" . | quote }}
  {{- end }}
  {{- with $t.statementTimeout }}
  - {{ printf "--statement-timeout=%s" . | quote }}
  {{- end }}
  {{- if $t.requirePlan }}
  - --require-plan
  {{- end }}
  {{- if hasKey $t "keepOld" }}
  - {{ printf "--keep-old=%v" $t.keepOld | quote }}
  {{- end }}
  {{- range $t.extraArgs }}
  - {{ . | quote }}
  {{- end }}
env:
  - name: GODWIT_SERVER
    value: {{ include "godwit.serverURL" $root | quote }}
  {{- with $root.Values.targets.tokenSecret.name }}
  - name: GODWIT_TOKEN
    valueFrom:
      secretKeyRef:
        name: {{ . }}
        key: {{ $root.Values.targets.tokenSecret.key }}
  {{- end }}
  {{- with $t.dsnSecret }}
  - name: GODWIT_TARGET_DSN
    valueFrom:
      secretKeyRef:
        name: {{ .name }}
        key: {{ .key }}
  {{- end }}
  {{- with $root.Values.targets.extraEnv }}
  {{- toYaml . | nindent 2 }}
  {{- end }}
{{- with $root.Values.targets.resources }}
resources:
  {{- toYaml . | nindent 2 }}
{{- end }}
{{- with $root.Values.targets.extraVolumeMounts }}
volumeMounts:
  {{- toYaml . | nindent 2 }}
{{- end }}
{{- end }}
