# Running Anything *But* Agents on Agent Substrate

Companion repository (demos, manifests, stage script, and talk outline) for the talk **"Running Anything *But* Agents on Agent Substrate"**.

If you strip the word **"Agent"** out of Agent Substrate, you have a **Kubernetes-native, sub-second, memory-checkpointing, wake-on-HTTP, sandboxed actor runtime**. This repository explores five non-agent workloads—from a zero-database Zork dungeon with "save-scumming" to an intentional Raft split-brain time-warp catastrophe—all multiplexed across a single 3-replica `WorkerPool`.

---

## Repository Structure

```text
cmd/
  dungeon/     # Demo 1: "Look Ma, No Database" — Zork-style RPG in pure Go heap RAM (+ revert/tag save-scumming)
  dungeon2d/   # Demo 1B: "Save-Scumming the Heap Dragon (2D Edition)" — PixiJS v8 8-floor action roguelike + Live Mission-Control Dashboard
  coldstart/   # Demo 2: "The Instant Cloner & Production HPA QPS Race" — 50k-doc in-memory search engine + K8s vs Substrate HPA QPS race
  tenantdb/    # Demo 3: "One DB Per Customer" — Per-tenant embedded DB (DurableDir + 16MB Buffer Pool) + Storage X-Ray Console
  honeypot/    # Demo 4: "The Truman Show Honeypot" — Per-attacker-IP gVisor sandbox + Split-Screen SOC Director's Booth UI
  timewarp/    # Demo 5: "Don't Try This at Home" — Wall-clock time-jump shock & Raft split-brain anti-pattern
manifests/
  workerpool.yaml.tmpl          # Shared 3-worker gVisor WorkerPool (`anything-but-agents`)
  dungeon-template.yaml.tmpl    # ActorTemplate (FULL pause, FULL commit)
  dungeon2d-template.yaml.tmpl  # ActorTemplate for 2D multilevel dungeon actors (FULL pause, FULL commit)
  dungeon2d-gateway.yaml.tmpl   # Optional K8s Deployment + LoadBalancer Service for audience phone QR access
  coldstart-template.yaml.tmpl  # ActorTemplate (12s 50k-doc search index warmup -> Golden Snapshot)
  coldstart-hpa.yaml.tmpl       # K8s Deployment, L7 Router, HPAs, External Metrics APIService & coldstart-gateway
  tenantdb-template.yaml.tmpl   # ActorTemplate (DurableDir /data, FULL pause, DATA commit)
  tenantdb-gateway.yaml.tmpl    # Optional K8s Deployment + LoadBalancer Service for Storage & Memory Tiering X-Ray UI
  honeypot-template.yaml.tmpl   # ActorTemplate (gVisor rootfs + RAM forensic trail)
  honeypot-gateway.yaml.tmpl    # Optional K8s Deployment + LoadBalancer Service for Split-Screen Honeypot UI
  timewarp-template.yaml.tmpl   # ActorTemplate (heartbeat + lease loop for split-brain demo)
  timewarp-gateway.yaml.tmpl    # Optional K8s Deployment + LoadBalancer Service for Chronos Split-Brain Lab UI
  gateway-tls.yaml.tmpl         # GKE Gateway API + Certificate Manager TLS + HTTPRoutes + HealthCheckPolicies
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
  * **`cmd/dungeon2d` (`act1b-dungeon2d`)** takes the concept to the next level for a live conference audience: a **mobile-optimized PixiJS v8 / HTML5 Canvas 8-floor action roguelike (`/play`)** paired with a **projector Mission-Control Dashboard (`/dashboard`)**. All 60fps real-time simulation (asymmetric Paladin/Ranger/Wizard timed combat, A* enemy pathfinding, line-of-sight aggro, levers/gates, mutually-exclusive Floor 6 relic vaults, and multi-phase Floor 8 Heap Dragon boss mechanics) runs 100% in the player's browser with zero network calls during gameplay. Only when a player explicitly triggers a **Save** (top-right Save button or in-game Save Shrine), **Revert** (`v1..vN`), **Timeline Fork** (`--tag`), or **Floor 8 Victory** does the Gateway wake the player's actor via `atenet-router`, persist the full multi-floor `WorldState` (hero stats, live enemy positions/HP, items, and vault states) in Go heap RAM, and **immediately suspend it back to GCS**—allowing dozens of conference attendees on their phones to multiplex across just 3 `WorkerPool` pods!
* **What do we learn:**
  1. Process memory (`SNAPSHOT_FIDELITY_MEMORY`) is durable across node migrations: when an actor suspends on Worker Pod A and resumes on Worker Pod B, its `pod_ip_history` slice grows while every local variable on the Go heap remains intact.
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
  * **`preferredFidelity: SNAPSHOT_FIDELITY_MEMORY`** captures the entire Go heap and goroutine stacks to GCS/S3.
  * **`atenet` (`ate-target-actor: anything-but-agents/hero-mofi`)** intercepts HTTP requests, wakes the suspended actor on any free worker pod in `~100ms`, and forwards the request.
  * **`kubectl ate revert actor`** discards the mutated (dead!) in-memory state and resets the actor pointer back to its last committed snapshot.
  * **`kubectl ate create tag` + `kubectl ate create actor --tag`** clones a suspended actor's snapshot into a brand-new actor identity.

---

### Demo 2: "The Instant Cloner & Production HPA QPS Race" — 12-Second Cold-Start Index vs. Golden Snapshot HPA (`cmd/coldstart`)

* **Why this demo:**
  Cold-start latency isn't just a first-request annoyance—in production Kubernetes behind a Load Balancer and HorizontalPodAutoscaler (HPA), slow cold starts directly cap your service's **effective QPS (goodput)** during traffic bursts. When HPA scales a service from `1 -> 3` replicas, newly scheduled Pods sit at `Ready: 0/1` (`503 Service Unavailable`) while building their in-memory index, leaving existing Pods to drown and shed overflow requests (`429 Too Many Requests`).
* **Why this demo specifically:**
  `cmd/coldstart` runs a realistic **50,000-document In-Memory Package & Code Search Engine** (modeled after `pkg.go.dev` / `Zoekt`). Across 6 observable startup stages (`--init-delay=12s`), it builds an inverted word-level posting list (`51,227` tokens), an inverted 3-gram (trigram) substring index (`2,025` trigrams), BM25 IDF weights, and a `1,000,000` prime lookup table in Go heap RAM (~76.6 MB post-GC `HeapAlloc`) before flipping `/readyz` from `503` to `200 OK`. Every `GET /search?q=...` request executes a real BM25 + 3-gram posting-list intersection (`~53,400` posting intersections across `~17,700` candidate docs in `~15ms` of CPU/RAM work) inside a strict **`20 QPS` per-replica concurrency ceiling** (`--max-concurrent=4`, `--min-query-duration=200ms`).
* **What do we learn:**
  1. `ActorTemplate` creation is a **cluster-wide `fork()` primitive**: the 12-second, 76.6 MB index build runs **once** before `wakeupProbe` (`/readyz`) returns `200 OK` and is frozen into the **Golden Snapshot**.
  2. When scaling out from `minReplicas: 1` to `maxReplicas: 3` on a `60 QPS` traffic spike using the exact same Load Balancer QPS metric (`coldstart_lb_qps`, `AverageValue: 20`):
     * **Standard Kubernetes HPA (`Deployment/coldstart-k8s`):** Pods 2 and 3 spend 12+ seconds cold-booting their 50,000-doc search index, pinning cluster goodput at **`20 QPS`** and shedding **`40 QPS` (67% of traffic)** throughout the cold-start window.
     * **Agent Substrate HPA (`coldstart-1..3` on `WorkerPool`):** Actors 2 and 3 restore from the Golden Snapshot in **~100ms** (with the same `golden_heap_fingerprint` and 76.6 MB search index already in RAM) and immediately serve **`60 QPS`**—delivering **~2x more total successful queries** during the burst.
* **How does Substrate fit into this:**
  * **`wakeupProbe` (`/readyz`)** gates when Substrate captures the `ActorTemplate` **Golden Snapshot**.
  * **`cmd/coldstart --mode=gateway|k8s-router|local`** serves a Kubernetes Secret token-gated **Hardware Telemetry & Oscilloscope Rack Dashboard (`/dashboard?key=...`)**, implements the Kubernetes `external.metrics.k8s.io/v1beta1` API (`coldstart_lb_qps{backend="k8s|substrate"}`) for two symmetrical `autoscaling/v2` `HorizontalPodAutoscaler` resources (`manifests/coldstart-hpa.yaml.tmpl`), and places both backends on an identical 2-hop `Service` → L7 Router (`svc/coldstart-k8s-router` watching `Endpoints/coldstart-k8s` vs `svc/atenet-router`) → Backend Pod network path.
  * **Hardware Telemetry Dashboard & Presenter Auth (`cmd/coldstart/web/dashboard.html`):**
    * **6-Stage Index Warmup LED Matrix:** Visualizes Pods 2 & 3 stepping through `Stage 1/6..6/6` (~12s cold boot) on Standard K8s vs. `PRE-BAKED AT BUILD` (~100ms Golden Snapshot restore) on Substrate.
    * **HiDPI Goodput & Load-Shedding Oscilloscope:** Plots real-time `200 OK` Served QPS against `429` Shed-Zone crosshatch and `20 QPS/pod` ceiling markers, plus a live **Dual-Backend Search Probe** (`/api/probe?q=...`), **Golden Heap Fingerprint Proof Strip** (`golden_heap_fingerprint`, `index_digest`, ~76.6 MB `HeapAlloc`), and a top-right icon-only Moon/Sun **Projector Light Mode** toggle.
    * **Presenter Token Security (`Secret/coldstart-gateway`):** `/dashboard` and `/api/*` require `PRESENTER_TOKEN` (matching `cmd/dungeon2d`). Unlock in the browser via `/dashboard?key=<PRESENTER_TOKEN>` (which sets an `HttpOnly`, `SameSite=Strict` cookie `cs_presenter` and `303`-redirects to `/dashboard` to strip the key from the URL bar) or pass `-H "X-Presenter-Key: <PRESENTER_TOKEN>"` from CLI scripts.

---

### Demo 3: "One Database Process Per Customer" — Per-Tenant Embedded DB (`cmd/tenantdb`)

* **Why this demo:**
  Multi-tenant SaaS databases are a constant source of engineering pain: a single runaway query from Tenant A degrades Tenant B (noisy neighbors), a missing `WHERE tenant_id = ?` clause causes a catastrophic data leak, and GDPR "Right to be Forgotten" deletion requires delicately purging foreign-key hierarchies across 50 shared tables. What if every customer literally had their own isolated database process (like SQLite, DuckDB, or an embedded KV store)?
* **Why this demo specifically:**
  `cmd/tenantdb` is a multi-mode binary (`--mode=actor|gateway|local`) combining a **Two-Tier Per-Tenant Embedded Database** with a **Storage & Memory Tiering X-Ray Console** (`:8086` / `/dashboard`):
  * **Tier 1 (Go Heap RAM Buffer Pool & Materialized View — `16.0 MB`):** Allocated lazily on first tenant query (`RAMBufferPoolSlab` + pre-computed revenue/invoice aggregations and session query counters).
  * **Tier 2 (`DurableDir` Write-Ahead Log — `/data/wal.jsonl`, `~480 B`):** Append-only JSONL transaction log with SHA-256 integrity hashes and persisted worker pod IP history (`pod_ip_history`).
* **What do we learn:**
  You don't want to upload a 16 MB–4 GB RAM buffer pool to object storage for each of 10,000 customers when they go idle overnight! Substrate separates **short-term node-local checkpointing (`onPause`)** from **long-term object-store persistence (`onCommit`)**:
  * When `db-acme` is **Paused** (`onPause: SNAPSHOT_CONTENT_SCOPE_FULL`), both its `/data` WAL and its **`16.0 MB` Hot RAM Buffer Pool** stay frozen on the node (`recovery_mode: "HOT_RAM_RESUME"`, `0` WAL entries replayed, `ram_queries_since_boot` keeps incrementing).
  * When `db-acme` is **Suspended** (`onCommit: SNAPSHOT_CONTENT_SCOPE_DATA`), Substrate **discards the `16.0 MB` dirty RAM heap** and uploads *only* the compact `DurableDir` (`/data/wal.jsonl`, `~480 B`) to GCS—a **`99.997%` reduction in GCS snapshot bandwidth**! On the next HTTP request, `db-acme` wakes in ~100ms, replays `/data/wal.jsonl` in `<1ms` (`recovery_mode: "COLD_DATA_REPLAY"`, `wal_replayed_on_boot: N`), and rebuilds its `16.0 MB` Buffer Pool with zero data loss.
* **Why is Substrate here:**
  Kubernetes `PersistentVolumeClaims` (PVCs) are designed for tens of volumes per node with slow CSI attach/detach cycles (seconds to minutes)—not thousands of micro-volumes attaching and detaching in 100ms as individual customer databases multiplex across a 3-worker pool.
* **Why is this cool (The Stage "Wow" Factors):**
  1. **Two-Tier Storage vs. RAM X-Ray Inspector (`/dashboard`):** Watch 16 x `1 MB` RAM Buffer Pool pages stay hot on `Pause (FULL)`, visibly evaporate (`0 B uploaded`) on `Suspend (DATA)` with a live **`99.997%` GCS Snapshot Bandwidth Saved** meter, and auto-rehydrate from `/data/wal.jsonl` on the next query.
  2. **4 Customer Databases on 3 Worker Pods (`db-acme`, `db-globex`, `db-initech`, `db-umbrella`):** Every query executes `SELECT * FROM tenant_records` with **zero `WHERE tenant_id = ?` clauses** because each customer has a dedicated gVisor sandbox and `/data` volume.
  3. **1-Command GDPR Article 17 Shredder:** Deleting a customer's entire compute environment, memory checkpoints, and durable storage is literally `kubectl ate delete actor db-acme -a anything-but-agents` (`404 TENANT_SHREDDED`), while sibling `db-globex` stays 100% online and intact.
* **How does Substrate fit into this:**
  * **`DurableDir` volume (`volumes: [{name: data, durableDir: {}}]`)** provides a snapshot-backed per-actor filesystem directory (`/data`).
  * **Split `snapshotConfig` (`onPause: FULL`, `onCommit: DATA`)** gives hot-RAM latency for bursty sessions and `99.997%` smaller data-only storage for long-term hibernation.

---

### Demo 4: "The Truman Show Honeypot" — On-Demand Per-Attacker Deception Grid (`cmd/honeypot`)

* **Why this demo:**
  High-interaction security honeypots (or per-student CTF boxes, or per-PR preview environments) need strong sandbox isolation because untrusted users are actively trying to break them or modify their filesystems. Running thousands of idle VMs waiting for a scanner to hit them is prohibitively expensive, shared honeypots leak files across attackers, fake emulated shells break on real pipelines, and skilled intruders run `rm -rf` on their stage-1 droppers before disconnecting.
* **Why this demo specifically:**
  `cmd/honeypot` is a multi-mode binary (`--mode=actor|gateway|local`) with a **Split-Screen "Movie Set vs. Director's Booth" Web UI** (`:8084` locally or via in-cluster `Deployment/honeypot-gateway` + LoadBalancer) that puts the audience inside *The Truman Show*:
  * **Left Pane ("The Movie Set" — Attacker View `/shell`):** Poses as an exposed "AcmeCorp Internal Payroll v1.4" debug console running **real `/bin/sh -c` commands** inside a **`gVisor` (`sandboxClass: gvisor`)** sandbox (`debian:13-slim` base image override in `.ko.yaml`) pre-seeded with decoy lures (`payroll_db_credentials.env.bak` and `wire_transfer_batch_409.csv`). Crucially, `/shell` returns *only* realistic command output (`stdout`, `stderr`, `exit_code`, `files_in_dir`)—never leaking `honeypot`, `gVisor`, `canary`, or `forensics`.
  * **Right Pane ("The Director's Booth" — Hidden SOC View `/forensics`):** Exposes the silent in-memory surveillance state (`canary_alerts`, SHA-256 `rootfs_delta` tracking `UNCHANGED` / `ADDED` / `MODIFIED` / `DELETED` files, `gvisor_sandbox_proof` reading `/proc/version` from the gVisor Sentry kernel, `forensic_audit_trail`, and real GKE Worker Pod IP `pod_ip_history`).
* **What do we learn (and what attendees take home beyond honeypots):**
  1. **`SNAPSHOT_FIDELITY_MEMORY` = Ephemeral Disk + RAM as a Time Machine:** It captures both Go process heap RAM (`CanaryAlerts`, `ForensicLog`, numbered `/checkpoint/save` snapshots) *and* the **container rootfs overlay** (`/tmp/honeypot/*`) on top of the OCI image. Even `/tmp` files survive across different Kubernetes Worker Pod IPs (`pod_ip_history: ["10.120.1.22", "10.120.0.11"]`) without a `PersistentVolumeClaim`.
  2. **Solving the "Untrusted Code" Trilemma (`gVisor` + Golden Snapshot + Suspend-to-GCS):** Previously, running untrusted code (PR previews, webhook runners, grading sandboxes, bug-bounty targets) forced a choice between **security** (dedicated VMs), **cost** (shared containers), or **latency** (20s cold VM boots). Here you get **user-space kernel isolation (`Linux 4.19.0-gvisor`), $0 idle cost in GCS, and ~100ms wakeup**.
  3. **`revert` and `--tag` as Production Debugging & Forensics Primitives:** Because the SOC Gateway auto-checkpoints (`/checkpoint/save` + `kubectl ate suspend actor`) when a Canary alert or file drop occurs—while refusing to overwrite the checkpoint on destructive `rm` commands—an attacker's anti-forensic `rm -rf` wipe is undone in <1 second via `kubectl ate revert actor`, and `kubectl ate create tag` + `--tag` forks a live Copy-on-Write clone of a running container without interrupting the live session.
* **Why is Substrate here:**
  It combines **hard sandbox isolation (`gVisor`/`microVM`)**, **zero idle compute cost**, **per-attacker rootfs + memory continuity** routed dynamically via an HTTP header, and **anti-forensic time-travel (`suspend`, `revert`, `tag`)**.
* **What would be other ways to solve this:**
  * *Dedicated Honeypot VMs:* Expensive to run at high cardinality; requires manual cleanup/reset scripts once compromised, and if an attacker runs `rm -rf` on their payload before disconnecting, the malware sample is gone unless you had external block-storage snapshots.
  * *Stateless Honeypot Emulators (Cowrie, etc.):* Easily detected by attackers who test whether real shell pipelines and dropped files persist across sessions.
* **Why is this cool (The Stage "Wow" Factors):**
  1. **Per-IP Isolation & $0 Idle Persistence:** Attacker A (`203.0.113.5`) steals decoy DB credentials and plants `rootkit.sh` / `dropper.sh` in `/tmp/honeypot` (triggering an automatic GCS snapshot `v2`), then disconnects ($0 compute). Attacker B (`198.51.100.9`) connects 10 seconds later and sees a pristine filesystem cloned from the Golden Snapshot.
  2. **Un-deleting `rm -rf` across Disk & RAM with `kubectl ate revert`:** Weeks later, Attacker A returns—their personal "Truman Show" movie set wakes in `~100ms` (even on a different GKE worker pod IP!) with their script still on disk. The attacker panics and runs `rm -f` to wipe their tracks. On the SOC side, `/forensics` flags `CRITICAL: ANTI_FORENSIC_WIPE_ATTEMPT` (`DELETED`), and with one click (**Revert Wipe** -> `kubectl ate revert actor`), the SOC Director rewinds the container rootfs overlay and RAM back to `v2`, bringing the deleted malware and its SHA-256 hash right back onto `/tmp/honeypot`!
  3. **Live Forensic Cloning (`--tag`):** The SOC Director clicks **Clone to Forensic Lab** (`kubectl ate create tag` + `kubectl ate create actor honeypot-forensic-lab --tag`) to detonate and analyze the malware in an isolated parallel sandbox while the attacker keeps typing in the original sandbox, completely oblivious.
* **How does Substrate fit into this:**
  * **`sandboxConfig: {sandboxClass: gvisor}`** sandboxes untrusted `/bin/sh -c` execution.
  * **Rootfs delta checkpointing (`SNAPSHOT_FIDELITY_MEMORY`)** preserves `/tmp/honeypot/*` edits and RAM audit logs per actor across worker pod migrations without affecting the Golden Snapshot or other actors.
  * **`kubectl ate suspend actor` + `kubectl ate revert actor` + `kubectl ate create tag`** turn container checkpointing into an anti-forensic recovery and live malware-cloning engine.

---

### Demo 5: "Don't Try This at Home" — 3-Node Raft Split-Brain & Dual-Clock Time-Jump Lab (`cmd/timewarp`)

* **Why this demo:**
  The best way to truly understand a new infrastructure primitive is to see **where the wheels fall off**. If you tell engineers "Substrate can checkpoint and restore any container," someone will inevitably try to run a distributed consensus cluster (`etcd`/`Raft`), a distributed lock service, or a wall-clock-sensitive heartbeat daemon on it.
* **Why this demo specifically:**
  `cmd/timewarp` is a multi-mode binary (`--mode=actor|gateway|local`) with an interactive **Chronos Split-Brain & Dual-Clock Telemetry Rack** (`:8085` locally or via in-cluster `Deployment/timewarp-gateway` + LoadBalancer) that coordinates **3 consensus & distributed-lock actors (`raft-node-1`, `raft-node-2`, `raft-node-3`)**:
  1. **Step 1 — Healthy Term 1 Quorum:** `raft-node-1` is `LEADER (Term 1)` with a 3.0s lease; client `"Alice (checkout-svc)"` acquires exclusive lock `#101`, replicated across all 3 actors.
  2. **Step 2 — Freeze Term 1 Leader in GCS (`kubectl ate suspend`) & Elect Term 2 Leader:** `raft-node-1` is suspended to GCS for 5 seconds (`0 CPU instructions executed`; its heap clock freezes flat in amber at `Role = LEADER (Term 1)`, `Lock = "Alice"`). After 3.0 seconds of real-world wall-clock time, `raft-node-1`'s lease expires—surviving quorum (`raft-node-2` + `raft-node-3`) elects **`raft-node-2` as `LEADER (Term 2)`** and grants exclusive lock `#102` to `"Bob (inventory-svc)"`.
  3. **Step 3 — Wake the Zombie Leader via `atenet` (`"Mallory"` Split-Brain!):** A client request (`POST /lock/acquire?owner=Mallory`) hits `ate-target-actor: anything-but-agents/raft-node-1`. `atenet` restores `raft-node-1` from GCS in ~100ms (often on a different Worker Pod IP!). Because its restored RAM heap still believes it is `LEADER (Term 1)`, three failures fire at once:
     * **Wall-Clock Time-Jump Shock:** A single `100ms` tick measures a `5,000+ ms` gap (`50x` normal tick).
     * **Zombie Leader Split-Brain Corruption:** `raft-node-1` grants lock `#101` to `"Mallory"` in Term 1 while `raft-node-2` holds lock `#102` for `"Bob"` in Term 2—**two active leaders granting the same exclusive lock to two different clients!**
     * **Naive Rate-Limiter Burst Overflow:** `tokens += gap.Seconds() * 100` mints `+500 burst tokens` in a single wakeup tick.
  4. **Step 4 — Wakeup Clock-Jump Guardrail (`?guard=true`):** Toggling on the guardrail shows how valid Substrate actors protect themselves against wall-clock jumps: detecting `dt >> expectedTick` on wakeup, immediately fencing stale leases (`STALE_LEADER_FENCED` -> `409 Conflict`), and clamping rate-limiter token refills to `1x` tick.
* **What do we learn:**
  **Checkpointing freezes process memory, not the universe's wall clock.**
  * For consensus systems (`etcd`, `Raft`, `ZooKeeper`), continuous streaming brokers (`Kafka`), or 60Hz real-time game loops: **use Kubernetes `StatefulSets` or `Deployments`!** They are never idle (`1:1` multiplexing ratio) and require continuous wall-clock participation.
  * For workloads that *do* belong on Substrate: avoid unguarded `time.Since(lastTick)` multipliers, re-validate external leases/tokens on wakeup, and treat wall-clock discontinuities as normal lifecycle events.
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

./hack/stage-demo.sh act1-dungeon          # Act I: Zero-DB Dungeon + `kubectl ate revert` save-scumming
./hack/stage-demo.sh act1b-dungeon2d       # Act I-B: 2D Multilevel Dungeon (/play) + Live Dashboard (/dashboard)
./hack/stage-demo.sh deploy-dungeon2d      # Deploy in-cluster Dungeon 2D Gateway + LoadBalancer + PRESENTER_TOKEN Secret
./hack/stage-demo.sh cleanup-dungeon2d     # Clean up in-cluster Dungeon 2D Gateway & d2d-* actors
./hack/stage-demo.sh act2-coldstart        # Act II: 12s Cold-Start Index -> 100ms Golden Snapshot Clones + HPA QPS Race
./hack/stage-demo.sh deploy-coldstart-hpa  # Deploy in-cluster K8s vs Substrate HPA Stack + LoadBalancer + PRESENTER_TOKEN Secret
./hack/stage-demo.sh cleanup-coldstart-hpa # Clean up in-cluster Cold-Start HPA Stack & coldstart-* actors
./hack/stage-demo.sh act3-tenantdb         # Act III: Per-Tenant DB (DurableDir + 16MB Hot RAM vs DATA Commit + X-Ray UI)
./hack/stage-demo.sh deploy-tenantdb       # Deploy in-cluster TenantDB Storage & Memory Tiering Gateway + LoadBalancer
./hack/stage-demo.sh cleanup-tenantdb      # Clean up in-cluster TenantDB Gateway & db-* actors
./hack/stage-demo.sh act4-honeypot         # Act IV: Truman Show gVisor Honeypot + Split-Screen UI (:8084) & Revert Recovery
./hack/stage-demo.sh deploy-honeypot       # Deploy in-cluster Honeypot Split-Screen Gateway + LoadBalancer
./hack/stage-demo.sh cleanup-honeypot      # Clean up in-cluster Honeypot Gateway & honeypot-* actors
./hack/stage-demo.sh act5-timewarp         # Act V: 3-Node Raft Split-Brain & Dual-Clock Time-Jump Lab (:8085)
./hack/stage-demo.sh deploy-timewarp       # Deploy in-cluster Time-Warp Split-Brain Lab Gateway + LoadBalancer
./hack/stage-demo.sh cleanup-timewarp      # Clean up in-cluster Time-Warp Gateway & raft-node-* actors
./hack/stage-demo.sh deploy-gateway-tls    # Deploy GKE Gateway API + Certificate Manager TLS routes (*.substrate.learngke.dev)
./hack/stage-demo.sh cleanup-gateway-tls   # Clean up GKE Gateway API + HTTPRoute + HealthCheckPolicy resources
```

### 4. Cleanup

```bash
./hack/stage-demo.sh cleanup
```
