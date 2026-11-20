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

echo "== ai stack: pod spec (${TARGET}) =="

spec="$(node_root <<EOS 2>/dev/null
export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
/usr/local/bin/kubectl -n $ns get deploy -l app.kubernetes.io/name=llamacpp \\
  -o jsonpath='{range .items[*]}{.metadata.name}{"\\t"}{.spec.template.spec.containers[0].resources.limits.nvidia.com/gpu}{"\\n"}{end}'
EOS
)"

if [ -z "$spec" ]; then
  bad "no llama.cpp backend found in namespace $ns"
  echo
  echo "AI CHECKS FAILED: 1 ($TARGET)"
  exit 1
fi

# Every backend must have an empty GPU limit. An absent key prints as nothing.
gpu_requested=""
while IFS=$'\t' read -r name gpu; do
  [ -n "$name" ] || continue
  if [ -n "$gpu" ]; then
    gpu_requested="$gpu_requested $name($gpu)"
  fi
done <<<"$spec"

if [ -n "$gpu_requested" ]; then
  bad "@check:llm_cpu_mode_requests_no_gpu backends request nvidia.com/gpu:$gpu_requested"
  echo "  CPU mode must not need a device plugin; set backends[].gpu.enabled=false"
  fails=$((fails + 1))
else
  ok "@check:llm_cpu_mode_requests_no_gpu no backend requests a GPU"
fi

# Each backend is a separate Deployment. With one, the "separate" property is
# trivially true; the check still runs so a future change to replicas is caught.
backends=$(printf '%s\n' "$spec" | grep -c . || true)
replicas="$(node_root <<EOS 2>/dev/null
export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
/usr/local/bin/kubectl -n $ns get deploy -l app.kubernetes.io/name=llamacpp \\
  -o jsonpath='{range .items[*]}{.metadata.name}={.spec.replicas}{"\\n"}{end}'
EOS
)"
multi=$(printf '%s\n' "$replicas" | awk -F= '$2+0 > 1 {print $1"="$2}')
if [ -n "$multi" ]; then
  bad "@check:llm_backends_are_separate a backend is scaled past one replica ($multi)"
  echo "  add another backends[] entry instead; replicas hide the pool from LiteLLM"
  fails=$((fails + 1))
else
  ok "@check:llm_backends_are_separate $backends backend deployment(s), one replica each"
fi

# The gateway must see the backends it was configured with.
models="$(node_root <<EOS 2>/dev/null
export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
/usr/local/bin/kubectl -n $ns exec deploy/${AI_RELEASE:-ai}-litellm -- \\
  python3 -c 'import os,urllib.request,json
r=urllib.request.Request("http://127.0.0.1:4000/v1/models", headers={"Authorization":"Bearer "+os.environ["LITELLM_MASTER_KEY"]})
d=json.load(urllib.request.urlopen(r, timeout=20))
print(" ".join(sorted(m["id"] for m in d.get("data",[]))))' 2>/dev/null
EOS
)"
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
