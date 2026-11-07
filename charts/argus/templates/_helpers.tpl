{{- define "argus.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- define "argus.fullname" -}}
{{- default .Release.Name .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- define "argus.clusterRoleName" -}}
{{- $identity := toJson (list .Release.Namespace (include "argus.fullname" .)) -}}
{{- $prefix := printf "%s-%s" .Release.Namespace (include "argus.fullname" .) | trunc 46 | trimSuffix "-" -}}
{{- printf "%s-%s" $prefix (sha256sum $identity | trunc 16) -}}
{{- end -}}
{{- define "argus.selectorLabels" -}}
app.kubernetes.io/name: {{ include "argus.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}
{{- define "argus.labels" -}}
{{ include "argus.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | quote }}
{{- end -}}
{{- define "argus.serviceAccount" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "argus.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- required "serviceAccount.name is required when create=false" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}
{{- define "argus.image" -}}
{{- if .Values.image.digest -}}
{{- printf "%s@%s" .Values.image.repository .Values.image.digest -}}
{{- else -}}
{{- printf "%s:%s" .Values.image.repository (default .Chart.AppVersion .Values.image.tag) -}}
{{- end -}}
{{- end -}}
{{- define "argus.tlsSecret" -}}
{{- if .Values.tls.certificate.create -}}
{{- printf "%s-tls" (include "argus.fullname" .) -}}
{{- else -}}
{{- required "tls.existingSecret must name a Secret with tls.crt and tls.key, or set tls.certificate.create" .Values.tls.existingSecret -}}
{{- end -}}
{{- end -}}
{{- define "argus.scheme" -}}
{{- if .Values.tls.enabled }}HTTPS{{ else }}HTTP{{ end -}}
{{- end -}}

{{/*
argus.config renders the strict JSON configuration: the global timing, the
chart-managed paths, and one section per enabled check without "enabled".
*/}}
{{- define "argus.config" -}}
{{- $config := deepCopy .Values.config -}}
{{- $_ := set $config "listenAddress" ":8443" -}}
{{- $_ := set $config "tokenFile" "/var/run/argus-auth/token" -}}
{{- $_ := set $config "state" (dict "namespace" .Release.Namespace "name" .Values.state.configMapName) -}}
{{- if .Values.tls.enabled -}}
{{- $_ := set $config "tls" (dict "certFile" "/var/run/argus-tls/tls.crt" "keyFile" "/var/run/argus-tls/tls.key") -}}
{{- end -}}
{{- $checks := dict -}}
{{- range $name, $section := .Values.checks -}}
{{- if $section.enabled -}}
{{- $copy := omit (deepCopy $section) "enabled" -}}
{{- if eq $name "hosts" -}}
{{- $_ := set $copy "privateKeyFile" "/var/run/argus-ssh/ssh_private_key" -}}
{{- $_ := set $copy "knownHostsFile" "/config/known_hosts" -}}
{{- end -}}
{{- $_ := set $checks $name $copy -}}
{{- end -}}
{{- end -}}
{{- $_ := set $config "checks" $checks -}}
{{- $config | toPrettyJson -}}
{{- end -}}

{{/*
argus.namespaceRules maps namespace -> apiGroup -> resources for the
namespaced list permissions of the enabled checks, as JSON.
*/}}
{{- define "argus.namespaceRules" -}}
{{- $rules := dict -}}
{{- $checks := .Values.checks -}}
{{- if $checks.pgbackrest.enabled -}}
{{- range $checks.pgbackrest.clusters -}}
{{- include "argus.addRule" (list $rules .namespace "batch" "jobs" "cronjobs") -}}
{{- end -}}
{{- end -}}
{{- if $checks.cronjobs.enabled -}}
{{- range $checks.cronjobs.items -}}
{{- include "argus.addRule" (list $rules .namespace "batch" "jobs" "cronjobs") -}}
{{- end -}}
{{- end -}}
{{- if $checks.longhorn.enabled -}}
{{- include "argus.addRule" (list $rules $checks.longhorn.namespace "longhorn.io" "volumes" "replicas" "backuptargets" "systembackups") -}}
{{- end -}}
{{- if and $checks.nodes.enabled $checks.nodes.longhornNamespace -}}
{{- include "argus.addRule" (list $rules $checks.nodes.longhornNamespace "longhorn.io" "nodes") -}}
{{- end -}}
{{- if $checks.argocd.enabled -}}
{{- include "argus.addRule" (list $rules $checks.argocd.namespace "argoproj.io" "applications") -}}
{{- end -}}
{{- $rules | toJson -}}
{{- end -}}

{{- define "argus.addRule" -}}
{{- $rules := index . 0 -}}
{{- $namespace := required "every inventory entry needs a namespace" (index . 1) -}}
{{- $group := index . 2 -}}
{{- $groups := get $rules $namespace | default dict -}}
{{- $resources := get $groups $group | default list -}}
{{- range (slice . 3) -}}
{{- $resources = append $resources . -}}
{{- end -}}
{{- $_ := set $groups $group ($resources | uniq | sortAlpha) -}}
{{- $_ := set $rules $namespace $groups -}}
{{- end -}}

{{/*
argus.clusterRules maps apiGroup -> resources for the cluster-wide get/list
permissions of the enabled checks, as JSON.
*/}}
{{- define "argus.clusterRules" -}}
{{- $groups := dict -}}
{{- $checks := .Values.checks -}}
{{- if $checks.nodes.enabled -}}
{{- include "argus.addGroup" (list $groups "" "nodes") -}}
{{- if ne (toString $checks.nodes.metrics) "false" -}}
{{- include "argus.addGroup" (list $groups "metrics.k8s.io" "nodes") -}}
{{- end -}}
{{- end -}}
{{- if or $checks.postgres.enabled $checks.pgbackrest.enabled -}}
{{- include "argus.addGroup" (list $groups "postgres-operator.crunchydata.com" "postgresclusters") -}}
{{- end -}}
{{- if $checks.certificates.enabled -}}
{{- include "argus.addGroup" (list $groups "cert-manager.io" "certificates" "certificaterequests" "issuers" "clusterissuers") -}}
{{- end -}}
{{- if $checks.pki.enabled -}}
{{- include "argus.addGroup" (list $groups "cert-manager.io" "certificates" "certificaterequests") -}}
{{- if $checks.pki.expectedBundles -}}
{{- include "argus.addGroup" (list $groups "trust.cert-manager.io" "bundles") -}}
{{- end -}}
{{- end -}}
{{- if $checks.externalsecrets.enabled -}}
{{- include "argus.addGroup" (list $groups "external-secrets.io" "externalsecrets" "clusterexternalsecrets" "secretstores" "clustersecretstores") -}}
{{- end -}}
{{- $groups | toJson -}}
{{- end -}}

{{- define "argus.addGroup" -}}
{{- $groups := index . 0 -}}
{{- $group := index . 1 -}}
{{- $resources := get $groups $group | default list -}}
{{- range (slice . 2) -}}
{{- $resources = append $resources . -}}
{{- end -}}
{{- $_ := set $groups $group ($resources | uniq | sortAlpha) -}}
{{- end -}}
