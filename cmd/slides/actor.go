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

// SlideDef defines the static metadata and Agent Substrate Day template content for a slide.
type SlideDef struct {
	ID                string            `json:"id"`
	Index             int               `json:"index"`
	Total             int               `json:"total"`
	Layout            string            `json:"layout"` // "asd-title" | "asd-demo" | "asd-cards" | "asd-two-col" | "asd-big-stat" | "asd-code" | "asd-closing"
	Kicker            string            `json:"kicker"`
	Chapter           string            `json:"chapter"`
	Badge             string            `json:"badge"`
	Title             string            `json:"title"`
	Subtitle          string            `json:"subtitle"`
	Kind              string            `json:"kind"` // "title" | "chapter" | "recap" | "closer"
	HighlightPhrases  []string          `json:"highlight_phrases,omitempty"`
	SpeakerByline     string            `json:"speaker_byline,omitempty"`
	EventFooter       string            `json:"event_footer,omitempty"`
	Points            []string          `json:"points"`
	Cards             []SlideCard       `json:"cards,omitempty"`
	Columns           []SlideColumn     `json:"columns,omitempty"`
	StatValue         string            `json:"stat_value,omitempty"`
	StatContext       string            `json:"stat_context,omitempty"`
	CodeFilename      string            `json:"code_filename,omitempty"`
	CodeLines         []string          `json:"code_lines,omitempty"`
	AgendaItems       []SlideAgendaItem `json:"agenda_items,omitempty"`
	SignOffLines      []string          `json:"sign_off_lines,omitempty"`
	DemoName          string            `json:"demo_name,omitempty"`
	DemoURL           string            `json:"demo_url,omitempty"`
	SubstrateFeatures []string          `json:"substrate_features"`
	SpeakerNotes      string            `json:"speaker_notes"`
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

// AllSlideDefs returns the full 12-slide presentation deck definitions styled
// after the official Agent Substrate Day (ATECon) speaker template.
func AllSlideDefs() []SlideDef {
	total := 12
	eventLine := "ATECon  •  Monday, November 9, 2026  •  Sky SLC"
	return []SlideDef{
		{
			ID:               "slide-01",
			Index:            1,
			Total:            total,
			Layout:           "asd-title",
			Kicker:           "AGENT SUBSTRATE DAY",
			Chapter:          "Cold open",
			Badge:            "AGENT SUBSTRATE DAY",
			Title:            "Running anything *but* agents on Agent Substrate",
			Subtitle:         "Five infrastructure crimes on three tiny Kubernetes pods — zero tokens, zero prompts",
			Kind:             "title",
			HighlightPhrases: []string{"*but*", "three tiny Kubernetes pods"},
			SpeakerByline:    "Mofi Rahman  •  Google Cloud Advocate, Google",
			EventFooter:      eventLine,
			Points: []string{
				"Zero tokens, zero prompts, and zero chatbots for the next 20 minutes",
				"Strip the AI away: a sub-second memory-checkpointing, wake-on-HTTP actor runtime",
				"Five non-agent workloads multiplexed across three 1-vCPU worker pods",
			},
			DemoName: "Scan audience QR & join Dungeon 2D",
			DemoURL:  "https://dungeon.substrate.learngke.dev/dashboard",
			SubstrateFeatures: []string{
				"WorkerPool (ate.dev/v1)",
				"Pod-Decoupled Identity",
				"gVisor Sandbox",
				"atenet Wake-on-HTTP",
			},
			SpeakerNotes: "Open with a 15-second title card, then flash the QR code. Tell the audience you are cheap and only provisioned three 1-CPU worker pods for the entire talk. Even this slide deck is 12 sleeping gVisor actors waking on arrow keys!",
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
			SpeakerNotes: "Think about how much of our lives we waste turning Go structs into SQL rows and back. For Demo 1: what if we just didn't use a database? Point out 50 people playing on 3 pods.",
		},
		{
			ID:               "slide-03",
			Index:            3,
			Total:            total,
			Layout:           "asd-cards",
			Kicker:           "DEMO 01 RECAP  •  SUBSTRATE SUPERPOWERS",
			Chapter:          "Demo 01 • Recap",
			Badge:            "DEMO 01 RECAP",
			Title:            "Look ma, no database",
			Subtitle:         "Three process-memory primitives we got for free without writing persistence code",
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
			SpeakerNotes: "Summarize: we just did git reset --hard on live RAM, hopped across worker pod IPs, and forked parallel timelines. Next up: autoscaling.",
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
			Kicker:           "DEMO 02 RECAP  •  SUBSTRATE SUPERPOWERS",
			Chapter:          "Demo 02 • Recap",
			Badge:            "DEMO 02 RECAP",
			Title:            "Golden snapshots beat cold boots",
			Subtitle:         "Build deterministic startup state once, then fork warm memory across pods",
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
			SpeakerNotes: "Substrate runs initialization once, freezes the warm heap into goldenTag, and forks it onto warm worker pods in 100ms. Photocopying 76MB of RAM is great, but what about nightly storage bills?",
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
			Kicker:           "DEMO 03 RECAP  •  MICRO-VOLUMES & TIERING",
			Chapter:          "Demo 03 • Recap",
			Badge:            "DEMO 03 RECAP",
			Title:            "Snapshot bandwidth saved by tiering hot RAM and DurableDir",
			Subtitle:         "Keep rebuildable caches hot during active bursts, commit only compact WAL logs for overnight hibernation",
			Kind:             "recap",
			StatValue:        "99.997%",
			StatContext:      "480 B /data/wal.jsonl committed on sync hibernate vs. 16.0 MB hot RAM buffer pool (<1ms wakeup replay)",
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
			SpeakerNotes: "DurableDir avoids Kubernetes PVC attach bottlenecks. Async suspend keeps 16 MB hot; sync hibernate drops cache and saves 99.997% bandwidth. Now, what if the user is an active hacker?",
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
			Kicker:           "DEMO 04 RECAP  •  SUBSTRATE SUPERPOWERS",
			Chapter:          "Demo 04 • Recap",
			Badge:            "DEMO 04 RECAP",
			Title:            "gVisor sandboxing and forensic time-travel",
			Subtitle:         "Un-deleting rm -rf and cloning a live compromised container while the attacker keeps typing",
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
			SpeakerNotes: "We un-deleted rm -rf and cloned a live crime scene without the hacker noticing. At this point, maximum hubris kicks in: let's put etcd and Raft on Substrate!",
		},
		{
			ID:               "slide-10",
			Index:            10,
			Total:            total,
			Layout:           "asd-demo",
			Kicker:           "LIVE DEMO 05  •  DISTRIBUTED CONSENSUS",
			Chapter:          "Demo 05 • Setup",
			Badge:            "LIVE DEMO 05",
			Title:            "Putting Raft in the freezer",
			Subtitle:         "What breaks when hubris takes over and you suspend the leader of a consensus cluster for five seconds?",
			Kind:             "chapter",
			HighlightPhrases: []string{"5,000ms clock jump", "dual-leader split-brain"},
			Points: []string{
				"Freeze raft-node-1 (Term 1 leader) while the real universe's clock moves on",
				"3-second lease expires; nodes 2 and 3 elect node 2 as Term 2 leader",
				"Waking node 1 triggers a 5,000ms clock jump and dual-leader split-brain",
			},
			DemoName: "Open Timewarp split-brain lab",
			DemoURL:  "https://timewarp.substrate.learngke.dev/dashboard",
			SubstrateFeatures: []string{
				"Wall-Clock Discontinuity",
				"Dual-Leader Split-Brain",
				"Wakeup Clock-Jump Fencing",
			},
			SpeakerNotes: "You don't understand a shiny new hammer until you smash your own thumb with it. Let's freeze the leader of a distributed consensus cluster and watch three shocks fire in one tick.",
		},
		{
			ID:               "slide-11",
			Index:            11,
			Total:            total,
			Layout:           "asd-two-col",
			Kicker:           "DEMO 05 RECAP  •  ARCHITECTURAL BOUNDARIES",
			Chapter:          "Demo 05 • Recap",
			Badge:            "LESSONS LEARNED",
			Title:            "The universe has a clock",
			Subtitle:         "Substrate freezes process RAM, never wall-clock time in the outside world",
			Kind:             "recap",
			HighlightPhrases: []string{"StatefulSets", "elapsed > tick", "wake-on-event"},
			Points: []string{
				"Substrate freezes process memory, never external wall clocks, leases, or OAuth expirations",
				"Guardrail (?guard=true): detect elapsed > tick on wakeup to fence stale leases with 409 Conflict",
				"Keep continuous consensus heartbeats on StatefulSets; run bursty wake-on-event actors on Substrate",
			},
			Columns: []SlideColumn{
				{
					Heading: "Keep on StatefulSets & Deployments",
					Bullets: []string{
						"Consensus voters (etcd, Raft) with continuous heartbeat leases",
						"Always-on stream brokers and 100% duty-cycle 60Hz loops",
						"Processes that assume unbroken monotonic wall-clock ticks",
					},
				},
				{
					Heading: "Run on Agent Substrate Actors",
					Bullets: []string{
						"Bursty wake-on-event sessions with high idle-to-active ratios",
						"Isolated per-user sandboxes, warm caches, and stateful tools",
						"Apps that check elapsed > tick on wakeup to fence stale leases",
					},
				},
			},
			DemoName: "Revisit Timewarp lab",
			DemoURL:  "https://timewarp.substrate.learngke.dev/dashboard",
			SubstrateFeatures: []string{
				"Wakeup Guardrails (?guard=true)",
				"Lease Fencing (409 Conflict)",
				"Actor vs StatefulSet Boundaries",
			},
			SpeakerNotes: "Never freeze continuous heartbeats. Check for wall-clock jumps on wakeup, and know what stays on StatefulSets versus Substrate. Now stack all five demos together for the punchline.",
		},
		{
			ID:               "slide-12",
			Index:            12,
			Total:            total,
			Layout:           "asd-closing",
			Kicker:           "AGENT SUBSTRATE DAY  •  THE PUNCHLINE",
			Chapter:          "Closer • Synthesis",
			Badge:            "THE PUNCHLINE",
			Title:            "Oops, we just built an AI agent by accident",
			Subtitle:         "An AI agent is the first workload that needs every one of these systems superpowers at once",
			Kind:             "closer",
			EventFooter:      eventLine,
			HighlightPhrases: []string{"AI agent", "systems superpowers"},
			Points: []string{
				"Dungeon 2D: working memory that checkpoints, reverts, and branches into parallel timelines",
				"Coldstart: heavy 12-second runtimes cloned in ~100ms from Golden Snapshots",
				"TenantDB: isolated per-user /data workspaces that burst for seconds, then sleep",
				"Honeypot: untrusted /bin/sh inside gVisor when someone inevitably hallucinates rm -rf",
				"Timewarp: driven by discrete turn bursts — never continuous consensus heartbeats",
			},
			AgendaItems: []SlideAgendaItem{
				{Number: "01", Topic: "Working memory with undo & branching", Detail: "Dungeon 2D  •  SNAPSHOT_FIDELITY_MEMORY, revert, --tag"},
				{Number: "02", Topic: "Heavy runtimes cloned in ~100ms", Detail: "Coldstart  •  ActorTemplate Golden Snapshots"},
				{Number: "03", Topic: "Isolated /data workspaces that sleep", Detail: "TenantDB  •  DurableDir & tiered suspend"},
				{Number: "04", Topic: "Untrusted /bin/sh trapped in gVisor", Detail: "Honeypot  •  Sandbox isolation & forensic rewind"},
				{Number: "05", Topic: "Discrete turn bursts, zero heartbeats", Detail: "Timewarp  •  Wake-on-event actor lifecycle"},
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
				"gVisor Sandbox",
				"Wake-on-HTTP",
			},
			SpeakerNotes: "Deliver the punchline: Substrate isn't for agents because someone slapped an AI label on Kubernetes. It is for agents because an agent is a save-scumming gamer, a heavy runtime, an idle workspace, and a chaotic shell user in a trench coat!",
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
