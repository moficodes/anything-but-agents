# AGENTS.md

Guidance for AI coding agents working in `anything-but-agents`.

## Project Overview

This is the companion repository (Go demo services, Substrate manifests, live-stage runner, and talk outline) for the talk **"Running Anything *But* Agents on Agent Substrate"**.

Without AI/LLM frameworks, **Agent Substrate (`ate`)** is a **Kubernetes-native, sub-second memory-checkpointing, wake-on-HTTP, gVisor-sandboxed actor runtime**. This project demonstrates its architectural primitives—and its failure modes—by multiplexing five non-agent workloads across a single 3-replica `WorkerPool`:

1. **`cmd/dungeon` ("Look Ma, No Database") & `cmd/dungeon2d` ("Save-Scumming the Heap Dragon — 2D Edition")**:
   - `cmd/dungeon`: Zork-style RPG where 100% of state (`GameState`: HP, inventory, room items, monster HP, `pod_ip_history`) lives in Go heap RAM with zero database or disk I/O. Demonstrates `SNAPSHOT_CONTENT_SCOPE_FULL` across pod migrations, save-scumming via `kubectl ate revert actor`, and timeline branching via `kubectl ate create tag` + `--tag`.
   - `cmd/dungeon2d`: Takes the zero-DB game concept to the next level with a mobile-optimized 2D 3-floor HTML5 Canvas game (`/play`) and a live projector Mission-Control Dashboard (`/dashboard`) using inline Lucide SVG icons. Wakes the player's actor via `atenet-router` only on state milestones, mutates Go heap RAM, and immediately suspends back to GCS so dozens of players multiplex across the 3-replica `WorkerPool`.
2. **`cmd/coldstart` ("The Instant Cloner")**: Simulates a 12-second heavy initialization (`--init-delay=12s` and Sieve of Eratosthenes up to `1,000,000`) before `/readyz` flips from `503` to `200 OK`. Demonstrates how `ActorTemplate` `wakeupProbe` captures a post-init **Golden Snapshot** (`golden_heap_fingerprint`) so new actors clone in ~100ms.
3. **`cmd/tenantdb` ("One Database Per Customer")**: Single-tenant embedded key-value store backed by a `DurableDir` volume (`/data/wal.jsonl`) plus an in-memory buffer pool and query counter. Demonstrates split snapshot scopes (`onPause: SNAPSHOT_CONTENT_SCOPE_FULL` for hot local resume vs. `onCommit: SNAPSHOT_CONTENT_SCOPE_DATA` for compact GCS storage + WAL replay on boot) and 1-command GDPR deletion (`kubectl ate delete actor ... --any-state`).
4. **`cmd/honeypot` ("The Truman Show Honeypot")**: Per-attacker-IP deception sandbox running in `gVisor` (`SANDBOX_CLASS_GVISOR`). Uses `SNAPSHOT_CONTENT_SCOPE_FULL` to persist both container rootfs overlay modifications (`/tmp/honeypot/*`) and in-memory forensic audit logs across suspends at $0 idle compute cost.
5. **`cmd/timewarp` ("Don't Try This at Home")**: Intentional anti-pattern demonstrating **Wall-Clock Time-Jump Shock** and **Raft Split-Brain**. Runs a 100ms heartbeat ticker, a 3s in-memory leader lease, and a naive `time.Since` rate-limiter token bucket; suspending the actor for 5s and resuming causes an immediate split-brain panic and token burst overflow.

## Repository Structure

```text
cmd/
  dungeon/                      # Demo 1: Zero-DB RPG in Go heap RAM (main.go, main_test.go)
  dungeon2d/                    # Demo 1B: 2D 3-floor mobile game (/play) + Live Dashboard (/dashboard)
  coldstart/                    # Demo 2: 12s cold-start prime sieve -> Golden Snapshot clone
  tenantdb/                     # Demo 3: Per-tenant embedded DB with DurableDir (/data/wal.jsonl)
  honeypot/                     # Demo 4: Per-attacker gVisor sandbox with rootfs delta persistence
  timewarp/                     # Demo 5: Wall-clock time-jump & Raft split-brain anti-pattern
manifests/
  workerpool.yaml.tmpl          # K8s Namespace + 3-replica ate.dev/v1alpha1 WorkerPool
  dungeon-template.yaml.tmpl    # ActorTemplate (FULL pause, FULL commit)
  dungeon2d-template.yaml.tmpl  # ActorTemplate for 2D multilevel dungeon actors
  dungeon2d-gateway.yaml.tmpl   # Optional K8s Deployment + LoadBalancer Service for audience phones
  coldstart-template.yaml.tmpl  # ActorTemplate (12s warmup before /readyz -> Golden Snapshot)
  tenantdb-template.yaml.tmpl   # ActorTemplate (DurableDir /data, FULL pause, DATA commit)
  honeypot-template.yaml.tmpl   # ActorTemplate (FULL pause/commit, gVisor sandbox)
  timewarp-template.yaml.tmpl   # ActorTemplate (FULL pause/commit, heartbeat/lease anti-pattern)
hack/
  stage-demo.sh                 # GKE bootstrapper, ko/ate deployer, and interactive 5-act runner
docs/
  talk-outline.md               # 4-act talk outline, sequence diagram, speaker notes & litmus test
.ko.yaml                        # ko build config (distroless static-debian13, linux/amd64 & arm64)
.env.example                    # Template for local .env (GCP PROJECT_ID, region/cluster defaults)
```

## Build, Test & Run Commands

### Unit Tests (Local, No Cluster Required)

All five demo binaries have unit tests under `cmd/*/*_test.go` using only the Go standard library:

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
   ./hack/stage-demo.sh bootstrap-gke   # Provision GKE (1.36+), install ate-system, and deploy all 5 demos
   ./hack/stage-demo.sh deploy          # Deploy WorkerPool + build/register all 5 ActorTemplates via ko
   ./hack/stage-demo.sh run-all         # Run all 5 live demos interactively
   PAUSE_BETWEEN_STEPS=0 ./hack/stage-demo.sh run-all  # Run all 5 demos non-interactively
   ./hack/stage-demo.sh act1-dungeon    # Run individual act (act1-dungeon .. act5-timewarp) in any order
   ./hack/stage-demo.sh cleanup         # Delete demo actors, tags, templates, atespace, and namespace
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
