{{- /*
Render an image reference from a {registry, repository, tag} block.
Empty registry means the repository's default registry.
*/ -}}
{{- define "ci-builders.image" -}}
{{- if .registry }}{{ .registry }}/{{ end }}{{ .repository }}:{{ .tag }}
{{- end -}}

{{/*
One RELEASE image reference, digest-pinned when a release stamped one and
tagged otherwise. Distinct from `ci-builders.image` above, which is the
tag-only form the upstream components use and which has no digest to
stamp.
*/ -}}
{{- define "ci-builders.pinnedImage" -}}
{{- if .digest -}}
{{ .registry }}/{{ .repository }}@{{ .digest }}
{{- else -}}
{{ .registry }}/{{ .repository }}:{{ .tag }}
{{- end -}}
{{- end -}}

{{- /*
A node class by name (values.nodeClasses): the placement the caller
defines for a name a preset or a values file refers to, as JSON
({nodeSelector, tolerations}, each optional). An empty name is the empty
class. An undefined name is a render error -- a typo must not quietly
schedule a builder anywhere; a class that places nothing is defined empty
(`nodeClasses: {name: {}}`).

Takes a dict `{root, name}`.
*/ -}}
{{- define "ci-builders.nodeClass" -}}
{{- $root := .root -}}
{{- $name := .name -}}
{{- if $name -}}
{{- $classes := $root.Values.nodeClasses | default (dict) -}}
{{- if not (hasKey $classes $name) }}{{ fail (printf "nodeClass %q is not defined: add it under nodeClasses (defined: [%s]); a class that places nothing is `nodeClasses: {%s: {}}`" $name (keys $classes | sortAlpha | join ", ") $name) }}{{ end -}}
{{- get $classes $name | default (dict) | toJson -}}
{{- else -}}
{}
{{- end -}}
{{- end -}}

{{- /*
Where a pod goes, as JSON {nodeSelector, tolerations}.

`sched` is a per-component block with its own nodeClass / nodeSelector /
tolerations, or empty; `arch` is the architecture it is for, empty for a
component with none (the caches).

  - sched.nodeSelector / sched.tolerations, when set, are used as given
    (they REPLACE; the arch selector is then the caller's to write).
  - else sched.nodeClass, when set: the class's placement, plus the
    kubernetes.io/arch selector for `arch` (the class's own selector wins
    on a clash).
  - else the release-wide nodeSelector / tolerations, when not empty
    (plus the arch selector), else the release-wide nodeClass's.
*/ -}}
{{- define "ci-builders.placement" -}}
{{- $root := .root -}}
{{- $arch := .arch | default "" -}}
{{- $sched := .sched | default (dict) -}}
{{- $class := dict -}}
{{- if $sched.nodeClass }}{{ $class = include "ci-builders.nodeClass" (dict "root" $root "name" $sched.nodeClass) | fromJson }}{{ end -}}
{{- $global := include "ci-builders.nodeClass" (dict "root" $root "name" $root.Values.nodeClass) | fromJson -}}
{{- $nodeSelector := dict -}}
{{- $tolerations := list -}}
{{- if $sched.nodeSelector -}}
{{- $nodeSelector = $sched.nodeSelector -}}
{{- else -}}
{{- if $arch }}{{ $_ := set $nodeSelector "kubernetes.io/arch" $arch }}{{ end -}}
{{- if $sched.nodeClass -}}
{{- $nodeSelector = mergeOverwrite $nodeSelector ($class.nodeSelector | default (dict)) -}}
{{- else -}}
{{- $nodeSelector = mergeOverwrite $nodeSelector ($root.Values.nodeSelector | default $global.nodeSelector | default (dict)) -}}
{{- end -}}
{{- end -}}
{{- if $sched.tolerations -}}
{{- $tolerations = $sched.tolerations -}}
{{- else if $sched.nodeClass -}}
{{- $tolerations = $class.tolerations | default (list) -}}
{{- else -}}
{{- $tolerations = $root.Values.tolerations | default $global.tolerations | default (list) -}}
{{- end -}}
{{- dict "nodeSelector" $nodeSelector "tolerations" $tolerations | toJson -}}
{{- end -}}
