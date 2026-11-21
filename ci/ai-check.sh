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
# Which inference mode this target is meant to run. The CPU check asserts a
# backend requests NO GPU; on a GPU target that is exactly wrong, so the
# expectation has to be a property of the target, not of the script.
#
#   ai_gpu=true  in the provision env (or AI_GPU=1) -> assert a card IS used
ai_gpu="${AI_GPU:-${ai_gpu:-false}}"

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

if [ "$ai_gpu" = "true" ]; then
  # GPU target: the backend must request a card, use the CUDA image, and the node
  # must actually advertise one. Requesting without advertising is the failure
  # that looks like "the app will not start".
  if [ -z "$gpu_requested" ]; then
    bad "@check:llm_gpu_mode_requests_a_card this target is a GPU target but no backend requests nvidia.com/gpu"
    echo "  set backends[].gpu.enabled=true in the catalog override for this host"
    fails=$((fails + 1))
  else
    ok "@check:llm_gpu_mode_requests_a_card backends request nvidia.com/gpu:$gpu_requested"
  fi
  allocated="$(node_root <<EOS 2>/dev/null
export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
/usr/local/bin/kubectl get node -o jsonpath='{.items[0].status.allocatable.nvidia\.com/gpu}' 2>/dev/null \
  || true
EOS
)"
  allocated="$(printf '%s' "$allocated" | tr -d '\r\n ')"
  if [ -z "$allocated" ] || [ "$allocated" = "0" ]; then
    bad "@check:llm_gpu_mode_requests_a_card the node advertises no nvidia.com/gpu"
    echo "  the device plugin is not registering a card; check the driver and runtime"
    fails=$((fails + 1))
  else
    ok "node advertises nvidia.com/gpu=$allocated"
  fi
  imgs="$(node_root <<EOS 2>/dev/null
export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
/usr/local/bin/kubectl -n $ns get deploy -l app.kubernetes.io/name=llamacpp \
  -o jsonpath='{range .items[*]}{.spec.template.spec.containers[0].image}{"\n"}{end}'
EOS
)"
  if printf '%s' "$imgs" | grep -q "server-cuda"; then
    ok "backend runs the CUDA image"
  else
    bad "a GPU target must use llama.cpp:server-cuda; got: $(printf '%s' "$imgs" | tr '\n' ' ')"
    fails=$((fails + 1))
  fi
else
  if [ -n "$gpu_requested" ]; then
    bad "@check:llm_cpu_mode_requests_no_gpu backends request nvidia.com/gpu:$gpu_requested"
    echo "  CPU mode must not need a device plugin; set backends[].gpu.enabled=false"
    fails=$((fails + 1))
  else
    ok "@check:llm_cpu_mode_requests_no_gpu no backend requests a GPU"
  fi
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
