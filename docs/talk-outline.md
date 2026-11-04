# Talk Outline: "Running Anything *But* Agents on Agent Substrate"

## Final CFP Abstract

> Agent Substrate is for agents. It is right there in the name. Or is it? Strip away the LLM orchestration frameworks, and what you are left with is a systems engineer's dream: an ultra-high-density, multi-tenant sandboxing platform built on Kubernetes that bypasses standard Pod lifecycle constraints.
>
> What happens when we hijack these primitives for workloads that have absolutely nothing to do with AI? In this talk, we will explore the limits of Substrate by utilizing its core building blocks: native process-level suspend/resume, rapid rehydration, and pod-decoupled scaling.
>
> Through this exercise of "unconventional systems-engineering design patterns" we'll look at the good ideas, the terrible ideas, and how pushing this infrastructure to its limits helps us all gain a better understanding of why these exact primitives make Substrate the perfect foundation for the agentic era.

## Stage Philosophy (20 Minutes Total)

1. **The Conference Rebel Hook:** Open by declaring that at an **Agent Substrate** conference where every talk has an LLM on screen, we are going rogue—zero models, zero prompts, and zero chatbots—raiding Substrate for raw systems primitives across **five rapid-fire non-AI demos** sharing **three tiny 1-vCPU pods** (`manifests/workerpool.yaml.tmpl`).
2. **Collect One Puzzle Piece Per Demo:** Keep a tight, repeatable 3-beat rhythm for all 5 demos:
   - **Setup Slide (`slide-02, 04, 06, 08, 10`, ~15s):** Why a lazy/rebellious systems engineer built this non-agent app.
   - **Live Show-Don't-Tell (~2m):** Click through the live UI so the room sees the behavior firsthand.
   - **Puzzle Piece Recap Slide (`slide-03, 05, 07, 09, 11`, ~35s):** Name the exact Agent Substrate primitive we just pocketed (`PUZZLE PIECE 01 / 05` .. `05 / 05`).
3. **The 3-Stage Jigsaw Epiphany (`slide-12` .. `slide-15`):** Put all 5 stolen rebel primitives on a `3×3` interlocking jigsaw board (`slide-12`), have the realization *"Wait... flip those 5 pieces over—those are the exact runtime superpowers an AI agent needs!"* (`slide-13`), snap in the **3 built-in Agent Substrate security/platform pieces** (`ActorEgressPolicy`, `ate-secret://`, `OpenFGA` + `microVM`/CSI) plus the **1 center keystone you bring (`Model + agent loop`)** (`slide-14`), and close cleanly (`slide-15`).

```mermaid
flowchart LR
    Open["00:00 Rebel Open (1.5m): Zero LLMs, 15 Slide Actors, 3 Pods"]
    D1["01:30 Piece 01 (3.2m): Dungeon 2D (RAM Save, Revert & Fork)"]
    D2["04:45 Piece 02 (3.2m): Coldstart Search (100ms Golden Clone)"]
    D3["08:00 Piece 03 (3.0m): TenantDB (DurableDir & Tiered Suspend)"]
    D4["11:00 Piece 04 (3.5m): Honeypot (gVisor & Anti-rm -rf)"]
    D5["14:30 Piece 05 (3.2m): Timewarp (Turn-Driven Wake & Clock Fence)"]
    Close["17:45 Jigsaw Epiphany (2.2m): 5 Flips + 3 Substrate + Model = Agent"]

    Open --> D1 --> D2 --> D3 --> D4 --> D5 --> Close
```

## 20-Minute Run of Show

| Time | Segment | Duration | Slides & Live Screen | Puzzle Piece / Substrate Primitives |
|---|---|---|---|---|
| `00:00` | **Rebel Cold Open: Zero Models, Zero Prompts** (`cmd/slides`) | `1.5 min` | `slide-01` (`abs.slides.substrate.learngke.dev/deck`) + QR | 15 sleeping slide actors, 0/3 workers held when idle, live heap state |
| `01:30` | **Demo 1: "I Was Too Lazy to Set Up Postgres"** (`cmd/dungeon2d`) | `3.2 min` | `slide-02` → `dungeon.substrate.learngke.dev` → `slide-03` | **Piece 01/05:** `SNAPSHOT_FIDELITY_MEMORY`, `revert`, `create tag` (`--tag`) |
| `04:45` | **Demo 2: "Cheating at the Autoscaling Olympics"** (`cmd/coldstart`) | `3.2 min` | `slide-04` → `coldstart.substrate.learngke.dev` → `slide-05` | **Piece 02/05:** `ActorTemplate` + `wakeupProbe` `goldenTag` (~100ms clone) |
| `08:00` | **Demo 3: "The Lazy DBA's Multi-Tenant SaaS"** (`cmd/tenantdb`) | `3.0 min` | `slide-06` → `tenantdb.substrate.learngke.dev` → `slide-07` | **Piece 03/05:** `DurableDir` (`/data`), async vs. sync suspend, `delete actor` |
| `11:00` | **Demo 4: "The Truman Show for Hackers"** (`cmd/honeypot`) | `3.5 min` | `slide-08` → `honeypot.substrate.learngke.dev` → `slide-09` | **Piece 04/05:** `gVisor` Sentry sandbox, rootfs overlay + forensic `revert`/`--tag` |
| `14:30` | **Demo 5: "Putting Raft in the Freezer"** (`cmd/timewarp`) | `3.2 min` | `slide-10` → `timewarp.substrate.learngke.dev` → `slide-11` | **Piece 05/05:** Turn-driven `atenet` Wake-on-HTTP, request parking & `?guard=true` |
| `17:45` | **The Jigsaw Epiphany: "Wait... We Built an Agent"** (`cmd/slides`) | `2.2 min` | `slide-12` (Loot) → `slide-13` (Flip) → `slide-14` (9/9) → `slide-15` | `+ ActorEgressPolicy` + `ate-secret://` + `OpenFGA`/`microVM` + `Model & Loop` |

## Pre-Stage Setup Checklist

1. **Cluster & Golden Snapshots (`v1.0.0-rc.2`):** Confirm `kubectl get workerpool anything-but-agents -n anything-but-agents` shows `3/3` ready `ateworker-gvisor` pods and `kubectl ate get actor-templates -a anything-but-agents` has `goldenTag` populated on all templates.
2. **Browser tabs pre-opened left-to-right (sign in once with Google on Tab 0 to unlock all 6 tabs via cross-subdomain SSO, and click the top-right Sun icon for Projector Light Mode if the room is bright):**
   - Tab 0: `https://abs.slides.substrate.learngke.dev/deck` (Live Slide Deck)
   - Tab 1: `https://dungeon.substrate.learngke.dev/dashboard` (with QR modal open!) + mobile `/play` tab
   - Tab 2: `https://coldstart.substrate.learngke.dev/dashboard`
   - Tab 3: `https://tenantdb.substrate.learngke.dev/dashboard`
   - Tab 4: `https://honeypot.substrate.learngke.dev/`
   - Tab 5: `https://timewarp.substrate.learngke.dev/dashboard`
3. **Clear stuck actors before walking on stage:** Run `./hack/stage-demo.sh act1-dungeon` once or check `kubectl ate get actors -a anything-but-agents` so all 3 worker slots start free.

## Minute-by-Minute Show Flow

### Cold Open: The Conference Rebel (`00:00 - 01:30`, `slide-01`)

* **On screen:** `slide-01` ("Running anything *but* agents on Agent Substrate") on `abs.slides.substrate.learngke.dev/deck`, then click the CTA button / Tab 1 (`dungeon2d` `/dashboard` with the giant QR code open next to `replicas: 3` from `manifests/workerpool.yaml.tmpl`).
* **Light spoken hook:**
  * *"We are at Agent Substrate Day, which means every other talk today has an LLM, a prompt, and an agent loop on screen. I'm feeling rebellious—so for the next 18 minutes, I promise you zero models, zero prompts, and zero chatbots."*
  * *"Instead, we're going to raid Agent Substrate for raw systems primitives across five rapid-fire non-AI demos—and pocket one puzzle piece from Substrate after every demo."*
  * *"Also, I am cheap. So all five demos—plus this 15-slide presentation deck, where every individual slide is its own sleeping gVisor actor—are sharing **three** 1-CPU worker pods. Scan that QR code right now, pick a class, and let's start stealing primitives."*

### Demo 1: "I Was Too Lazy to Set Up Postgres" (`01:30 - 05:00`, `cmd/dungeon2d`)

* **The Joke / Setup (15s):**
  * *"Think about how much of our lives we waste turning Go structs into SQL rows before an HTTP request finishes, and turning SQL rows back into Go structs 200ms later. For Demo 1, I asked: what if I just... didn't use a database?"*
* **Show, Don't Tell (2m 15s live in Tab 1):**
  1. **Close the QR modal to reveal the Live Leaderboard:** Point at the screen: 20 to 50 people in the audience are playing an 8-floor roguelike right now, and all of them are sharing **3 pods** (`0` pods used while they run around at 60fps in the browser; an actor only wakes up for ~100ms when someone hits a Save Shrine!). Point out `pod_ip_history`: players hop from pod IP to pod IP between saves without losing a single gold coin.
  2. **Die on purpose:** Take your hero on Floor 8 straight into the **Ancient Heap Dragon** without fire protection -> `HP = 0`, dead on screen.
  3. **Save-Scum like a shameless gamer:** Click **Revert** (`kubectl ate revert actor d2d-mofi --snapshot-version`). Boom: alive again before the boss room! That was `git reset --hard` on live Go heap memory.
  4. **Cheat at a mutually exclusive choice:** Floor 6 has two locked chests and one key (`sunforged-aegis` vs. `voidbane-relic`). Click **Fork Timeline** (`--tag`) to clone your frozen memory into a parallel universe so you can open both.
* **What the Demo Does & What Substrate Gives Us (1m):**
  * **What the demo does:** Runs a multi-player 8-floor dungeon crawler where `100%` of server persistence is ordinary Go structs (`WorldState` / `ActorSession`) sitting in RAM, with zero SQL drivers, Redis clients, or files on disk.
  * **Features we get from Substrate:**
    * **`WorkerPool` + `atenet` Wake-on-HTTP:** An HTTP header (`ate-target-actor`) wakes a sleeping process on whichever of the 3 pods is free in ~100ms (and parks requests briefly if all 3 are busy), turning 3 pods into capacity for the whole room.
    * **Process Memory Checkpointing (`SNAPSHOT_FIDELITY_MEMORY`):** Freezes the Go heap and goroutine state to GCS when idle and restores it onto a totally different pod IP later.
    * **`kubectl ate revert` & `kubectl ate create tag` (`--tag`):** Infrastructure-level undo and branching timelines with zero application rollback code.

```mermaid
sequenceDiagram
    participant Phone as Audience Phone
    participant Router as atenet-router
    participant Worker as Any Free Pod (1 of 3)
    participant GCS as GCS Snapshot Store

    Phone->>Router: POST /save (ate-target-actor: d2d-player)
    Router->>Worker: Wake d2d-player (~100ms from GCS)
    Worker->>GCS: Stream Go Heap Snapshot in & out
    Worker-->>Phone: 200 OK (Saved v3 on new Worker Pod IP!)
```

* **Transition joke to Demo 2:**
  * *"Okay, I can hear the backend engineers thinking: 'Cute game, Mofi, I don't run dungeons at work, I run serious stateless services behind HorizontalPodAutoscaler.' Great! Let's race."*

### Demo 2: "Cheating at the Autoscaling Olympics" (`05:00 - 08:30`, `cmd/coldstart`)

* **The Joke / Setup (15s):**
  * *"Who here runs a 'stateless' Java app, Python ML service, or search engine that takes 12+ seconds to boot before `/readyz` turns green? When traffic spikes, HPA panics and adds pods, and your new pods spend 12 seconds doing yoga while Pod 1 catches fire."*
* **Show, Don't Tell (2m 15s live in Tab 2):**
  1. **Show the contestants:** A real 50,000-package BM25 + trigram code search engine (~76 MB Go heap, capped at `20 QPS` per pod) that takes **12 seconds** across 6 startup stages to build its in-memory index. Click **Probe Both Backends** to show `coldstart-1` and `coldstart-2` returning real search hits with the **exact same `golden_heap_fingerprint`**.
  2. **Hit the `60 QPS` Spike button (`1 -> 3` replicas):** Watch the oscilloscope and 6-stage LED matrix live!
     * **Top track (Standard K8s HPA):** Pods 2 and 3 sit at `503` slowly ticking through `Stage 1/6... 2/6...` for 12 agonizing seconds while Pod 1 drowns in red `429 Too Many Requests` crosshatch.
     * **Bottom track (Substrate HPA):** Actors 2 and 3 wake up in **~100ms** with the 50,000-doc index *already built in RAM*, jumping straight to `60 QPS` and serving ~2x the total queries!
* **What the Demo Does & What Substrate Gives Us (1m):**
  * **What the demo does:** Races standard Kubernetes HPA against Substrate under a `3x` traffic spike on a heavy 50,000-document in-memory search engine.
  * **Features we get from Substrate:**
    * **`ActorTemplate` + `wakeupProbe` Golden Snapshots:** Substrate runs the 12-second index build **once** when you register the template, waits for `/readyz == 200 OK`, and freezes that warm 76 MB heap into a Golden Snapshot (`goldenTag`).
    * **Cluster-Wide `fork()` & Pod-Decoupled Scaling:** Scaling from `1 -> 3` stamps out pre-warmed clones onto warm worker pods in ~100ms instead of repeating deterministic startup work on every scale-out event.

* **Transition joke to Demo 3:**
  * *"Photocopying 76 MB of read-only RAM is awesome. Know what is less awesome? Uploading 10,000 giant RAM caches to GCS every night when your SaaS customers go to bed."*

### Demo 3: "The Lazy DBA's Multi-Tenant SaaS" (`08:30 - 11:30`, `cmd/tenantdb`)

* **The Joke / Setup (15s):**
  * *"Every SaaS engineer has two nightmares: forgetting `WHERE tenant_id = ?` on a SQL query, and getting a GDPR 'delete all my data' ticket across 50 foreign-key tables on Friday at 4:55 PM. So I gave every customer their own private database binary... on our same 3 pods."*
* **Show, Don't Tell (2m live in Tab 3):**
  1. **Four DBs, Three Pods, Zero `tenant_id`:** Show `db-acme`, `db-globex`, `db-initech`, and `db-umbrella` multiplexing across the 3 worker slots. Every query is literally `SELECT * FROM tenant_records` because each tenant lives in its own isolated sandbox!
  2. **The Storage vs. RAM X-Ray:** Warm up `db-acme`'s `16.0 MB` RAM Buffer Pool. Click **Quick Suspend (`--mode=async`)** -> query it: `HOT_RAM_RESUME` (the 16 MB cache stayed hot!). Now click **Cold Hibernate (`--mode=sync`)** -> watch the 16 MB scratch cache evaporate so only the **`~480 B` `/data/wal.jsonl`** uploads to GCS (**`99.997%` bandwidth saved**!), replaying in `<1ms` on the next click.
  3. **The Friday 4:55 PM GDPR Button:** Acme Corp churns and demands GDPR deletion. Click **GDPR Shred** (`kubectl ate delete actor db-acme`): compute, RAM snapshots, and `/data` volume are vaporized in one command (`404 TENANT_SHREDDED`) while `db-globex` next door doesn't drop a beat.
* **What the Demo Does & What Substrate Gives Us (45s):**
  * **What the demo does:** Runs a dedicated embedded database + 16 MB buffer pool per SaaS customer without Kubernetes PVC attach bottlenecks or giant object-storage bills.
  * **Features we get from Substrate:**
    * **`DurableDir` Micro-Volumes (`volumes: [{durableDir: {}}]`):** Snapshot-backed `/data` directories that attach and detach in ~100ms across thousands of tenants (unlike K8s PVCs, which cap out at tens per node and take seconds/minutes to mount).
    * **Hot-RAM (`--mode=async`) vs. Compact Durable (`--mode=sync`) Suspend:** Keep RAM warm during active bursts, drop rebuildable caches for long hibernation.
    * **1-Command Tenant Shredding (`kubectl ate delete actor`):** Per-customer blast radius and instant cryptographic/storage cleanup.

* **Transition joke to Demo 4:**
  * *"Acme and Globex were polite paying customers. What happens when the user connecting to your app is an active hacker running `rm -rf`?"*

### Demo 4: "The Truman Show for Hackers" (`11:30 - 15:00`, `cmd/honeypot`)

* **The Joke / Setup (15s):**
  * *"Security honeypots usually suck: real VMs cost a fortune to leave idle, and fake Python shell emulators fool nobody. With Substrate, every sketchy IP address gets its own private *Truman Show* movie set."*
* **Show, Don't Tell (2m 30s live in Tab 4):**
  1. **Left ("The Movie Set") vs. Right ("The Director's Booth"):** On the left terminal, pretend to be Attacker `203.0.113.5` poking around an "exposed AcmeCorp Payroll console." Run real `/bin/sh` commands: `cat payroll_db_credentials.env.bak` and drop `/tmp/honeypot/dropper.sh`. Point to the right pane: silent SOC telemetry confirms `Linux 4.19.0-gvisor`, logs Canary alerts, hashes `dropper.sh`, and suspends the crime scene to GCS (`$0` idle cost while a second attacker IP gets a clean box!).
  2. **The Attacker Panics (`rm -f`):** Reconnect as `203.0.113.5` (it wakes in ~100ms on a *different* pod IP with `dropper.sh` still sitting in `/tmp`!). Run `rm -f dropper.sh` to destroy the evidence. The SOC pane flashes `ANTI_FORENSIC_WIPE_ATTEMPT (DELETED)`.
  3. **Un-deleting `rm -rf` & Secret Cloning:** Click **Revert Wipe** (`kubectl ate revert actor`) in the SOC booth -> the container filesystem rewinds and **`dropper.sh` pops right back onto disk**! Then click **Clone to Forensic Lab** (`--tag`) to spin up an identical copy of the compromised container for malware detonation while the hacker keeps typing on the left, totally clueless.
* **What the Demo Does & What Substrate Gives Us (45s):**
  * **What the demo does:** Gives every attacker IP a dedicated Linux shell environment with `$0` idle cost, invisible SOC telemetry, un-deletable evidence, and live sandbox cloning.
  * **Features we get from Substrate:**
    * **Native `gVisor` Isolation (`sandboxClasses: [gvisor]`):** Real `/bin/sh` Linux binaries trapped inside a user-space application kernel (`ateworker-gvisor`), solving the VM-security vs. `$0`-idle vs. 100ms-wakeup trilemma.
    * **Rootfs Overlay + RAM Persistence:** Even ephemeral `/tmp` edits survive suspend/resume across different GKE worker pods without a PVC.
    * **Forensic Time-Travel (`revert` + `--tag`):** Rewind destructive filesystem mutations or fork a live container Copy-on-Write to debug/inspect it safely on the side.

* **Transition joke to Demo 5 (Maximum Hubris):**
  * *"Pause and admire our ego right now. We deleted Postgres! We humiliated HPA! We solved GDPR and un-deleted `rm -rf`! At this point every engineer in the room has the same intrusive thought: 'Put EVERYTHING on Substrate! Let's put `etcd` and Raft on Substrate so our control plane scales to zero!' ... Let's watch how fast that ruins your weekend."*

### Demo 5: "Putting Raft in the Freezer" (`15:00 - 18:30`, `cmd/timewarp`)

* **The Joke / Setup (15s):**
  * *"You don't really understand a shiny new hammer until you smash your own thumb with it. Let's freeze the leader of a distributed consensus cluster."*
* **Show, Don't Tell (2m 15s live in Tab 5):**
  1. **Happy Cluster:** `raft-node-1..3` are up. `raft-node-1` is `LEADER (Term 1)` with a 3-second lease; Alice holds exclusive lock `#101`. Both clocks tick together.
  2. **Put Leader 1 in the Freezer:** Click **Suspend Term 1 Leader** (`kubectl ate suspend actor raft-node-1 --mode=sync`). Its CPU instructions drop to `0` and its heap clock freezes flat in amber. Count 3 seconds out loud: its lease expires in the real universe! `raft-node-2` and `raft-node-3` elect **`raft-node-2` as `LEADER (Term 2)`** and give exclusive lock `#102` to Bob.
  3. **Wake the Zombie:** Send a write from Mallory to `raft-node-1`. `atenet` cheerfully wakes `raft-node-1` up in 100ms! Because its frozen brain has no idea 5 seconds passed, three explosions happen in one tick:
     * **5,000ms Clock-Jump Shock** on a 100ms ticker.
     * **Dual-Leader Split-Brain:** `raft-node-1` still thinks it is `LEADER (Term 1)` and hands lock `#101` to Mallory while Bob holds `#102` on Node 2, corrupting the KV store on screen!
     * **Token-Bucket Jackpot:** `tokens += elapsed * rate` mints `+500` burst tokens at once.
  4. **The Seatbelt (`?guard=true`):** Toggle **Clock-Jump Guardrail ON** and replay: now on wakeup the process sees `dt >> 100ms`, immediately fences its stale lease (`409 Conflict`), and clamps its rate limiter.
* **What the Demo Does & Puzzle Piece #5 Grabbed (`slide-11`, 45s):**
  * **What the demo does:** Intentionally split-brains a 3-node Raft & distributed lock cluster by suspending its leader across a lease timeout, then fixes the wakeup shock with `?guard=true`.
  * **Puzzle Piece 05 / 05 we pocket from Substrate:**
    * **Turn-Driven `atenet` Wake-on-HTTP & Request Parking:** Substrate isn't built for continuous 100ms consensus heartbeats (leave `etcd` and `Kafka` on `StatefulSets`!). It is engineered for **discrete request/turn bursts** that wake in ~100ms on HTTP and sleep at `$0` between turns.
    * **Wakeup Clock-Jump Guardrail (`?guard=true`):** Because freezing RAM does not freeze the universe's wall clock, bursty actors fence stale leases/OAuth tokens and clamp `time.Since(lastTick)` on wakeup.

### The Epiphany Finale: Snapping the 9-Piece Agent Puzzle Together (`17:45 - 20:00`, `slide-12` .. `slide-15`)

* **Step 1 — `slide-12` ("Five non-AI demos, five stolen Substrate primitives" — `puzzle_stage: "loot"`):**
  * Put all 5 stolen Rebel Demo jigsaw pieces onto the `3×3` board (`Dungeon 2D`, `Coldstart`, `TenantDB`, `Honeypot`, `Timewarp`) with 4 empty dashed cutouts remaining.
  * *"Okay, rebel mission accomplished: zero LLMs used, and look at the five systems primitives we just looted onto our board."*
* **Step 2 — `slide-13` ("Wait... flip those five pieces over" — `puzzle_stage: "epiphany"`):**
  * Press `→` (or click individual jigsaw tiles / **Flip 5 demo pieces** live on screen) to flip all 5 Rebel tiles into green **Agent Superpowers**:
    1. **Piece 01 (`Dungeon 2D` → Working memory & branching):** Undo bad tool calls (`revert`) and fork parallel plans (`--tag`).
    2. **Piece 02 (`Coldstart` → 100ms warm toolchains):** Pre-baked Python, Node, browser, and repo index ready instantly (`goldenTag`).
    3. **Piece 03 (`TenantDB` → Sleeping `/workspace` per user):** Burst for 5 seconds, then pay `$0` while the human thinks (`DurableDir`).
    4. **Piece 04 (`Honeypot` → Safe untrusted bash sandbox):** Contain prompt-injected `/bin/sh` inside `gVisor` and rewind `rm -rf`.
    5. **Piece 05 (`Timewarp` → Turn-driven wake & refresh):** Wake on each user/tool turn via `atenet` and fence stale credentials on resume.
  * Point at the 4 empty dashed jigsaw cutouts now asking: *"What drives the loop? Outbound network guard? Keep API keys out of RAM? Tenant authz & VM options?"*
* **Step 3 — `slide-14` ("Plus 3 Substrate guards + a model = a production agent" — `puzzle_stage: "complete"`):**
  * Press `→` to watch the final 4 jigsaw pieces snap into place:
    * **3 `"ALSO IN SUBSTRATE"` platform/security pieces** completing the 8-piece outer ring:
      * **`ActorEgressPolicy` firewall:** Deny-by-default L7 HTTP & DNS egress control so sandboxes cannot exfiltrate data.
      * **`ate-secret://` MITM credential injection:** Inject real LLM/API keys at the egress proxy boundary so secrets **never** enter actor RAM or GCS snapshots.
      * **`OpenFGA` `AccessPolicy`, `microVM` (Kata) & CSI volumes:** Fine-grained per-tenant authorization and hardware VM / storage options.
    * **1 Center Bullseye Keystone (`"YOU BRING THIS"`):** **`Model + agent loop`** (Gemini, Claude, or open weights + MCP tools).
  * **Final spoken punchline:**
    > *"Look at that 9-piece board. Eight of the nine pieces are pure distributed-systems infrastructure—and Agent Substrate gives you all eight out of the box. You bring the center piece—your model and agent loop—and that is why Agent Substrate is an awesome place to run agents."*
* **Step 4 — `slide-15` ("Thank you" — `asd-closing`):**
  * Clean closing summary + GitHub repo QR/link for Q&A.

## Quick Reference: Demo-to-Substrate Feature Cheat Sheet

| Demo | What the Demo Does | Features We Get from Substrate (`v1.0.0-rc.2`) |
|---|---|---|
| **0. `cmd/slides`** | Presentation deck where each slide (`slide-01..15`) is an actor sleeping in GCS | 15 sleeping slide actors, 0/3 workers held when idle, `SNAPSHOT_FIDELITY_MEMORY` heap views + `kubectl ate revert` + `asd-puzzle` finale |
| **1. `cmd/dungeon2d`** | Multi-player 8-floor browser roguelike with zero DB code | `WorkerPool` (`ate.dev/v1`), `atenet` Wake-on-HTTP, `SNAPSHOT_FIDELITY_MEMORY`, `save` / `--snapshot-version`, `revert`, `create tag` + `--tag` |
| **2. `cmd/coldstart`** | 50k-doc in-memory search engine in a `60 QPS` HPA race | `ActorTemplate` + `wakeupProbe` (`/readyz` -> `goldenTag` Golden Snapshot), ~100ms Copy-on-Write heap restore, pod-decoupled scaling |
| **3. `cmd/tenantdb`** | 1 isolated DB + 16 MB cache per customer across 3 pods | `DurableDir` (`/data`), `suspend --mode=async` (hot RAM) vs. `--mode=sync` (compact WAL, `99.997%` GCS saved), 1-cmd `delete actor` |
| **4. `cmd/honeypot`** | Per-IP `gVisor` hacker Movie Set + SOC forensic booth | `sandboxClasses: [gvisor]` (`ateworker-gvisor`), OCI rootfs overlay + RAM persistence across pod IPs, `revert` (`rm -rf` undo), `--tag` live clone |
| **5. `cmd/timewarp`** | 3-node Raft cluster broken by a 5s leader freeze | Turn-driven `atenet` Wake-on-HTTP + request parking, wakeup clock-jump fencing (`?guard=true`), `Actor` vs. `StatefulSet` boundary |

## Pocket Q&A Answers (If Time Permits or Hallway Track)

* **Why run `dungeon2d` 60fps movement in the browser instead of on the actor?** Ticking an idle player at 60Hz on the server pins a worker pod `1:1` (Demo 5's anti-pattern). Keeping frame rendering client-side and waking the actor on `atenet` only for Save Shrines, revert, fork, and victory lets 50+ audience phones share 3 pods.
* **Why does `coldstart` put a custom L7 router in front of K8s `Deployment`?** Fairness! Default `kube-proxy` pins HTTP/1.1 keep-alives to Pod 1 even after Pods 2 and 3 turn Ready. Giving K8s its own endpoint-watching L7 router isolates the 12s cold boot as the *only* difference.
* **What happens to open TCP connections when an actor suspends?** Inbound HTTP goes through `atenet-router` (`:443` mTLS), which routes to whichever pod wakes the actor. Outbound TCP sockets to external DBs will time out over long freezes, so use HTTP RPCs or let connection pools reconnect lazily on wakeup.
* **Why `gVisor` instead of Linux CRIU on `runc`?** You cannot run untrusted multi-tenant `/bin/sh` (`honeypot` or AI agents) safely on `runc`, and because gVisor's Sentry is a user-space kernel in Go, serializing kernel + heap + rootfs overlay into one portable snapshot restores cleanly across nodes without host-PID conflicts.
* **How does this differ from Cloud Run / Knative or Durable Objects / Temporal?** Cloud Run scales to zero by killing your process (wiping RAM and paying the 12s cold boot next time); Durable Objects and Temporal require rewriting code in a custom runtime/SDK. Substrate checkpoints ordinary Linux containers in place.
