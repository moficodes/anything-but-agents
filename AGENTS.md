# AGENTS.md

Guidance for AI coding agents working in `anything-but-agents`.

## Project Overview

This is the companion repository (Go demo services, Substrate manifests, live-stage runner, and talk outline) for the talk **"Running Anything *But* Agents on Agent Substrate"**.

Without AI/LLM frameworks, **Agent Substrate (`ate`)** is a **Kubernetes-native, sub-second memory-checkpointing, wake-on-HTTP, gVisor-sandboxed actor runtime**. This project demonstrates its architectural primitives—and its failure modes—by multiplexing five non-agent workloads across a single 3-replica `WorkerPool`:

1. **`cmd/dungeon` ("Look Ma, No Database")**: Zork-style RPG where 100% of state (`GameState`: HP, inventory, room items, monster HP, `pod_ip_history`) lives in Go heap RAM with zero database or disk I/O. Demonstrates `SNAPSHOT_CONTENT_SCOPE_FULL` across pod migrations, save-scumming via `kubectl ate revert actor`, and timeline branching via `kubectl ate create tag` + `--tag`.
1b. **`cmd/dungeon2d` ("2D Roguelike & Presenter Dashboard")**: Dual-mode binary (`--mode=actor|gateway`) with a 2D canvas roguelike (`/play`, public) and a Kubernetes Secret token-gated presenter leaderboard (`/dashboard?token=...`). Demonstrates milestone-driven `kubectl ate save actor` (`v1..vN`), version-specific `kubectl ate revert actor --snapshot-version`, and timeline branching via `kubectl ate create tag` + `--tag`.
2. **`cmd/coldstart` ("The Instant Cloner & Production HPA QPS Race")**: Multi-mode binary (`--mode=worker|gateway|k8s-router|local`). Worker mode builds a 50,000-document in-memory trigram & token search index + prime lookup table (~76.6 MB post-GC Go heap) across 6 stages (`--init-delay=12s`) before `/readyz` flips from `503` to `200 OK`, executing real BM25 + 3-gram searches (~53k posting intersections, ~15ms compute) with a `20 QPS` per-replica concurrency cap (`--max-concurrent=4`, `--min-query-duration=200ms`). Gateway/local mode runs a synchronized traffic generator, a Kubernetes Secret token-gated (`Secret/coldstart-gateway` `PRESENTER_TOKEN`) Hardware Telemetry & Oscilloscope Rack Web Dashboard (`/dashboard?key=...` setting `cs_presenter` cookie or `X-Presenter-Key` header, with top-right icon-only Moon/Sun Projector Light Mode toggle), and a Kubernetes `external.metrics.k8s.io/v1beta1` HTTPS server (`coldstart_lb_qps`) comparing Standard K8s HPA (`Deployment/coldstart-k8s`) vs. Substrate Golden Snapshot HPA (`coldstart-1..3`) across an identical 2-hop `Service` → L7 Router (`svc/coldstart-k8s-router` running `--mode=k8s-router` vs `svc/atenet-router`) → Backend Pod network path.
3. **`cmd/tenantdb` ("One Database Per Customer")**: Multi-mode binary (`--mode=actor|gateway|local`) combining a single-tenant embedded database (`DurableDir` `/data/wal.jsonl` + lazy `16.0 MB` Hot RAM Buffer Pool & Materialized View) with a **Storage & Memory Tiering X-Ray Console** (`:8086` / `/dashboard`). Demonstrates split snapshot scopes (`onPause: SNAPSHOT_CONTENT_SCOPE_FULL` keeping the `16.0 MB` RAM Buffer Pool hot vs. `onCommit: SNAPSHOT_CONTENT_SCOPE_DATA` discarding the `16.0 MB` RAM heap and committing only `/data/wal.jsonl` to GCS for `99.997%` bandwidth savings + `<1ms` WAL replay on wakeup), 4 customer databases (`db-acme`, `db-globex`, `db-initech`, `db-umbrella`) multiplexing across the 3-replica `WorkerPool` with zero `WHERE tenant_id` queries, and 1-command GDPR Article 17 deletion (`kubectl ate delete actor ... --any-state`).
4. **`cmd/honeypot` ("The Truman Show Honeypot")**: Dual-mode binary (`--mode=actor|gateway|local`) with a Split-Screen "Movie Set vs. Director's Booth" Web UI (`:8084`) and per-attacker-IP deception sandbox running real `/bin/sh -c` commands inside `gVisor` (`SANDBOX_CLASS_GVISOR`, using `debian:13-slim` in `.ko.yaml`). Strictly separates attacker `/shell` output from SOC `/forensics` (`canary_alerts`, SHA-256 `rootfs_delta` tracking `UNCHANGED`/`ADDED`/`MODIFIED`/`DELETED`, `gvisor_sandbox_proof`, and `pod_ip_history`), and demonstrates auto-checkpointing (`kubectl ate save actor`) + `kubectl ate revert actor` to recover deleted malware after an attacker's `rm -rf` wipe alongside `--tag` forensic cloning.
5. **`cmd/timewarp` ("Don't Try This at Home")**: Multi-mode binary (`--mode=actor|gateway|local`) demonstrating **Wall-Clock Time-Jump Shock**, **Zombie Leader Raft Split-Brain**, and **Naive Rate-Limiter Burst Overflow** across a 3-node cluster (`raft-node-1..3`), paired with the **Chronos Split-Brain & Dual-Clock Lab** Web UI (`:8085` / `/dashboard`) and an optional **Wakeup Clock-Jump Guardrail** (`?guard=true`).

## Repository Structure

```text
cmd/
  dungeon/                      # Demo 1: Zero-DB RPG in Go heap RAM (main.go, main_test.go)
  dungeon2d/                    # Demo 1b: 2D Canvas Roguelike & Presenter Gateway (actor + gateway)
  coldstart/                    # Demo 2: 50k-doc in-memory search engine + K8s vs Substrate HPA QPS race
  tenantdb/                     # Demo 3: Per-tenant embedded DB (DurableDir + 16MB Buffer Pool + Storage X-Ray Gateway)
  honeypot/                     # Demo 4: Per-attacker gVisor sandbox + Split-Screen SOC Gateway (actor, gateway, local)
  timewarp/                     # Demo 5: 3-node Raft split-brain & Dual-Clock Time-Jump Lab (actor, gateway, local)
manifests/
  workerpool.yaml.tmpl          # K8s Namespace + 3-replica ate.dev/v1alpha1 WorkerPool
  dungeon-template.yaml.tmpl    # ActorTemplate (FULL pause, FULL commit)
  dungeon2d-template.yaml.tmpl  # ActorTemplate (2D Roguelike actor, FULL pause, FULL commit)
  dungeon2d-gateway.yaml.tmpl   # K8s Secret, RBAC, Deployment & Service for dungeon2d-gateway
  coldstart-template.yaml.tmpl  # ActorTemplate (12s 50k-doc search index warmup -> Golden Snapshot)
  coldstart-hpa.yaml.tmpl       # K8s Deployment, L7 Router, HPAs, External Metrics APIService & coldstart-gateway
  tenantdb-template.yaml.tmpl   # ActorTemplate (DurableDir /data, FULL pause, DATA commit)
  tenantdb-gateway.yaml.tmpl    # K8s RBAC, Deployment & LoadBalancer Service for tenantdb-gateway
  honeypot-template.yaml.tmpl   # ActorTemplate (FULL pause/commit, gVisor sandbox)
  honeypot-gateway.yaml.tmpl    # K8s RBAC, Deployment & LoadBalancer Service for honeypot-gateway
  timewarp-template.yaml.tmpl   # ActorTemplate (FULL pause/commit, heartbeat/lease anti-pattern)
  timewarp-gateway.yaml.tmpl    # K8s RBAC, Deployment & LoadBalancer Service for timewarp-gateway
hack/
  stage-demo.sh                 # GKE bootstrapper, ko/ate deployer, and interactive 5-act runner
docs/
  talk-outline.md               # 4-act talk outline, sequence diagram, speaker notes & litmus test
.ko.yaml                        # ko build config (distroless static-debian13, base-debian13 for gateways, debian:13-slim for kubectl-ate & honeypot)
.env.example                    # Template for local .env (GCP PROJECT_ID, region/cluster defaults)
```

## Build, Test & Run Commands

### Unit Tests (Local, No Cluster Required)

All demo binaries have unit tests under `cmd/*/*_test.go` using only the Go standard library:

```bash
go test -v ./...
```

Always run `go test ./...` after modifying any file under `cmd/`.

### Deploying & Running Demos on GKE / Agent Substrate

1. **Environment configuration**:
   Copy `.env.example` to `.env` and set `PROJECT_ID`. (`.env` is gitignored; never commit credentials or project-specific `.env` files.)
   - Default region/zone: `us-west3` / `us-west3-a` (Salt Lake City).
   - `SUBSTRATE_ROOT`: Path to the `agent-substrate` checkout (needed only for `bootstrap-gke` or building `ateom-gvisor` / `kubectl-ate` from source; auto-detected from `..` or `../../agent-substrate/*` if unset).
2. **Stage script (`hack/stage-demo.sh`)**:
   ```bash
   ./hack/stage-demo.sh bootstrap-gke         # Provision GKE (1.36+), install ate-system, and deploy all demos
   ./hack/stage-demo.sh deploy                # Deploy WorkerPool + build/register all ActorTemplates via ko
   ./hack/stage-demo.sh deploy-dungeon2d      # Deploy interactive 2D Dungeon Gateway (prints presenter auth URL)
   ./hack/stage-demo.sh cleanup-dungeon2d     # Clean up 2D Dungeon Gateway deployment/secret/actors/tags
   ./hack/stage-demo.sh deploy-coldstart-hpa  # Deploy K8s vs Substrate HPA QPS comparison stack & gateway
   ./hack/stage-demo.sh cleanup-coldstart-hpa # Clean up Cold-Start HPA comparison stack & actors
   ./hack/stage-demo.sh deploy-tenantdb       # Deploy interactive TenantDB Storage & Memory Tiering Gateway
   ./hack/stage-demo.sh cleanup-tenantdb      # Clean up TenantDB Gateway deployment/service/secret/actors
   ./hack/stage-demo.sh deploy-honeypot       # Deploy interactive Honeypot Split-Screen Gateway & LoadBalancer
   ./hack/stage-demo.sh cleanup-honeypot      # Clean up Honeypot Gateway deployment/service/actors/tags
   ./hack/stage-demo.sh deploy-timewarp       # Deploy interactive Time-Warp Split-Brain Lab Gateway & LoadBalancer
   ./hack/stage-demo.sh cleanup-timewarp      # Clean up Time-Warp Gateway deployment/service/actors
   ./hack/stage-demo.sh run-all               # Run all 5 live demos interactively
   PAUSE_BETWEEN_STEPS=0 ./hack/stage-demo.sh run-all  # Run all 5 demos non-interactively
   ./hack/stage-demo.sh act1-dungeon          # Run individual act (act1-dungeon, act1b-dungeon2d .. act5-timewarp)
   ./hack/stage-demo.sh cleanup               # Delete demo actors, tags, templates, atespace, and namespace
   ```
   Each `act*` target is idempotent and self-contained: `reset_act_resources` deletes that act's prior actors/tags before starting, and `free_workers` suspends any `RUNNING` or `PAUSED` actors before and after each act so the 3-replica `WorkerPool` is never exhausted across runs.

## Conventions & Architectural Gotchas

- **Zero External Go Dependencies**: `go.mod` targets Go `1.23.0` and uses **only the Go standard library**. Do not add third-party dependencies to `go.mod` unless explicitly requested.
- **HTTP Server & Stage Output Contract**:
  - Every demo binary listens on `--addr=:80` by default to match the container `wakeupProbe` port (`80`) in `manifests/*-template.yaml.tmpl`.
  - Every demo binary exposes `GET /readyz`. Note that Agent Substrate waits for `wakeupProbe` (`/readyz` returning `200 OK`) before taking the `ActorTemplate` Golden Snapshot.
  - Format HTTP responses as pretty-printed JSON (`enc.SetIndent("", "  ")`) so `curl` output in `hack/stage-demo.sh` is human-readable on stage without requiring `jq`.
  - Include pod IP tracking (`current_pod_ip` / `pod_ip_history`) in responses so cross-worker-pod actor migrations are visible in demo output.
- **`kubectl` vs. `kubectl ate` Manifests**:
  - `manifests/workerpool.yaml.tmpl` is a standard Kubernetes manifest (`apiVersion: ate.dev/v1alpha1`, `kind: WorkerPool`) applied via `envsubst | kubectl apply -f -`.
  - `manifests/*-template.yaml.tmpl` are **Agent Substrate API payloads** (no `apiVersion`/`kind`), built and registered via `envsubst < manifests/<name>-template.yaml.tmpl | ko resolve -f - | kubectl ate create actor-template -f -`. Do not add Kubernetes CRD headers to `*-template.yaml.tmpl` or pass them to `kubectl apply`.
- **`ko` Image References**:
  - `ActorTemplate` manifests use `image: ko://github.com/moficodes/anything-but-agents/cmd/<demo>` and `command: ["/ko-app/<demo>"]`.
- **Routing via `atenet`**:
  - Requests to actors go through `svc/atenet-router` (port-forwarded to `http://localhost:8000` by `hack/stage-demo.sh`) using the HTTP header `ate-target-actor: anything-but-agents/<actor-name>`.
- **Keep Docs & Scripts Synchronized**:
  - When adding or modifying a demo's flags, endpoints, or lifecycle steps, update `hack/stage-demo.sh`, `README.md`, and `docs/talk-outline.md` together.
- **License Headers**:
  - Include the standard `Copyright 2026 Google LLC` Apache 2.0 license header on all new `.go`, `.sh`, and `.yaml.tmpl` files.
