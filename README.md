# Running Anything *But* Agents on Agent Substrate

Companion repository (demos, manifests, stage script, and talk outline) for the talk **"Running Anything *But* Agents on Agent Substrate"**.

If you strip the word **"Agent"** out of Agent Substrate, you have a **Kubernetes-native, sub-second, memory-checkpointing, wake-on-HTTP, sandboxed actor runtime**. This repository explores five non-agent workloads—from a zero-database Zork dungeon with "save-scumming" to an intentional Raft split-brain time-warp catastrophe—all multiplexed across a single 3-replica `WorkerPool`.

---

## Repository Structure

```text
cmd/
  dungeon/     # Demo 1: "Look Ma, No Database" — Zork-style RPG in pure Go heap RAM (+ revert/tag save-scumming)
  dungeon2d/   # Demo 1B: "Save-Scumming the Heap Dragon (2D Edition)" — Mobile 2D 3-floor game + Live Mission-Control Dashboard
  coldstart/   # Demo 2: "The Instant Cloner" — 12s cold-start prime sieve skipped via Golden Snapshot
  tenantdb/    # Demo 3: "One DB Per User" — Per-tenant embedded DB with DurableDir + FULL Pause / DATA Commit
  honeypot/    # Demo 4: "The Truman Show Honeypot" — Per-attacker-IP gVisor deception sandbox
  timewarp/    # Demo 5: "Don't Try This at Home" — Wall-clock time-jump shock & Raft split-brain anti-pattern
manifests/
  workerpool.yaml.tmpl          # Shared 3-worker gVisor WorkerPool (`anything-but-agents`)
  dungeon-template.yaml.tmpl    # ActorTemplate (FULL pause, FULL commit)
  dungeon2d-template.yaml.tmpl  # ActorTemplate for 2D multilevel dungeon actors (FULL pause, FULL commit)
  dungeon2d-gateway.yaml.tmpl   # Optional K8s Deployment + LoadBalancer Service for audience phone QR access
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

## Deep Dive: Rationale Behind Each Demo

Each of the demos is designed to isolate and prove a specific architectural property of Agent Substrate—without a single AI agent in sight.

---

### Demo 1: "Look Ma, No Database" — Zork-Style Dungeon RPG (`cmd/dungeon`) & 2D Edition (`cmd/dungeon2d`)

* **Why this demo:**
  Most application code in the cloud exists for one boring reason: translating in-memory data structures into SQL rows, Redis keys, or JSON blobs before a stateless request finishes, and hydrating them back on the next request. This demo asks: *What if we deleted the database layer entirely and relied on the container runtime to checkpoint process RAM?*
* **Why this demo specifically:**
  A turn-based dungeon crawler (player HP, inventory, monsters in rooms, items dropped on the floor) has deeply nested, relational, mutable state—and gamers immediately understand the concepts of **quicksaving**, **save-scumming** (reloading after dying to a boss), and **branching timelines**.
  * **`cmd/dungeon` (`act1-dungeon`)** shows the raw mechanics via CLI `curl` calls.
  * **`cmd/dungeon2d` (`act1b-dungeon2d`)** takes the concept to the next level for a live conference audience: a **mobile-optimized 2D HTML5 Canvas 3-floor dungeon game (`/play`)** paired with a **projector Mission-Control Dashboard (`/dashboard`)**. Local 60fps movement runs in the player's browser; whenever a state milestone occurs (picking up gear, slaying a monster, descending stairs, touching a Quicksave Shrine, dying, or escaping with the Golden Chalice), the Gateway wakes the player's actor via `atenet-router`, mutates its Go heap RAM, and **immediately suspends it back to GCS**—allowing dozens of conference attendees on their phones to multiplex across just 3 `WorkerPool` pods!
* **What do we learn:**
  1. Process memory (`SNAPSHOT_CONTENT_SCOPE_FULL`) is durable across node migrations: when an actor suspends on Worker Pod A and resumes on Worker Pod B, its `pod_ip_history` slice grows while every local variable on the Go heap remains intact.
  2. Infrastructure-level lifecycle operations (`suspend`, `revert`, `tag`) become application-level features (save points, undo, and multiverse forking) with zero application code.
* **Why is Substrate here:**
  Plain Kubernetes Pods lose all in-memory state when evicted, restarted, or scaled down, and keeping one idle Pod per player for 6 months while they wait to make their next turn would waste enormous cluster capacity.
* **What would be other ways to solve this:**
  * *Stateless API + Postgres/Redis:* Requires schema migrations, ORM/serialization code, transaction locking for room/inventory state, and manually implementing snapshot/undo tables if you want "save points."
  * *StatefulSets / Sticky Game Servers:* High idle compute cost; cannot scale to zero per player or per dungeon room without writing custom disk serialization.
  * *Durable Execution Engines (Temporal / Cloudflare Durable Objects):* Require rewriting your application in a deterministic workflow DSL or proprietary SDK. Here, it is just an ordinary Go `net/http` binary.
* **Why is this cool:**
  You walk into the Dragon's Lair without a fire-shield, quicksave via `kubectl ate suspend actor`, attack the Dragon, **die (`PlayerHP = 0`)**, and then run `kubectl ate revert actor`—literally `git reset --hard` on live process memory—to come back to life before the fatal turn. Then you fork a second timeline (`kubectl ate create actor ... --tag before-dragon`) so two players can play out alternate histories from the exact same heap snapshot.
* **How does Substrate fit into this:**
  * **`SNAPSHOT_CONTENT_SCOPE_FULL`** (`onPause` and `onCommit`) captures the entire Go heap and goroutine stacks to GCS/S3.
  * **`atenet` (`ate-target-actor: anything-but-agents/hero-mofi`)** intercepts HTTP requests, wakes the suspended actor on any free worker pod in `~100ms`, and forwards the request.
  * **`kubectl ate revert actor`** discards the mutated (dead!) in-memory state and resets the actor pointer back to its last committed snapshot.
  * **`kubectl ate create tag` + `kubectl ate create actor --tag`** clones a suspended actor's snapshot into a brand-new actor identity.

---

### Demo 2: "The Instant Cloner" — Taming a 12-Second Cold-Start Monster (`cmd/coldstart`)

* **Why this demo:**
  Many workloads are terrible candidates for scale-to-zero serverless because their initialization phase takes 10 to 90 seconds—think legacy Enterprise Java/Spring Boot monoliths, language servers (`rust-analyzer`, `gopls`) indexing a monorepo AST, or Python scientific/financial models loading gigabytes of lookup tables into RAM.
* **Why this demo specifically:**
  Instead of bringing a bloated 4GB enterprise dependency onto stage, `cmd/coldstart` deterministically simulates a 12-second heavy startup (`--init-delay=12s`), computes all `78,498` prime numbers up to `1,000,000` in RAM using a Sieve of Eratosthenes, and stamps a cryptographic random `golden_heap_fingerprint` into memory right before flipping `/readyz` from `503` to `200 OK`.
* **What do we learn:**
  `ActorTemplate` creation is not just registering YAML—it is a **cluster-wide `fork()` primitive**. Any work completed *before* the `wakeupProbe` (`/readyz`) returns `200 OK` is baked into the **Golden Snapshot** once and amortized across thousands of future actor instances.
* **Why is Substrate here:**
  In Kubernetes or Knative, scaling from 0 to 10 replicas means running container startup and `main()` initialization 10 separate times (paying the 12-second penalty on every cold start).
* **What would be other ways to solve this:**
  * *Overprovisioned warm pools (minReplicas > N):* You pay for idle RAM/CPU 24/7 just to hide startup latency, and each pod is already bound to a single tenant or shared across tenants.
  * *JVM CRaC / Custom CRI Checkpoint Images:* Requires bespoke build pipelines, custom node configurations, and manual snapshot distribution, without an actor router to hydrate instances on demand.
* **Why is this cool:**
  On stage, you instantiate `clone-1`, `clone-2`, and `clone-3` and immediately `curl` them. Instead of waiting 12 seconds each, all three respond in `~100ms`, all three report `cold_boot_duration_ms: 12000`, and all three return the **exact same `golden_heap_fingerprint`**—proving they are copy-on-write memory clones of the same post-initialization process—while each clone independently tracks its own post-fork mutations!
* **How does Substrate fit into this:**
  * **`wakeupProbe` (`/readyz`)** gates when Substrate captures the `ActorTemplate` **Golden Snapshot**.
  * **Golden Snapshot (`Full` scope)** freezes the post-init heap in GCS/S3.
  * **`kubectl ate create actor --template coldstart`** points each new actor at the Golden Snapshot, skipping cold boot entirely on first `Resume`.

---

### Demo 3: "One Database Process Per Customer" — Per-Tenant Embedded DB (`cmd/tenantdb`)

* **Why this demo:**
  Multi-tenant SaaS databases are a constant source of engineering pain: a single runaway query from Tenant A degrades Tenant B (noisy neighbors), a missing `WHERE tenant_id = ?` clause causes a catastrophic data leak, and GDPR "Right to be Forgotten" deletion requires delicately purging foreign-key hierarchies across 50 shared tables. What if every customer literally had their own isolated database process (like SQLite, DuckDB, or an embedded KV store)?
* **Why this demo specifically:**
  `cmd/tenantdb` implements a lightweight embedded database with both an **on-disk Write-Ahead Log (`/data/wal.jsonl`)** on a `DurableDir` volume and an **in-memory buffer pool + session query counter** in RAM. Having both surfaces visible in the JSON output lets us visually prove the difference between `Pause` (`FULL` snapshot scope) and `Suspend` (`DATA` snapshot scope).
* **What do we learn:**
  You don't always want to upload a 4GB RAM heap to object storage when an actor goes to sleep for the weekend! Substrate separates **short-term node-local checkpointing (`onPause`)** from **long-term object-store persistence (`onCommit`)**:
  * When `db-acme` is **Paused** (`onPause: SNAPSHOT_CONTENT_SCOPE_FULL`), both its `/data` WAL and its hot in-memory counters (`ram_queries_since_boot`) stay frozen on the node for instant hot resume.
  * When `db-acme` is **Suspended** (`onCommit: SNAPSHOT_CONTENT_SCOPE_DATA`), Substrate discards the process memory and uploads *only* the compact `DurableDir` (`/data/wal.jsonl`) to GCS. On the next request, the binary cold-boots in milliseconds (`ram_queries_since_boot` resets to `1`) and replays `/data/wal.jsonl` (`wal_replayed_on_boot > 0`)!
* **Why is Substrate here:**
  Kubernetes `PersistentVolumeClaims` (PVCs) are designed for tens of volumes per node with slow CSI attach/detach cycles (seconds to minutes)—not millions of micro-volumes attaching and detaching in 100ms as individual customers make sporadic API calls.
* **What would be other ways to solve this:**
  * *Shared Multi-Tenant RDS/Cloud SQL with Row-Level Security (RLS):* Always-on cost, noisy neighbors, complex per-tenant point-in-time recovery, and risky GDPR deletes.
  * *Turso / libSQL / LiteFS / Neon:* Purpose-built serverless database platforms use similar storage-compute separation under the hood—Substrate gives you that exact architectural primitive for *any* arbitrary binary or embedded engine (`DuckDB`, `SQLite`, `RocksDB`, `LanceDB`) on your own Kubernetes cluster.
* **Why is this cool:**
  1. You see `Pause` vs `Suspend` (`FULL` vs `DATA` scope) in action in two `curl` commands.
  2. **1-Command GDPR Compliance:** Deleting a customer's entire compute environment, memory state, and durable storage is literally `kubectl ate delete actor db-acme -a anything-but-agents --any-state`.
* **How does Substrate fit into this:**
  * **`DurableDir` volume (`volumes: [{name: data, durableDir: {}}]`)** provides a snapshot-backed per-actor filesystem directory (`/data`).
  * **Split `snapshotConfig` (`onPause: FULL`, `onCommit: DATA`)** gives hot-RAM latency for bursty sessions and cheap data-only storage for long-term hibernation.

---

### Demo 4: "The Truman Show Honeypot" — On-Demand Per-Attacker Deception Grid (`cmd/honeypot`)

* **Why this demo:**
  High-interaction security honeypots (or per-student CTF boxes, or per-PR preview environments) need strong sandbox isolation because untrusted users are actively trying to break them or modify their filesystems. Running thousands of idle VMs waiting for a scanner to hit them is prohibitively expensive, while stateless low-interaction honeypots are immediately fingerprinted as fake because dropped files vanish on reconnect.
* **Why this demo specifically:**
  `cmd/honeypot` poses as a "Legacy Corp Payroll Admin Console" running inside a **`gVisor` (`SANDBOX_CLASS_GVISOR`)** sandbox with a pre-seeded decoy credential file (`payroll_db_credentials.env.bak`). When an attacker (`honeypot-203-0-113-5`) drops `/tmp/honeypot/rootkit.sh` into the container rootfs and runs recon commands, both the modified rootfs overlay and the in-memory forensic audit log are captured.
* **What do we learn:**
  `SNAPSHOT_CONTENT_SCOPE_FULL` doesn't just capture process RAM—it also captures the **container rootfs delta** on top of the base OCI image, while `gVisor` isolates the kernel syscall surface.
* **Why is Substrate here:**
  It combines **hard sandbox isolation (`gVisor`/`microVM`)**, **zero idle compute cost**, and **per-attacker rootfs + memory continuity** routed dynamically via an HTTP header.
* **What would be other ways to solve this:**
  * *Dedicated Honeypot VMs:* Expensive to run at high cardinality; requires manual cleanup/reset scripts once compromised.
  * *Stateless Honeypot Emulators (Cowrie, etc.):* Easily detected by attackers who test whether processes/files persist across sessions.
* **Why is this cool:**
  Attacker A (`203.0.113.5`) drops `rootkit.sh` in `/tmp` and disconnects; their actor suspends to GCS ($0 compute). Attacker B (`198.51.100.9`) connects 10 seconds later and sees a pristine filesystem cloned from the Golden Snapshot. Weeks later, Attacker A returns—and in `~100ms`, their personal "Truman Show" movie set wakes back up on a completely different worker node with `rootkit.sh` still sitting in `/tmp`!
* **How does Substrate fit into this:**
  * **`sandboxConfig: {sandboxClass: SANDBOX_CLASS_GVISOR}`** sandboxes untrusted execution.
  * **Rootfs delta checkpointing (`SNAPSHOT_CONTENT_SCOPE_FULL`)** preserves `/tmp/honeypot/*` edits per actor without affecting the Golden Snapshot or other actors.

---

### Demo 5: "Don't Try This at Home" — Wall-Clock Time-Jump Shock & Raft Split-Brain (`cmd/timewarp`)

* **Why this demo:**
  The best way to truly understand a new infrastructure primitive is to see **where the wheels fall off**. If you tell engineers "Substrate can checkpoint and restore any container," someone will inevitably try to run a distributed consensus cluster (`etcd`/`Raft`), a Kafka broker, or a wall-clock-sensitive heartbeat daemon on it.
* **Why this demo specifically:**
  `cmd/timewarp` simulates a naive cluster node that holds an in-memory leader lease (`InMemoryRaftRole = "LEADER (Term 1)"`, `LeaseTTL = 3s`), runs a `100ms` background heartbeat ticker, and refills a naive rate-limiter token bucket using `elapsed.Seconds() * 100`.
* **What do we learn:**
  **Checkpointing freezes process memory, not the universe's wall clock.**
  1. While an actor is suspended in GCS for 5 seconds, it executes **zero CPU instructions**—so its heap variables still believe it is the Term 1 Leader.
  2. The moment it resumes, `time.Since(lastHeartbeat)` jumps from `100ms` to `5,000+ ms` in a single tick.
  3. Any workload that relies on continuous wall-clock progression, quorum heartbeats, or long-lived non-HTTP sockets will experience **Time-Travel Shock** and **Split-Brain**.
* **Why is Substrate here:**
  Here, Substrate is the **villain**! We use `kubectl ate suspend actor` to freeze a "Raft Leader" for 5 seconds (longer than its 3-second leader lease) and then wake it via `atenet` to watch its assumptions implode.
* **What would be other ways to solve this:**
  * For consensus systems (`etcd`, `Raft`, `ZooKeeper`), continuous streaming brokers (`Kafka`), or 60Hz real-time game loops: **use Kubernetes `StatefulSets` or `Deployments`!** They are never idle (`1:1` multiplexing ratio) and require continuous wall-clock participation.
  * For workloads that *do* belong on Substrate: avoid unguarded `time.Since(lastTick)` multipliers, re-validate external leases/tokens on wakeup, and treat wall-clock discontinuities as normal lifecycle events.
* **Why is this cool:**
  On stage, one `curl` before suspend returns `"status": "HEALTHY"`, and the very next `curl` after a 5-second suspend returns `"status": "PANIC: TIME-TRAVEL SHOCK & RAFT SPLIT-BRAIN DETECTED AFTER RESUME!"`—showing a `5,000ms` gap in a `100ms` ticker, an expired leader lease that still thinks it's in charge, and a token bucket that just minted 500 burst tokens at once.
* **How does Substrate fit into this:**
  * Demonstrates the architectural boundary of **process freeze (`Suspend`/`Resume`)** and sets up the closing **"Secretly Agent-Shaped" Litmus Test** of the talk.

---

## Summary: When Should You Run a Non-Agent Workload on Substrate?

| Workload Property | Good Fit for Substrate? | Why |
|---|---|---|
| **High cardinality, low duty cycle** (idle >90% of the time) | **Yes** | Multiplexes thousands of actors onto a tiny `WorkerPool` + [Request Parking](../docs/request-parking.md) |
| **Heavy cold-start initialization** (JVM, LSPs, ML/quant models) | **Yes** | `ActorTemplate` Golden Snapshot runs init once before `/readyz` |
| **Complex in-memory state** (games, simulations, REPLs, notebooks) | **Yes** | `FULL` snapshots turn process RAM into durable storage + `revert`/`tag` |
| **Single-tenant data isolation** (one SQLite/DuckDB per customer) | **Yes** | `DurableDir` + `FULL` Pause / `DATA` Commit + 1-command GDPR delete |
| **Untrusted / hostile per-session code** (honeypots, PR previews) | **Yes** | Per-actor `gVisor` / `microVM` sandboxes with rootfs delta persistence |
| **Distributed consensus / quorum voters** (Raft, etcd, Paxos) | **No** | Suspending voters triggers elections; resuming causes split-brain |
| **100% duty-cycle streaming / 60Hz real-time loops** | **No** | `1:1` worker pinning + high snapshot churn bandwidth cost |
| **Stateless fast-boot CRUD microservices** | **No** | Standard Kubernetes `Deployment` + `Service` is simpler and faster |

---

## Quickstart (GKE in `us-west3` Salt Lake City)

### 1. Configure `.env`

Copy [`.env.example`](.env.example) to `.env` and set your GCP `PROJECT_ID`. All other settings default automatically (`GCE_REGION=us-west3` for Salt Lake City, `CLUSTER_LOCATION=us-west3-a`, `CLUSTER_NAME=anything-but-agents`, `BUCKET_NAME`, and `KO_DOCKER_REPO`), and any of them can be overridden in `.env`:

```bash
cp .env.example .env
# Edit .env and set: export PROJECT_ID="your-gcp-project-id"
```

### 2. Provision GKE Cluster, Install Substrate & Deploy All 5 Demos

If you need a fresh GKE cluster with Agent Substrate (`ate-system`) and all 5 demos deployed:

```bash
./hack/stage-demo.sh bootstrap-gke
```

*(If you already have an Agent Substrate cluster running, run `./hack/stage-demo.sh deploy` instead.)*

### 3. Run the Live Stage Demos

`hack/stage-demo.sh` automatically port-forwards `svc/atenet-router` on `localhost:8000` if needed. Each step pauses for `<Enter>` so you can narrate on stage (set `PAUSE_BETWEEN_STEPS=0` to run non-interactively):

```bash
./hack/stage-demo.sh run-all           # Run all 5 demos in sequence (or run them individually below)

./hack/stage-demo.sh act1-dungeon      # Act I: Zero-DB Dungeon + `kubectl ate revert` save-scumming
./hack/stage-demo.sh act1b-dungeon2d   # Act I-B: 2D Multilevel Dungeon (/play) + Live Dashboard (/dashboard)
./hack/stage-demo.sh deploy-dungeon2d  # Deploy in-cluster Dungeon 2D Gateway + LoadBalancer + PRESENTER_TOKEN Secret
./hack/stage-demo.sh cleanup-dungeon2d # Clean up in-cluster Dungeon 2D Gateway & d2d-* actors
./hack/stage-demo.sh act2-coldstart    # Act II: 12s Cold-Start Monster -> 100ms Golden Snapshot clones
./hack/stage-demo.sh act3-tenantdb     # Act III: Per-Tenant DB (DurableDir + FULL Pause vs DATA Commit)
./hack/stage-demo.sh act4-honeypot     # Act IV: Per-Attacker-IP gVisor Honeypot
./hack/stage-demo.sh act5-timewarp     # Act V: Wall-Clock Time-Jump Shock & Raft Split-Brain
```

### 4. Cleanup

```bash
./hack/stage-demo.sh cleanup
```
