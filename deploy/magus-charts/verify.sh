#!/usr/bin/env bash
# Verify the SillyTavern ÚTK stack the way a user meets it.
#
# This is NOT a "are the pods Running" check: readiness only proves the app
# answered a probe. Each check below exercises something a person or a client
# actually does, and fails loudly when the answer is wrong.
#
#   ./verify.sh [namespace]
#
# Deliberately excluded: anything requiring a real model call, because that needs
# an API key the operator supplies. The model wiring is checked structurally
# instead (right source, right URL, right headers), which is what catches the
# class of bug where the settings file is silently ignored.
set -uo pipefail

NS="${1:-sillytavern}"
k() { kubectl -n "$NS" "$@"; }
fails=0
ok()   { printf '  ok    %s\n' "$1"; }
bad()  { printf '  FAIL  %s\n' "$1"; fails=$((fails + 1)); }
skip() { printf '  skip  %s\n' "$1"; }

echo "== $NS: the three pieces =="
for d in sillytavern-combat sillytavern-alkoto sillytavern-sillytavern; do
  if k get deploy "$d" >/dev/null 2>&1; then
    ready=$(k get deploy "$d" -o jsonpath='{.status.readyReplicas}' 2>/dev/null)
    [ "${ready:-0}" -ge 1 ] && ok "$d is available" || bad "$d is not available"
  else
    skip "$d is not installed (selectable, so this may be intended)"
  fi
done

echo "== SillyTavern serves its UI =="
title=$(k exec deploy/sillytavern-sillytavern -- \
  sh -c 'wget -qO- http://127.0.0.1:8000/ 2>/dev/null | grep -oE "<title>[^<]*"' 2>/dev/null)
case "$title" in
  *SillyTavern*) ok "the UI answers: ${title#<title>}" ;;
  *) bad "the UI did not answer (${title:-no response})" ;;
esac

echo "== settings.json is JSON SillyTavern can parse =="
# The chart wrote this file. A YAML body here is silently ignored by the app and
# it falls back to its own connection defaults, so the file is parsed the same way
# the app parses it.
parsed=$(k exec deploy/sillytavern-sillytavern -- node -e '
  const fs = require("fs");
  try {
    const s = JSON.parse(fs.readFileSync("/home/node/app/data/default-user/settings.json", "utf8"));
    const o = s.oai_settings || {};
    console.log([s.main_api, o.chat_completion_source, o.custom_url || o.openrouter_model || ""].join("|"));
  } catch (e) { console.log("PARSE_ERROR"); }
' 2>/dev/null | tr -d '\r')
if [ "$parsed" = "PARSE_ERROR" ] || [ -z "$parsed" ]; then
  bad "settings.json is not valid JSON (the app would use its own defaults)"
else
  ok "settings.json parses: $parsed"
  case "$parsed" in
    openai\|custom\|https://*) ok "a Custom OpenAI profile is configured" ;;
    openai\|openrouter\|*)     ok "an OpenRouter profile is configured" ;;
    *) bad "no model profile is configured: $parsed" ;;
  esac
fi

echo "== the pack is on the PVC =="
worlds=$(k exec deploy/sillytavern-sillytavern -- sh -c \
  'ls data/default-user/worlds/*.json 2>/dev/null | wc -l' 2>/dev/null | tr -d '\r')
[ "${worlds:-0}" -ge 10 ] && ok "$worlds world books" || bad "only ${worlds:-0} world books"
card=$(k exec deploy/sillytavern-sillytavern -- sh -c \
  'ls data/default-user/characters/Ynevi_Kalandmester_UTK.png 2>/dev/null' 2>/dev/null | tr -d '\r')
[ -n "$card" ] && ok "the KM character card is present" || bad "the character card is missing"

echo "== the MCP plugin and extensions are mounted =="
plugin=$(k exec deploy/sillytavern-sillytavern -- sh -c \
  'ls plugins/SillyTavern-MCP-Server/package.json 2>/dev/null' 2>/dev/null | tr -d '\r')
[ -n "$plugin" ] && ok "the MCP server plugin is mounted" \
                 || bad "the MCP server plugin is missing: ST cannot call tools"
ext=$(k exec deploy/sillytavern-sillytavern -- sh -c \
  'ls public/scripts/extensions/third-party/SillyTavern-MCP-Client/manifest.json 2>/dev/null' 2>/dev/null | tr -d '\r')
[ -n "$ext" ] && ok "the MCP client extension is mounted" || bad "the MCP client extension is missing"

echo "== the rules MCP answers, and SillyTavern can reach it =="
if k get deploy sillytavern-combat >/dev/null 2>&1; then
  health=$(k exec deploy/sillytavern-sillytavern -- \
    sh -c 'wget -qO- http://sillytavern-combat:3921/health 2>/dev/null' 2>/dev/null | tr -d '\r')
  case "$health" in
    *'"ok":true'*) ok "reachable from the ST pod: $health" ;;
    *) bad "not reachable from the ST pod (${health:-no response})" ;;
  esac
  # The URL in mcp_settings must be the Service, never 127.0.0.1: inside a pod
  # loopback is the SillyTavern container itself.
  mcpurl=$(k exec deploy/sillytavern-sillytavern -- node -e '
    const fs=require("fs");
    try {
      const m=JSON.parse(fs.readFileSync("/home/node/app/data/default-user/mcp_settings.json","utf8"));
      const s=(m.mcpServers||{})["utk-combat"];
      console.log(s?s.url:"NONE");
    } catch(e) { console.log("PARSE_ERROR"); }
  ' 2>/dev/null | tr -d '\r')
  case "$mcpurl" in
    PARSE_ERROR|NONE|"") bad "mcp_settings.json does not register utk-combat" ;;
    *127.0.0.1*|*localhost*) bad "mcp_settings points at loopback ($mcpurl): unreachable in-cluster" ;;
    *) ok "mcp_settings registers $mcpurl" ;;
  esac
fi

echo "== the character builder serves =="
if k get deploy sillytavern-alkoto >/dev/null 2>&1; then
  atitle=$(k exec deploy/sillytavern-alkoto -- python3 -c \
    'import urllib.request;print(urllib.request.urlopen("http://127.0.0.1:8765/",timeout=5).read().decode()[:400])' 2>/dev/null | tr -d '\r')
  case "$atitle" in
    *karakteralkotó*|*karakteralkoto*) ok "utk-alkoto serves its UI" ;;
    *) bad "utk-alkoto did not serve its UI" ;;
  esac
fi

echo "== routes are VPN-only =="
hosts=$(k get ingress -o jsonpath='{range .items[*]}{.spec.rules[*].host}{"\n"}{end}' 2>/dev/null | tr -d '\r')
if [ -z "$hosts" ]; then
  bad "no routes published"
else
  echo "$hosts" | while read -r h; do
    [ -n "$h" ] || continue
    case "$h" in
      *.vpn.*) printf '  ok    %s (VPN only)\n' "$h" ;;
      *)       printf '  FAIL  %s is NOT VPN-only\n' "$h"; exit 9 ;;
    esac
  done
  [ $? -eq 9 ] && fails=$((fails + 1))
fi

echo
if [ "$fails" -eq 0 ]; then
  echo "RESULT: the stack answers as a user would expect."
  echo "Not covered here: a real model completion, which needs your API key."
else
  echo "RESULT: $fails check(s) failed."
fi
exit "$fails"
