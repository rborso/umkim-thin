{{/*
The public home-page host.

Defaults to home.<rest> when the console host is console.<rest> (the documented
convention), else home.<consoleHost>. A value that collides with the console is
a hard error: both would claim the same Authelia-gated host.

Used by the home Ingress and by the console's shortcut link, so the two can
never disagree.
*/}}
{{- define "umkim-thin.homeHost" -}}
{{- $consoleHost := required "consoleHost is required (e.g. console.example.com)" .Values.consoleHost -}}
{{- $homeHost := .Values.homeHost -}}
{{- if not $homeHost -}}
{{- if hasPrefix "console." $consoleHost -}}
{{- $homeHost = replace "console." "home." $consoleHost -}}
{{- else -}}
{{- $homeHost = printf "home.%s" $consoleHost -}}
{{- end -}}
{{- end -}}
{{- if eq $homeHost $consoleHost -}}
{{- fail "homeHost must differ from consoleHost" -}}
{{- end -}}
{{- $homeHost -}}
{{- end -}}
