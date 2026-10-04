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
export PATH="/usr/bin/go/bin:${HOME}/google-cloud-sdk/bin:${HOME}/go/bin:${HOME}/.local/bin:${PATH}"
export PATH="$(go env GOPATH)/bin:${PATH}"

# Load local .env if present; fall back to parent .ate-dev-env.sh if present.
if [[ -f "${REPO_ROOT}/.env" ]]; then
  # shellcheck disable=SC1091
  source "${REPO_ROOT}/.env"
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
  if [[ -z "${ATE_CREDENTIAL_PROVIDER:-}" ]]; then
    export ATE_CREDENTIAL_PROVIDER='{"name":"k8s.io"}'
  fi
  export KO_DEFAULTPLATFORMS="${KO_DEFAULTPLATFORMS:-linux/amd64}"
  if [[ -z "${SUBSTRATE_ROOT:-}" ]]; then
    for candidate in \
      "${REPO_ROOT}/.." \
      "${REPO_ROOT}/../../agent-substrate/substrate" \
      "${REPO_ROOT}/../../agent-substrate/b-substrate"; do
      if [[ -d "${candidate}/tools/setup-gcp" ]]; then
        SUBSTRATE_ROOT="$(cd "${candidate}" && pwd)"
        break
      fi
    done
    export SUBSTRATE_ROOT="${SUBSTRATE_ROOT:-$(cd "${REPO_ROOT}/.." && pwd)}"
  fi
}

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

    ./hack/install-ate.sh \
      --deploy-ate-system \
      --credential-provider="${ATE_CREDENTIAL_PROVIDER}"
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
    # Discover an existing ateom-gvisor image from the cluster if one is already deployed,
    # or build it from SUBSTRATE_ROOT via ko.
    ATEOM_GVISOR_IMAGE="$(kubectl get workerpools -A -o jsonpath='{.items[0].spec.workerImage}' 2>/dev/null || true)"
    if [[ -z "${ATEOM_GVISOR_IMAGE}" && -n "${SUBSTRATE_ROOT:-}" && -d "${SUBSTRATE_ROOT}/cmd/ateom-gvisor" ]]; then
      echo "Building ateom-gvisor worker image via ko..."
      ATEOM_GVISOR_IMAGE="$(cd "${SUBSTRATE_ROOT}" && ko build ./cmd/ateom-gvisor)"
    fi
  fi
  export BUCKET_NAME SUBSTRATE_VERSION ATEOM_GVISOR_IMAGE
}

deploy_all() {
  detect_cluster_defaults
  banner "Deploying 'Anything But Agents' WorkerPool & 5 ActorTemplates"

  echo "Using BUCKET_NAME=${BUCKET_NAME}"
  echo "Using SUBSTRATE_VERSION=${SUBSTRATE_VERSION}"
  echo "Using ATEOM_GVISOR_IMAGE=${ATEOM_GVISOR_IMAGE}"
  echo "Using KO_DOCKER_REPO=${KO_DOCKER_REPO:-}"

  envsubst < "${REPO_ROOT}/manifests/workerpool.yaml.tmpl" | kubectl apply -f -
  kubectl rollout status deployment/anything-but-agents -n anything-but-agents --timeout=300s

  if ! kubectl get secret dungeon2d-gateway -n anything-but-agents >/dev/null 2>&1; then
    kubectl create secret generic dungeon2d-gateway \
      -n anything-but-agents \
      --from-literal=PRESENTER_TOKEN="$(openssl rand -hex 24)"
  fi

  kubectl ate create atespace "${ATESPACE}" 2>/dev/null || true

  for actor in hero-mofi hero-timeline-b d2d-mofi d2d-mofi-timeline-b clone-1 clone-2 clone-3 db-acme db-globex honeypot-203-0-113-5 honeypot-198-51-100-9 raft-leader-1; do
    kubectl ate delete actor "${actor}" -a "${ATESPACE}" --any-state 2>/dev/null || true
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
  done

  echo ""
  echo "Waiting for all Golden Snapshots to become READY (coldstart takes ~12s by design)..."
  for tmpl in dungeon dungeon2d coldstart tenantdb honeypot timewarp; do
    for ((i = 1; i <= 60; i++)); do
      yaml_out="$(kubectl ate get actor-template "${tmpl}" -a "${ATESPACE}" -o yaml 2>/dev/null || true)"
      golden_tag="$(echo "${yaml_out}" | awk '/takeGoldenSnapshotAt:/ {print $2; exit}')"
      err_msg="$(echo "${yaml_out}" | awk -F': ' '/errorMessage:/ {print $2; exit}')"
      if [[ -n "${golden_tag}" ]]; then
        echo "  [READY] ${tmpl} (snapshot taken at: ${golden_tag})"
        break
      fi
      if [[ -n "${err_msg}" ]]; then
        echo "  [ERROR] ${tmpl}: ${err_msg}" >&2
        exit 1
      fi
      sleep 3
    done
  done
  kubectl ate get actor-templates -a "${ATESPACE}"
}

ensure_router_port_forward() {
  if ! curl -s -o /dev/null --connect-timeout 1 "${ROUTER_URL}/" 2>/dev/null; then
    echo "Starting background port-forward to svc/atenet-router on ${ROUTER_URL}..."
    kubectl port-forward -n ate-system svc/atenet-router 8000:80 >/dev/null 2>&1 &
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
# RUNNING or PAUSED actors in the atespace.
free_workers() {
  local active_actors
  active_actors="$(kubectl ate get actors -a "${ATESPACE}" 2>/dev/null \
    | awk '$4 == "ACTOR_STATE_RUNNING" || $4 == "ACTOR_STATE_PAUSED" {print $2}' || true)"
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
        kubectl ate delete actor "${resource}" -a "${ATESPACE}" --any-state >/dev/null 2>&1 || true
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
  run_cmd kubectl ate suspend actor hero-mofi -a "${ATESPACE}"
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

  step "1. Start 2D hero session 'mofi' -> Wakes actor on WorkerPool, initializes Floor 1 heap, and suspends to GCS"
  run_cmd curl -fsS -X POST "${gateway_url}/api/session/start" -d '{"hero_name":"mofi","character_class":"paladin"}'

  step "2. Clear Floor 1 (The Forgotten Crypt): defeat Skeleton, grab Crypt Key, and descend to Floor 2"
  run_cmd curl -fsS -X POST "${gateway_url}/api/session/action" -d '{"actor_name":"d2d-mofi","action":"combat","target":"Restless Skeleton","entity_id":"f1:skeleton"}'
  run_cmd curl -fsS -X POST "${gateway_url}/api/session/action" -d '{"actor_name":"d2d-mofi","action":"pickup","target":"crypt-key","entity_id":"f1:key"}'
  run_cmd curl -fsS -X POST "${gateway_url}/api/session/action" -d '{"actor_name":"d2d-mofi","action":"stairs","target":"f1:stairs"}'

  step "3. Floor 2 (Abandoned Armory): pick up Dragon-Slayer Sword, activate Quicksave Shrine, and descend to Floor 3 (skipping Fire Shield!)"
  run_cmd curl -fsS -X POST "${gateway_url}/api/session/action" -d '{"actor_name":"d2d-mofi","action":"pickup","target":"dragon-slayer-sword","entity_id":"f2:sword"}'
  run_cmd curl -fsS -X POST "${gateway_url}/api/session/action" -d '{"actor_name":"d2d-mofi","action":"quicksave","target":"f2:shrine"}'
  run_cmd curl -fsS -X POST "${gateway_url}/api/session/action" -d '{"actor_name":"d2d-mofi","action":"stairs","target":"f2:stairs"}'

  step "4. Floor 3 Boss Fight: attack the Ancient Heap Dragon without Fire Shield -> YOU DIE (Alive=false in RAM)!"
  run_cmd curl -fsS -X POST "${gateway_url}/api/session/action" -d '{"actor_name":"d2d-mofi","action":"combat","target":"Ancient Heap Dragon","entity_id":"f3:dragon"}'

  step "5. SAVE-SCUM! Revert actor directly back to Snapshot v6 (Floor 2 Shrine) via kubectl ate revert"
  run_cmd curl -fsS -X POST "${gateway_url}/api/session/revert" -d '{"actor_name":"d2d-mofi","target_version":6}'

  step "6. Grab the Fire Shield on Floor 2, descend to Floor 3, slay the Ancient Heap Dragon, claim the Golden Chalice, and escape!"
  run_cmd curl -fsS -X POST "${gateway_url}/api/session/action" -d '{"actor_name":"d2d-mofi","action":"pickup","target":"fire-shield","entity_id":"f2:shield"}'
  run_cmd curl -fsS -X POST "${gateway_url}/api/session/action" -d '{"actor_name":"d2d-mofi","action":"stairs","target":"f2:stairs"}'
  run_cmd curl -fsS -X POST "${gateway_url}/api/session/action" -d '{"actor_name":"d2d-mofi","action":"combat","target":"Ancient Heap Dragon","entity_id":"f3:dragon"}'
  run_cmd curl -fsS -X POST "${gateway_url}/api/session/action" -d '{"actor_name":"d2d-mofi","action":"combat","target":"Ancient Heap Dragon","entity_id":"f3:dragon"}'
  run_cmd curl -fsS -X POST "${gateway_url}/api/session/action" -d '{"actor_name":"d2d-mofi","action":"pickup","target":"golden-chalice","entity_id":"f3:chalice"}'
  run_cmd curl -fsS -X POST "${gateway_url}/api/session/action" -d '{"actor_name":"d2d-mofi","action":"escape","target":"f3:portal"}'

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
  kubectl ate delete actor-template dungeon2d -a "${ATESPACE}" 2>/dev/null || true
  (
    cd "${REPO_ROOT}"
    envsubst < "manifests/dungeon2d-template.yaml.tmpl" \
      | ko resolve -f - \
      | kubectl ate create actor-template -f -
  )

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
  echo "  Player 2D Game URL (Public) : http://${lb_ip:-<pending>}/play"
  echo "  Presenter Dashboard Unlock  : http://${lb_ip:-<pending>}/dashboard?key=\$(kubectl get secret dungeon2d-gateway -n anything-but-agents -o jsonpath='{.data.PRESENTER_TOKEN}' | base64 -d)"
  echo ""
}

cleanup_dungeon2d_gateway() {
  banner "Cleaning up Dungeon 2D Gateway Deployment & Actors"
  for act in $(kubectl ate get actors -a "${ATESPACE}" 2>/dev/null | awk '$2 ~ /^d2d-/ {print $2}' || true); do
    kubectl ate delete actor "${act}" -a "${ATESPACE}" --any-state 2>/dev/null || true
  done
  for tag in $(kubectl ate get tags -a "${ATESPACE}" 2>/dev/null | awk '$2 ~ /^tag-d2d-/ {print $2}' || true); do
    kubectl ate delete tag "${tag}" -a "${ATESPACE}" 2>/dev/null || true
  done
  kubectl delete deployment,service,secret,serviceaccount dungeon2d-gateway -n anything-but-agents --ignore-not-found
  kubectl delete role,rolebinding dungeon2d-gateway-token-minter -n ate-system --ignore-not-found
  kubectl delete clusterrole,clusterrolebinding dungeon2d-gateway-ate-client --ignore-not-found
  free_workers
}

act2_coldstart() {
  ensure_router_port_forward
  banner "ACT II: 'The Instant Cloner' — Skipping a 12-Second Cold Start via Golden Snapshots"
  reset_act_resources clone-1 clone-2 clone-3

  step "1. Create 3 separate actors from the 'coldstart' template (whose Golden Snapshot already ran the 12s prime sieve)"
  run_cmd kubectl ate create actor clone-1 -a "${ATESPACE}" --template coldstart
  run_cmd kubectl ate create actor clone-2 -a "${ATESPACE}" --template coldstart
  run_cmd kubectl ate create actor clone-3 -a "${ATESPACE}" --template coldstart

  step "2. Query clone-1 and clone-2 — both wake in ~100ms with the SAME golden_heap_fingerprint and 78,498 primes in RAM!"
  run_cmd curl -fsS -H "ate-target-actor: ${ATESPACE}/clone-1" "${ROUTER_URL}/?add_symbol=user_a_module"
  run_cmd curl -fsS -H "ate-target-actor: ${ATESPACE}/clone-2" "${ROUTER_URL}/?add_symbol=user_b_module"

  free_workers
}

act3_tenantdb() {
  ensure_router_port_forward
  banner "ACT III: 'One Database Per Customer' — DurableDir + FULL Pause vs DATA Commit"
  reset_act_resources db-acme db-globex

  step "1. Create two isolated single-tenant database actors: 'db-acme' and 'db-globex'"
  run_cmd kubectl ate create actor db-acme -a "${ATESPACE}" --template tenantdb
  run_cmd kubectl ate create actor db-globex -a "${ATESPACE}" --template tenantdb

  step "2. Insert rows into db-acme's DurableDir (/data/wal.jsonl) and warm its RAM counter"
  run_cmd curl -fsS -H "ate-target-actor: ${ATESPACE}/db-acme" "${ROUTER_URL}/?key=tier&val=enterprise"
  run_cmd curl -fsS -H "ate-target-actor: ${ATESPACE}/db-acme" "${ROUTER_URL}/?key=region&val=us-west3-slc"

  step "3. PAUSE db-acme (onPause: FULL) -> resume it: both DurableDir rows AND ram_queries_since_boot survive!"
  run_cmd kubectl ate pause actor db-acme -a "${ATESPACE}"
  run_cmd curl -fsS -H "ate-target-actor: ${ATESPACE}/db-acme" "${ROUTER_URL}/"

  step "4. SUSPEND db-acme (onCommit: DATA) -> resume it: DurableDir rows survive via WAL replay, while RAM cold-boots cleanly!"
  run_cmd kubectl ate suspend actor db-acme -a "${ATESPACE}"
  run_cmd curl -fsS -H "ate-target-actor: ${ATESPACE}/db-acme" "${ROUTER_URL}/"

  step "5. GDPR Right-to-be-Forgotten in one command: delete tenant 'db-acme'"
  run_cmd kubectl ate delete actor db-acme -a "${ATESPACE}" --any-state

  free_workers
}

act4_honeypot() {
  ensure_router_port_forward
  banner "ACT IV: 'The Truman Show Honeypot' — On-Demand Per-Attacker gVisor Deception"
  reset_act_resources honeypot-203-0-113-5 honeypot-198-51-100-9

  step "1. Create honeypot actors for two suspicious scanner IPs"
  run_cmd kubectl ate create actor honeypot-203-0-113-5 -a "${ATESPACE}" --template honeypot
  run_cmd kubectl ate create actor honeypot-198-51-100-9 -a "${ATESPACE}" --template honeypot

  step "2. Attacker 203.0.113.5 'compromises' their sandbox and drops /tmp/honeypot/rootkit.sh"
  run_cmd curl -fsS -H "ate-target-actor: ${ATESPACE}/honeypot-203-0-113-5" \
    "${ROUTER_URL}/?drop_file=rootkit.sh&content=curl+evil.example/beacon&cmd=id"

  step "3. Attacker disconnects -> suspend honeypot-203-0-113-5 to GCS (\$0 compute while idle)"
  run_cmd kubectl ate suspend actor honeypot-203-0-113-5 -a "${ATESPACE}"

  step "4. Meanwhile, Attacker 198.51.100.9 probes the system and sees a completely clean filesystem"
  run_cmd curl -fsS -H "ate-target-actor: ${ATESPACE}/honeypot-198-51-100-9" "${ROUTER_URL}/"

  step "5. Weeks later, Attacker 203.0.113.5 returns — their sandbox wakes in ~100ms with rootkit.sh still in /tmp!"
  run_cmd curl -fsS -H "ate-target-actor: ${ATESPACE}/honeypot-203-0-113-5" "${ROUTER_URL}/?cmd=ls+-la+/tmp/honeypot"

  free_workers
}

act5_timewarp() {
  ensure_router_port_forward
  banner "ACT V: 'Don't Try This at Home' — Wall-Clock Time-Jump Shock & Raft Split-Brain"
  reset_act_resources raft-leader-1

  step "1. Create 'raft-leader-1' from the 'timewarp' template and verify it is HEALTHY"
  run_cmd kubectl ate create actor raft-leader-1 -a "${ATESPACE}" --template timewarp
  run_cmd curl -fsS -H "ate-target-actor: ${ATESPACE}/raft-leader-1" "${ROUTER_URL}/"

  step "2. Suspend 'raft-leader-1' for 5 seconds (longer than its 3-second leader lease TTL!)"
  run_cmd kubectl ate suspend actor raft-leader-1 -a "${ATESPACE}"
  echo "Sleeping 5 seconds while raft-leader-1 is frozen in object storage..."
  sleep 5

  step "3. Wake 'raft-leader-1' with an HTTP request and observe the Time-Travel Shock & Split-Brain Panic!"
  run_cmd curl -fsS -H "ate-target-actor: ${ATESPACE}/raft-leader-1" "${ROUTER_URL}/"

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
  for actor in hero-mofi hero-timeline-b d2d-mofi d2d-mofi-timeline-b clone-1 clone-2 clone-3 db-acme db-globex honeypot-203-0-113-5 honeypot-198-51-100-9 raft-leader-1; do
    kubectl ate delete actor "${actor}" -a "${ATESPACE}" --any-state 2>/dev/null || true
  done
  kubectl ate delete tag before-dragon -a "${ATESPACE}" 2>/dev/null || true
  for tmpl in dungeon dungeon2d coldstart tenantdb honeypot timewarp; do
    kubectl ate delete actor-template "${tmpl}" -a "${ATESPACE}" 2>/dev/null || true
  done
  kubectl ate delete atespace "${ATESPACE}" 2>/dev/null || true
  kubectl delete namespace anything-but-agents --ignore-not-found
}

case "${1:-}" in
  bootstrap-gke)     bootstrap_gke ;;
  deploy)            deploy_all ;;
  deploy-dungeon2d)  deploy_dungeon2d_gateway ;;
  cleanup-dungeon2d) cleanup_dungeon2d_gateway ;;
  run-all)           run_all_acts ;;
  act1-dungeon)      act1_dungeon ;;
  act1b-dungeon2d)   act1b_dungeon2d ;;
  act2-coldstart)    act2_coldstart ;;
  act3-tenantdb)     act3_tenantdb ;;
  act4-honeypot)     act4_honeypot ;;
  act5-timewarp)     act5_timewarp ;;
  cleanup)           cleanup_all ;;
  *)
    echo "Usage: $0 {bootstrap-gke|deploy|deploy-dungeon2d|cleanup-dungeon2d|run-all|act1-dungeon|act1b-dungeon2d|act2-coldstart|act3-tenantdb|act4-honeypot|act5-timewarp|cleanup}" >&2
    exit 1
    ;;
esac
