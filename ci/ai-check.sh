#!/usr/bin/env bash
# Cluster-level checks for the AI stack that a browser test cannot make.
#
#   ./ci/ai-check.sh single
#   ./ci/ai-check.sh ha --env deploy/provision-ha.env
#
# The browser gate proves the stack answers (model list, completion). This proves
# the two things that live in the pod spec instead:
#
#   llm_cpu_mode_requests_no_gpu - no backend asks for nvidia.com/gpu, so CPU
#     mode keeps working on a cluster with no driver and no device plugin. This
#     is the check that stops a GPU requirement creeping into the default.
#   llm_backends_are_separate - every backend is its own Deployment, not replicas
#     of one. LiteLLM balances across api_base entries, so a single Service with
#     two replicas would look like one backend to the router and pooling would
#     silently do nothing.
set -uo pipefail
cd "$(dirname "$0")/.."

TARGET="${1:?usage: ci/ai-check.sh single|ha}"
shift || true
export TARGET
export PROVISION_ENV="${PROVISION_ENV:-deploy/provision-${TARGET}.env}"
# shellcheck source=ci/lib.sh
source ci/lib.sh

ns="${AI_NAMESPACE:-ai}"
release="${AI_RELEASE:-ai}"

# kube <args...> : run kubectl on the target node as root.
#
# The command is passed as arguments rather than spliced into a heredoc: a
# heredoc through `ssh 'sudo bash -s'` silently mangles backslash escapes, which
# is how an earlier version of this script reported "no backend found" while a
# direct kubectl returned a row.
kube() {
  local quoted=""
  printf -v quoted '%q ' "$@"
  node_root <<EOS
export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
/usr/local/bin/kubectl -n $(printf '%q' "$ns") $quoted
EOS
}

echo "== ai stack: pod spec (${TARGET}) =="

# One row per backend: name, then the GPU limit (empty when unset).
spec="$(kube get deploy -l app.kubernetes.io/name=llamacpp -o "custom-columns=NAME:.metadata.name,GPU:.spec.template.spec.containers[0].resources.limits.nvidia\.com/gpu,REPLICAS:.spec.replicas" --no-headers 2>/dev/null)"

if [ -z "$spec" ]; then
  bad "no llama.cpp backend found in namespace $ns"
  echo
  echo "AI CHECKS FAILED: 1 ($TARGET)"
  exit 1
fi

gpu_requested=""
multi=""
backends=0
while read -r name gpu reps _; do
  [ -n "$name" ] || continue
  backends=$((backends + 1))
  case "$gpu" in
    "" | "<none>") ;;
    *) gpu_requested="$gpu_requested $name($gpu)" ;;
  esac
  case "$reps" in
    "" | "1" | "<none>") ;;
    *) multi="$multi $name($reps)" ;;
  esac
done <<<"$spec"

if [ -n "$gpu_requested" ]; then
  bad "@check:llm_cpu_mode_requests_no_gpu backends request nvidia.com/gpu:$gpu_requested"
  echo "  CPU mode must not need a device plugin; set backends[].gpu.enabled=false"
  fails=$((fails + 1))
else
  ok "@check:llm_cpu_mode_requests_no_gpu no backend requests a GPU"
fi

if [ -n "$multi" ]; then
  bad "@check:llm_backends_are_separate a backend is scaled past one replica:$multi"
  echo "  add another backends[] entry instead; replicas hide the pool from LiteLLM"
  fails=$((fails + 1))
else
  ok "@check:llm_backends_are_separate $backends backend deployment(s), one replica each"
fi

# The gateway must see the models it was configured to publish.
models="$(kube exec "deploy/${release}-litellm" -- python3 -c '
import os, json, urllib.request
r = urllib.request.Request("http://127.0.0.1:4000/v1/models",
    headers={"Authorization": "Bearer " + os.environ["LITELLM_MASTER_KEY"]})
d = json.load(urllib.request.urlopen(r, timeout=20))
print(" ".join(sorted(m["id"] for m in d.get("data", []))))
' 2>/dev/null | tr -d '\r')"

if [ -z "$models" ]; then
  bad "the gateway did not answer /v1/models inside the pod"
  fails=$((fails + 1))
else
  ok "gateway publishes: $models"
fi

echo
if [ "$fails" -ne 0 ]; then
  echo "AI CHECKS FAILED: $fails ($TARGET)"
  exit 1
fi
echo "AI CHECKS PASSED ($TARGET)"
