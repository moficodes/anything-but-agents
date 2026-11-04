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
			RebelTitle:  "Zero-DB game saves & undo",
			RebelDetail: "SNAPSHOT_FIDELITY_MEMORY • revert • --tag",
			AgentTitle:  "Working memory & branching",
			AgentDetail: "Undo bad tool calls & fork parallel plans",
		},
		{
			Slot:        1,
			Code:        "02",
			Kind:        "demo",
			Filled:      true,
			Face:        demoFace,
			Badge:       "PIECE 02 • COLDSTART",
			RebelTitle:  "12s search index HPA clone",
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
			RebelTitle:  "1 private DB per customer",
			RebelDetail: "DurableDir /data • tiered suspend • delete",
			AgentTitle:  "Sleeping /workspace per user",
			AgentDetail: "Burst 5s, pay $0 while the human thinks",
		},
		{
			Slot:        3,
			Code:        "04",
			Kind:        "demo",
			Filled:      true,
			Face:        demoFace,
			Badge:       "PIECE 04 • HONEYPOT",
			RebelTitle:  "Hacker /bin/sh movie set",
			RebelDetail: "gVisor Sentry kernel • rootfs rewind",
			AgentTitle:  "Safe untrusted bash sandbox",
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
			AgentDetail: "Gemini / Claude / open weights + MCP tools",
			EmptyLabel:  emptyCenter,
		},
		{
			Slot:        5,
			Code:        "05",
			Kind:        "demo",
			Filled:      true,
			Face:        demoFace,
			Badge:       "PIECE 05 • TIMEWARP",
			RebelTitle:  "Wake-on-HTTP & clock fence",
			RebelDetail: "atenet request parking • ?guard=true",
			AgentTitle:  "Turn-driven wake & refresh",
			AgentDetail: "Sleep between turns & fence stale tokens",
		},
		{
			Slot:        6,
			Code:        "S1",
			Kind:        "substrate",
			Filled:      fillExtras,
			Face:        "extra",
			Badge:       "ALSO IN SUBSTRATE",
			AgentTitle:  "ActorEgressPolicy firewall",
			AgentDetail: "Deny-by-default L7 & DNS egress control",
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
			AgentDetail: "Inject API keys on egress; 0 keys in GCS",
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

// AllSlideDefs returns the full 15-slide presentation deck definitions styled
// after the official Agent Substrate Day (ATECon) speaker template.
func AllSlideDefs() []SlideDef {
	total := 15
	eventLine := "ATECon  •  Monday, November 9, 2026  •  Sky SLC"
	return []SlideDef{
		{
			ID:               "slide-01",
			Index:            1,
			Total:            total,
			Layout:           "asd-title",
			Kicker:           "AGENT SUBSTRATE DAY  •  REBEL TRACK",
			Chapter:          "Cold open",
			Badge:            "AGENT SUBSTRATE DAY",
			Title:            "Running anything *but* agents on Agent Substrate",
			Subtitle:         "Every talk today has an LLM. Let's be rebels: zero models, zero prompts, five weird demos on three tiny pods.",
			Kind:             "title",
			HighlightPhrases: []string{"*but*", "zero models, zero prompts", "three tiny pods"},
			SpeakerByline:    "Mofi Rahman  •  Google Cloud Advocate, Google",
			EventFooter:      eventLine,
			Points: []string{
				"Zero tokens, zero prompts, and zero chatbots for the next 18 minutes",
				"Raiding Agent Substrate for raw systems superpowers across five non-AI apps",
				"All five demos (plus this 15-actor slide deck) share three 1-vCPU worker pods",
			},
			DemoName: "Scan audience QR & join Dungeon 2D",
			DemoURL:  "https://dungeon.substrate.learngke.dev/dashboard",
			SubstrateFeatures: []string{
				"WorkerPool (ate.dev/v1)",
				"Pod-Decoupled Identity",
				"gVisor Sandbox",
				"atenet Wake-on-HTTP",
			},
			SpeakerNotes: "Open as the conference rebel: we are at Agent Substrate Day, everyone else has LLMs on screen, so I promise zero models and zero prompts. We're raiding Substrate for raw systems primitives across five rapid-fire demos on three 1-CPU pods—even this slide deck is 15 sleeping gVisor actors!",
		},
		{
			ID:               "slide-02",
			Index:            2,
			Total:            total,
			Layout:           "asd-demo",
			Kicker:           "LIVE DEMO 01  •  ZERO DATABASE",
			Chapter:          "Demo 01 • Setup",
			Badge:            "LIVE DEMO 01",
			Title:            "I was too lazy to set up Postgres",
			Subtitle:         "What happens when 50 players share 3 pods and 100% of game state lives in Go heap RAM?",
			Kind:             "chapter",
			HighlightPhrases: []string{"Go heap RAM", "$0 compute"},
			Points: []string{
				"100% of 8-floor game state lives in ordinary structs in Go heap RAM",
				"Zero SQL rows, zero Redis keys, and zero disk serialization",
				"Idle heroes sleep in GCS at $0 compute and wake in ~100ms at Save Shrines",
			},
			DemoName: "Launch Dungeon 2D live demo",
			DemoURL:  "https://dungeon.substrate.learngke.dev/dashboard",
			SubstrateFeatures: []string{
				"SNAPSHOT_FIDELITY_MEMORY",
				"atenet-router",
				"Zero Database Architecture",
			},
			SpeakerNotes: "First rebel experiment: a game with save files, boss-death undo, and forked save slots—without writing a single SQL query. Point out 50 people playing on 3 pods.",
		},
		{
			ID:               "slide-03",
			Index:            3,
			Total:            total,
			Layout:           "asd-cards",
			Kicker:           "PUZZLE PIECE 01 / 05  •  SUBSTRATE MEMORY",
			Chapter:          "Piece 01 • Memory",
			Badge:            "PIECE 01 / 05",
			Title:            "Look ma, no database",
			Subtitle:         "Piece #1 grabbed from Substrate: checkpoint live RAM, rewind mistakes, and fork new timelines",
			Kind:             "recap",
			HighlightPhrases: []string{"SNAPSHOT_FIDELITY_MEMORY", "git reset --hard", "parallel universes"},
			Points: []string{
				"SNAPSHOT_FIDELITY_MEMORY freezes the Go heap to GCS on idle and restores onto any free pod in ~100ms",
				"Save-scumming via kubectl ate revert actor runs git reset --hard on live RAM after dying on Floor 8",
				"Timeline branching via kubectl ate create tag clones frozen memory into parallel universes",
			},
			Cards: []SlideCard{
				{
					Number: "01",
					Title:  "Freeze Go heap",
					Body:   "SNAPSHOT_FIDELITY_MEMORY checkpoints live structs to GCS when idle and wakes on any pod IP in ~100ms.",
				},
				{
					Number: "02",
					Title:  "Undo death in RAM",
					Body:   "kubectl ate revert actor rewinds a dead Floor 8 hero like git reset --hard on live process memory.",
				},
				{
					Number: "03",
					Title:  "Branch timelines",
					Body:   "kubectl ate create tag clones frozen memory into parallel universes so one key opens both chests.",
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
			SpeakerNotes: "Pocket Piece #1: process memory checkpointing, revert, and tag branching. Next rebel experiment: beating Kubernetes HPA.",
		},
		{
			ID:               "slide-04",
			Index:            4,
			Total:            total,
			Layout:           "asd-demo",
			Kicker:           "LIVE DEMO 02  •  AUTOSCALING RACE",
			Chapter:          "Demo 02 • Setup",
			Badge:            "LIVE DEMO 02",
			Title:            "Cheating at the autoscaling olympics",
			Subtitle:         "Racing standard Kubernetes HPA against Golden Snapshots when a 12-second cold boot hits a 3x traffic spike",
			Kind:             "chapter",
			HighlightPhrases: []string{"12 seconds", "429 load shedding"},
			Points: []string{
				"50,000-package BM25 + trigram search index takes 12 seconds to build in RAM",
				"New Kubernetes pods sit at 503 while Pod 1 drowns in 429 load shedding",
				"Both stacks run behind identical 2-hop L7 routers for a fair race",
			},
			DemoName: "Open Cold-Start HPA race",
			DemoURL:  "https://coldstart.substrate.learngke.dev/dashboard",
			SubstrateFeatures: []string{
				"ActorTemplate",
				"wakeupProbe",
				"Golden Snapshots",
				"external.metrics.k8s.io",
			},
			SpeakerNotes: "Who runs a Java, Python, or search service that takes 12 seconds to boot? When traffic spikes from 20 to 60 QPS, standard HPA adds pods that spend 12 seconds warming up while Pod 1 catches fire.",
		},
		{
			ID:               "slide-05",
			Index:            5,
			Total:            total,
			Layout:           "asd-two-col",
			Kicker:           "PUZZLE PIECE 02 / 05  •  GOLDEN SNAPSHOTS",
			Chapter:          "Piece 02 • Warm Clone",
			Badge:            "PIECE 02 / 05",
			Title:            "Golden snapshots beat cold boots",
			Subtitle:         "Piece #2 grabbed from Substrate: pay deterministic startup once, then clone warm RAM in ~100ms",
			Kind:             "recap",
			HighlightPhrases: []string{"goldenTag", "~100ms", "0% shed rate"},
			Points: []string{
				"ActorTemplate runs the 12-second index build once and freezes the 76 MB heap into goldenTag at /readyz == 200",
				"Scaling 1 -> 3 clones warm replicas in ~100ms instead of repeating deterministic initialization",
				"Zero cold-boot penalty under a 3x spike: ~2x total served queries with 0% shed rate",
			},
			Columns: []SlideColumn{
				{
					Heading: "Standard Kubernetes HPA",
					Bullets: []string{
						"Repeats 12-second index build on every scale-out event",
						"Scale-out pods fail /readyz while traffic spikes to 60 QPS",
						"Pod 1 drops ~50% of live user queries with HTTP 429",
					},
				},
				{
					Heading: "Substrate Golden Snapshots",
					Bullets: []string{
						"Runs 12-second warmup once at ActorTemplate registration",
						"Captures warm 76 MB heap into goldenTag when /readyz hits 200",
						"Clones warm replicas in ~100ms with 0% shed rate",
					},
				},
			},
			DemoName: "Revisit Cold-Start oscilloscope",
			DemoURL:  "https://coldstart.substrate.learngke.dev/dashboard",
			SubstrateFeatures: []string{
				"ActorTemplate Golden Snapshots",
				"wakeupProbe /readyz",
				"Cluster-Wide fork()",
				"Pod-Decoupled Scaling",
			},
			SpeakerNotes: "Pocket Piece #2: ActorTemplate + wakeupProbe Golden Snapshots. Photocopying 76 MB of RAM is awesome, but what about per-customer storage?",
		},
		{
			ID:               "slide-06",
			Index:            6,
			Total:            total,
			Layout:           "asd-demo",
			Kicker:           "LIVE DEMO 03  •  ONE DB PER CUSTOMER",
			Chapter:          "Demo 03 • Setup",
			Badge:            "LIVE DEMO 03",
			Title:            "The lazy DBA's multi-tenant SaaS",
			Subtitle:         "Giving every customer their own private database process on 3 pods — with zero WHERE tenant_id clauses",
			Kind:             "chapter",
			HighlightPhrases: []string{"zero WHERE tenant_id", "GDPR deletion"},
			Points: []string{
				"Acme, Globex, Initech, and Umbrella each get a dedicated embedded database",
				"No shared tables and zero WHERE tenant_id security leak anxiety",
				"One-command Friday 4:55 PM GDPR deletion without foreign-key archaeology",
			},
			DemoName: "Open TenantDB X-Ray console",
			DemoURL:  "https://tenantdb.substrate.learngke.dev/dashboard",
			SubstrateFeatures: []string{
				"DurableDir (/data)",
				"suspend --mode=async",
				"suspend --mode=sync",
				"kubectl ate delete actor",
			},
			SpeakerNotes: "Every SaaS engineer fears forgetting WHERE tenant_id or getting a Friday 4:55 PM GDPR delete ticket across 50 tables. So we gave each customer their own database binary sharing 3 pods.",
		},
		{
			ID:               "slide-07",
			Index:            7,
			Total:            total,
			Layout:           "asd-big-stat",
			Kicker:           "PUZZLE PIECE 03 / 05  •  DURABLEDIR & TIERING",
			Chapter:          "Piece 03 • Workspaces",
			Badge:            "PIECE 03 / 05",
			Title:            "Snapshot bandwidth saved by tiering hot RAM and DurableDir",
			Subtitle:         "Piece #3 grabbed from Substrate: private /data micro-volumes that burst hot and hibernate tiny",
			Kind:             "recap",
			StatValue:        "99.997%",
			StatContext:      "Piece #3 unlocked: 480 B /data/wal.jsonl on sync hibernate vs. 16.0 MB hot RAM buffer pool (<1ms wakeup replay)",
			HighlightPhrases: []string{"16.0 MB", "480 B", "kubectl ate delete actor"},
			Points: []string{
				"Async suspend keeps the 16.0 MB buffer pool warm in memory for active customer bursts",
				"Sync hibernate discards rebuildable RAM cache and uploads only 480 B /data/wal.jsonl",
				"One-command GDPR shred via kubectl ate delete actor wipes compute, RAM, and storage",
			},
			DemoName: "Revisit TenantDB console",
			DemoURL:  "https://tenantdb.substrate.learngke.dev/dashboard",
			SubstrateFeatures: []string{
				"DurableDir Micro-Volumes",
				"Async vs Sync Suspend Tiering",
				"WAL Fast-Replay (<1ms)",
				"GDPR Blast-Radius Isolation",
			},
			SpeakerNotes: "Pocket Piece #3: DurableDir micro-volumes, hot vs. cold suspend tiering, and instant actor deletion. Now, what if the user is an active hacker?",
		},
		{
			ID:               "slide-08",
			Index:            8,
			Total:            total,
			Layout:           "asd-demo",
			Kicker:           "LIVE DEMO 04  •  DECEPTION SANDBOX",
			Chapter:          "Demo 04 • Setup",
			Badge:            "LIVE DEMO 04",
			Title:            "The Truman Show for hackers",
			Subtitle:         "Every suspicious IP gets its own private Linux movie set while the SOC watches behind a one-way mirror",
			Kind:             "chapter",
			HighlightPhrases: []string{"gVisor Sentry", "rm -f"},
			Points: []string{
				"Dedicated /bin/sh Linux environment per attacker IP at $0 idle cost",
				"gVisor Sentry user-space kernel traps syscalls, fires canaries, and hashes drops",
				"Watch the attacker drop malware, panic, and run rm -f to wipe the crime scene",
			},
			DemoName: "Open Honeypot movie set",
			DemoURL:  "https://honeypot.substrate.learngke.dev/",
			SubstrateFeatures: []string{
				"sandboxClasses: [gvisor]",
				"Rootfs Overlay Snapshots",
				"Silent Canary Telemetry",
			},
			SpeakerNotes: "Honeypots usually suck: idle VMs cost a fortune and Python shell emulators fool nobody. With Substrate, every attacker IP gets a real Linux movie set inside gVisor.",
		},
		{
			ID:               "slide-09",
			Index:            9,
			Total:            total,
			Layout:           "asd-code",
			Kicker:           "PUZZLE PIECE 04 / 05  •  GVISOR & FORENSICS",
			Chapter:          "Piece 04 • Sandbox",
			Badge:            "PIECE 04 / 05",
			Title:            "gVisor sandboxing and forensic time-travel",
			Subtitle:         "Piece #4 grabbed from Substrate: isolate hostile /bin/sh code and un-delete rm -rf",
			Kind:             "recap",
			CodeFilename:     "forensic-playbook.sh",
			CodeLines: []string{
				"# Attacker drops payload inside gVisor Sentry kernel, then tries to wipe evidence:",
				"$ rm -f /tmp/honeypot/dropper.sh",
				"",
				"# 1. Rewind rootfs overlay & memory snapshot to un-delete wiped malware:",
				"$ kubectl ate revert actor honeypot-203-0-113-5 -a anything-but-agents",
				"",
				"# 2. Clone live crime scene into a forensic copy without alerting the attacker:",
				"$ kubectl ate create tag crime-scene-v1 --actor honeypot-203-0-113-5",
				"$ kubectl ate create actor forensic-lab --template honeypot --tag crime-scene-v1",
			},
			HighlightPhrases: []string{"forensic time-travel", "kubectl ate revert", "--tag"},
			Points: []string{
				"gVisor Sentry kernel isolates real Linux binaries in user space with zero host escape path",
				"Rootfs overlay persistence carries /tmp payloads across worker pod hops without a PVC",
				"Forensic revert and --tag cloning recover wiped files and detonate copies safely",
			},
			DemoName: "Revisit Honeypot console",
			DemoURL:  "https://honeypot.substrate.learngke.dev/",
			SubstrateFeatures: []string{
				"gVisor Application Kernel",
				"Rootfs Overlay Snapshots",
				"kubectl ate revert (Anti-Wipe)",
				"Forensic Tag Cloning (--tag)",
			},
			SpeakerNotes: "Pocket Piece #4: gVisor kernel sandboxing + rootfs time-travel. Now hubris peaks: we beat Postgres, HPA, SaaS DBs, and hackers—let's freeze Raft!",
		},
		{
			ID:               "slide-10",
			Index:            10,
			Total:            total,
			Layout:           "asd-demo",
			Kicker:           "LIVE DEMO 05  •  TESTING THE BOUNDARY",
			Chapter:          "Demo 05 • Setup",
			Badge:            "LIVE DEMO 05",
			Title:            "Putting Raft in the freezer",
			Subtitle:         "Maximum hubris: we beat databases, HPA, and hackers — what happens when we freeze consensus?",
			Kind:             "chapter",
			HighlightPhrases: []string{"5,000ms clock jump", "dual-leader split-brain"},
			Points: []string{
				"Freeze raft-node-1 (Term 1 leader) while the real universe's clock moves on",
				"Its 3-second lease expires; nodes 2 and 3 elect node 2 as Term 2 leader",
				"atenet wakes node 1 on HTTP — triggering a 5,000ms clock jump and dual-leader split-brain",
			},
			DemoName: "Open Timewarp split-brain lab",
			DemoURL:  "https://timewarp.substrate.learngke.dev/dashboard",
			SubstrateFeatures: []string{
				"atenet Request Parking",
				"Wall-Clock Discontinuity",
				"Wakeup Clock-Jump Fencing",
			},
			SpeakerNotes: "You don't understand a shiny new hammer until you smash your own thumb with it. Let's freeze the leader of a Raft cluster, watch it split-brain, and see what shape of workload Substrate actually wants.",
		},
		{
			ID:               "slide-11",
			Index:            11,
			Total:            total,
			Layout:           "asd-two-col",
			Kicker:           "PUZZLE PIECE 05 / 05  •  TURN WAKE & CLOCK FENCE",
			Chapter:          "Piece 05 • Turn Wake",
			Badge:            "PIECE 05 / 05",
			Title:            "Built for discrete turns, not heartbeats",
			Subtitle:         "Piece #5 grabbed from Substrate: park HTTP requests, wake on demand, and fence stale clocks",
			Kind:             "recap",
			HighlightPhrases: []string{"atenet", "elapsed > tick", "StatefulSets", "discrete turns"},
			Points: []string{
				"Substrate freezes process RAM and CLOCK_MONOTONIC, never external wall clocks or leases",
				"atenet parks incoming HTTP requests, wakes sleeping actors in ~100ms, and runs discrete turns",
				"Guardrail (?guard=true) detects elapsed > tick on wake to fence stale leases and refresh tokens",
			},
			Columns: []SlideColumn{
				{
					Heading: "Why Raft Exploded (Heartbeat Daemons)",
					Bullets: []string{
						"Assumes continuous 100ms ticks and unbroken leases",
						"Freezing RAM pauses CLOCK_MONOTONIC while real time moves on",
						"Keep etcd, Raft, and Kafka on Kubernetes StatefulSets",
					},
				},
				{
					Heading: "Piece #5: Turn-Driven Wake & Park",
					Bullets: []string{
						"atenet parks HTTP bursts and wakes sleeping actors in ~100ms",
						"Sleep at $0 for minutes or hours between discrete user turns",
						"Check elapsed > tick (?guard=true) to fence expired leases",
					},
				},
			},
			DemoName: "Revisit Timewarp lab",
			DemoURL:  "https://timewarp.substrate.learngke.dev/dashboard",
			SubstrateFeatures: []string{
				"atenet Wake & Request Parking",
				"Wakeup Guardrails (?guard=true)",
				"Actor vs StatefulSet Boundaries",
			},
			SpeakerNotes: "Pocket Piece #5: turn-driven wake-on-HTTP with request parking and clock-jump guardrails. Now let's put all five pieces we stole onto the table...",
		},
		{
			ID:               "slide-12",
			Index:            12,
			Total:            total,
			Layout:           "asd-puzzle",
			Kicker:           "THE REBEL LOOT  •  ZERO AI SO FAR",
			Chapter:          "Finale 1 • Rebel Loot",
			Badge:            "5 PIECES STOLEN",
			Title:            "Five Substrate features we grabbed for non-AI apps",
			Subtitle:         "Put all five pieces from our rapid-fire demos on the board... wait, what shape is this?",
			Kind:             "puzzle",
			PuzzleStage:      "loot",
			PuzzlePieces:     buildPuzzlePieces("loot"),
			HighlightPhrases: []string{"Five Substrate features", "what shape is this?"},
			Points: []string{
				"Piece 01 (Dungeon 2D): SNAPSHOT_FIDELITY_MEMORY, revert, and --tag branching",
				"Piece 02 (Coldstart): ActorTemplate + wakeupProbe Golden Snapshots (~100ms clone)",
				"Piece 03 (TenantDB): DurableDir /data micro-volumes and tiered suspend",
				"Piece 04 (Honeypot): gVisor Sentry kernel sandbox and rootfs rewind",
				"Piece 05 (Timewarp): Turn-driven atenet wake-on-HTTP, parking, and clock fencing",
			},
			SubstrateFeatures: []string{
				"SNAPSHOT_FIDELITY_MEMORY",
				"Golden Snapshots",
				"DurableDir + gVisor + atenet",
			},
			SpeakerNotes: "Pause here before the punchline! Read off the five non-agent features we just grabbed from Substrate. Point at the jigsaw board: look at those five pieces together... wait a second, what kind of workload needs all five of these at once?",
		},
		{
			ID:               "slide-13",
			Index:            13,
			Total:            total,
			Layout:           "asd-puzzle",
			Kicker:           "THE EPIPHANY  •  FLIP THE PIECES OVER",
			Chapter:          "Finale 2 • Epiphany",
			Badge:            "WAIT A SECOND...",
			Title:            "Hold on... every piece we stole is what an *agent* needs",
			Subtitle:         "Flip our five rebel hacks over — they map 1-to-1 onto the hardest problems in agent infrastructure",
			Kind:             "puzzle",
			PuzzleStage:      "epiphany",
			PuzzlePieces:     buildPuzzlePieces("epiphany"),
			HighlightPhrases: []string{"*agent*", "1-to-1"},
			Points: []string{
				"Working memory with undo & branching -> rollback bad tool calls and explore parallel plans",
				"100ms warm toolchains -> Python, Node, browser, and repo indexes ready on Turn 1",
				"Sleeping /workspace per user -> burst for 5 seconds, pay $0 while the human reads",
				"Safe untrusted bash in gVisor -> survive prompt injection and hallucinated rm -rf",
				"Turn-driven wake & clock fence -> sleep between turns and refresh expired tokens on wake",
			},
			SubstrateFeatures: []string{
				"Agent Working Memory",
				"100ms Warm Toolchains",
				"Sandboxed Turn Execution",
			},
			SpeakerNotes: "Have the epiphany live! Flip the 5 pieces over (and click any tile if you want to toggle it): a save-scumming gamer, a heavy search index, a sleeping workspace, a chaotic bash user, and a turn-driven lifecycle... that IS an AI agent! Now look at the four empty slots—what else do we need?",
		},
		{
			ID:               "slide-14",
			Index:            14,
			Total:            total,
			Layout:           "asd-puzzle",
			Kicker:           "COMPLETE PUZZLE  •  SUBSTRATE + YOUR MODEL",
			Chapter:          "Finale 3 • Complete",
			Badge:            "PUZZLE COMPLETE",
			Title:            "Add Substrate's security layer + a *model* = production agent",
			Subtitle:         "All 8 outer pieces ship in Agent Substrate — drop your model & agent loop into the center",
			Kind:             "puzzle",
			PuzzleStage:      "complete",
			PuzzlePieces:     buildPuzzlePieces("complete"),
			HighlightPhrases: []string{"*model*", "All 8 outer pieces"},
			Points: []string{
				"Also in Substrate: deny-by-default ActorEgressPolicy L7 & DNS outbound firewall",
				"Also in Substrate: ate-secret:// MITM auth injects API keys outside gVisor (0 keys in GCS snapshots)",
				"Also in Substrate: OpenFGA per-tenant AccessPolicy, SPIFFE mTLS, Kata microVMs & CSI volumes",
				"Center keystone (you bring): frontier or open model + a 50-line tool/MCP agent loop",
			},
			SubstrateFeatures: []string{
				"ActorEgressPolicy",
				"ate-secret:// MITM Secrets",
				"OpenFGA + microVM + Model",
			},
			SpeakerNotes: "Snap the final four pieces into place! Three more pieces are ALREADY inside Agent Substrate—ActorEgressPolicy outbound firewall, ate-secret:// MITM credential injection so API keys never leak into GCS RAM dumps, and OpenFGA multi-tenant authz + microVM/CSI options. Drop your model and loop into the center, and you have a production agent!",
		},
		{
			ID:               "slide-15",
			Index:            15,
			Total:            total,
			Layout:           "asd-closing",
			Kicker:           "AGENT SUBSTRATE DAY  •  THANK YOU",
			Chapter:          "Closer • Sign-off",
			Badge:            "ATECON 2026",
			Title:            "Come for the rebel hacks, stay for the *agents*",
			Subtitle:         "All five demos, Kubernetes manifests, and this 15-actor slide deck are open source",
			Kind:             "closer",
			EventFooter:      eventLine,
			HighlightPhrases: []string{"*agents*"},
			Points: []string{
				"Dungeon 2D: working memory that checkpoints, reverts, and branches into parallel timelines",
				"Coldstart: heavy 12-second runtimes cloned in ~100ms from Golden Snapshots",
				"TenantDB: isolated per-user /data workspaces that burst for seconds, then sleep",
				"Honeypot: untrusted /bin/sh inside gVisor when someone inevitably hallucinates rm -rf",
				"Timewarp + Egress: turn-driven wakeups, ActorEgressPolicy, and ate-secret:// + your model",
			},
			AgendaItems: []SlideAgendaItem{
				{Number: "01", Topic: "Working memory with undo & branching", Detail: "Dungeon 2D  •  SNAPSHOT_FIDELITY_MEMORY, revert, --tag"},
				{Number: "02", Topic: "Heavy runtimes cloned in ~100ms", Detail: "Coldstart  •  ActorTemplate Golden Snapshots"},
				{Number: "03", Topic: "Isolated /data workspaces that sleep", Detail: "TenantDB  •  DurableDir & tiered suspend"},
				{Number: "04", Topic: "Untrusted /bin/sh trapped in gVisor", Detail: "Honeypot  •  Sandbox isolation & forensic rewind"},
				{Number: "05", Topic: "Turn wake + zero-trust + your model", Detail: "Timewarp + Egress  •  atenet, ActorEgressPolicy & ate-secret://"},
			},
			SignOffLines: []string{
				"Mofi Rahman  •  Google Cloud Advocate",
				"@moficodes  •  learngke.dev",
				"github.com/moficodes/anything-but-agents",
			},
			DemoName: "Explore repo & manifests on GitHub",
			DemoURL:  "https://github.com/moficodes/anything-but-agents",
			SubstrateFeatures: []string{
				"Memory Checkpointing",
				"Golden Snapshots",
				"DurableDir Micro-Volumes",
				"gVisor + Egress MITM",
				"Wake-on-HTTP",
			},
			SpeakerNotes: "Land the close: Agent Substrate isn't awesome for agents because someone slapped an AI sticker on Kubernetes—it's awesome because an agent is the first mainstream workload that needs all eight of those systems primitives at the same time. Thank you!",
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
