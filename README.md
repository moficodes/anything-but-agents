# Running Anything *But* Agents on Agent Substrate

Companion repository (demos, manifests, stage script, and talk outline) for the talk **"Running Anything *But* Agents on Agent Substrate"**.

If you strip the word **"Agent"** out of Agent Substrate, you have a **Kubernetes-native, sub-second, memory-checkpointing, wake-on-HTTP, sandboxed actor runtime**. This repository explores five non-agent workloads—from a zero-database Zork dungeon with "save-scumming" to an intentional Raft split-brain time-warp catastrophe—all multiplexed across a single 3-replica `WorkerPool`.

---

## Repository Structure

```text
cmd/
  dungeon/     # Demo 1: "Look Ma, No Database" — Zork-style RPG in pure Go heap RAM (+ revert/tag save-scumming)
  coldstart/   # Demo 2: "The Instant Cloner" — 12s cold-start prime sieve skipped via Golden Snapshot
  tenantdb/    # Demo 3: "One DB Per User" — Per-tenant embedded DB with DurableDir + FULL Pause / DATA Commit
  honeypot/    # Demo 4: "The Truman Show Honeypot" — Per-attacker-IP gVisor deception sandbox
  timewarp/    # Demo 5: "Don't Try This at Home" — Wall-clock time-jump shock & Raft split-brain anti-pattern
manifests/
  workerpool.yaml.tmpl          # Shared 3-worker gVisor WorkerPool (`anything-but-agents`)
  dungeon-template.yaml.tmpl    # ActorTemplate (FULL pause, FULL commit)
  coldstart-template.yaml.tmpl  # ActorTemplate (12s warmup before /readyz -> Golden Snapshot)
  tenantdb-template.yaml.tmpl   # ActorTemplate (DurableDir /data, FULL pause, DATA commit)
  honeypot-template.yaml.tmpl   # ActorTemplate (gVisor rootfs + RAM forensic trail)
  timewarp-template.yaml.tmpl   # ActorTemplate (heartbeat + lease loop for split-brain demo)
hack/
  stage-demo.sh                 # Interactive live-stage deployer & act-by-act demo runner
docs/
  talk-outline.md               # Full 4-act slide-by-slide outline, speaker notes & decision matrix
```

---

## Quickstart

### 1. Run Unit Tests Locally

All five demo workloads are pure standard-library Go and can be tested without a cluster:

```bash
go test -v ./...
```

### 2. Deploy to an Agent Substrate Cluster

Ensure your cluster has Agent Substrate (`ate-system`) installed, `kubectl-ate` on your `PATH`, and `atenet-router` port-forwarded (`kubectl port-forward -n ate-system svc/atenet-router 8000:80`):

```bash
export BUCKET_NAME="<your-snapshot-gcs-bucket>"
./hack/stage-demo.sh deploy
```

### 3. Run the Live Stage Demos

Each act pauses before executing each command so you can narrate on stage (set `PAUSE_BETWEEN_STEPS=0` to run non-interactively):

```bash
./hack/stage-demo.sh act1-dungeon    # Act I: Zero-DB Dungeon + `kubectl ate revert` save-scumming
./hack/stage-demo.sh act2-coldstart  # Act II: 12s Cold-Start Monster -> 100ms Golden Snapshot clones
./hack/stage-demo.sh act3-tenantdb   # Act III: Per-Tenant DB (DurableDir + FULL Pause vs DATA Commit)
./hack/stage-demo.sh act4-honeypot   # Act IV: Per-Attacker-IP gVisor Honeypot
./hack/stage-demo.sh act5-timewarp   # Act V: Wall-Clock Time-Jump Shock & Raft Split-Brain
```

### 4. Cleanup

```bash
./hack/stage-demo.sh cleanup
```
