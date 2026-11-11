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
	"time"
)

const (
	DocMergerAcquisition = "doc-merger-acquisition"
	DocQ4Roadmap         = "doc-q4-roadmap"
	DocIncidentPostmortem = "doc-incident-postmortem"

	PrincipalAlice   = "user:alice"
	PrincipalBob     = "user:bob"
	PrincipalCharlie = "user:charlie"

	RoleOwner  = "owner"
	RoleEditor = "editor"
	RoleViewer = "viewer"

	PermCanGet          = "can_get"
	PermCanUpdate       = "can_update"
	PermCanDelete       = "can_delete"
	PermCanManagePolicy = "can_manage_policy"
	PermCanResume       = "can_resume"
	PermCanSuspend      = "can_suspend"

	ActorStateRunning   = "RUNNING"
	ActorStateSuspended = "SUSPENDED"
)

type PrincipalInfo struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Title       string `json:"title"`
	RoleDesc    string `json:"role_desc"`
	AvatarColor string `json:"avatar_color"`
	Badge       string `json:"badge"`
}

var KnownPrincipals = map[string]PrincipalInfo{
	PrincipalAlice: {
		ID:          PrincipalAlice,
		Name:        "Alice Chen",
		Title:       "VP Strategy & M&A",
		RoleDesc:    "Document Owner & Executive Sponsor",
		AvatarColor: "#8b5cf6",
		Badge:       "OWNER",
	},
	PrincipalBob: {
		ID:          PrincipalBob,
		Name:        "Bob Martinez",
		Title:       "Staff Systems Architect",
		RoleDesc:    "Core Architecture & Infrastructure Designer",
		AvatarColor: "#38bdf8",
		Badge:       "STAFF ARCHITECT",
	},
	PrincipalCharlie: {
		ID:          PrincipalCharlie,
		Name:        "Charlie Vance",
		Title:       "External Security Contractor",
		RoleDesc:    "Third-Party Auditor & Consultant",
		AvatarColor: "#f59e0b",
		Badge:       "CONTRACTOR",
	},
}

type CanvasNode struct {
	ID        string    `json:"id"`
	Type      string    `json:"type"` // "service", "database", "gateway", "queue", "client", "milestone"
	Label     string    `json:"label"`
	SubLabel  string    `json:"sub_label,omitempty"`
	X         int       `json:"x"`
	Y         int       `json:"y"`
	Width     int       `json:"width"`
	Height    int       `json:"height"`
	Color     string    `json:"color"`
	Author    string    `json:"author"`
	UpdatedAt time.Time `json:"updated_at"`
}

type CanvasEdge struct {
	ID        string    `json:"id"`
	From      string    `json:"from"`
	To        string    `json:"to"`
	Label     string    `json:"label,omitempty"`
	Style     string    `json:"style"` // "solid", "dashed"
	Author    string    `json:"author"`
	UpdatedAt time.Time `json:"updated_at"`
}

type CanvasNote struct {
	ID        string    `json:"id"`
	Text      string    `json:"text"`
	X         int       `json:"x"`
	Y         int       `json:"y"`
	Width     int       `json:"width"`
	Height    int       `json:"height"`
	Color     string    `json:"color"`
	Author    string    `json:"author"`
	UpdatedAt time.Time `json:"updated_at"`
}

type HistoryEntry struct {
	Revision  int       `json:"revision"`
	Timestamp time.Time `json:"timestamp"`
	Action    string    `json:"action"`
	Author    string    `json:"author"`
	Detail    string    `json:"detail"`
}

type Collaborator struct {
	Principal string    `json:"principal"`
	Name      string    `json:"name"`
	Role      string    `json:"role"`
	LastSeen  time.Time `json:"last_seen"`
	IsActive  bool      `json:"is_active"`
}

type SceneGraph struct {
	DocID                  string         `json:"doc_id"`
	Title                  string         `json:"title"`
	SecurityClassification string         `json:"security_classification"`
	Revision               int            `json:"revision"`
	Nodes                  []CanvasNode   `json:"nodes"`
	Edges                  []CanvasEdge   `json:"edges"`
	Notes                  []CanvasNote   `json:"notes"`
	History                []HistoryEntry `json:"history"`
	Collaborators          []Collaborator `json:"collaborators"`
	RAMObjectCount         int            `json:"ram_object_count"`
	RAMBytesEstimate       int64          `json:"ram_bytes_estimate"`
	CurrentPodIP           string         `json:"current_pod_ip"`
	PodIPHistory           []string       `json:"pod_ip_history"`
	ProcessBootedAt        string         `json:"process_booted_at"`
	WakeCount              int            `json:"wake_count"`
	QueryCount             int            `json:"query_count"`
	ActorState             string         `json:"actor_state"` // RUNNING | SUSPENDED
}

type OpenFGATuple struct {
	User     string `json:"user"`
	Relation string `json:"relation"`
	Object   string `json:"object"`
}

type PerimeterLogEntry struct {
	Timestamp       time.Time `json:"timestamp"`
	Principal       string    `json:"principal"`
	Action          string    `json:"action"`
	Resource        string    `json:"resource"`
	Role            string    `json:"role,omitempty"`
	Decision        string    `json:"decision"` // "ALLOWED", "DENIED_AT_PERIMETER", "POLICY_UPDATE"
	Reason          string    `json:"reason"`
	ActorState      string    `json:"actor_state"`
	ActorWoken      bool      `json:"actor_woken"`
	ComputeWastedMs int       `json:"compute_wasted_ms"`
	LatencyMs       int       `json:"latency_ms"`
}

type PerimeterMetrics struct {
	BlockedWhileAsleep       int     `json:"blocked_while_asleep"`
	TotalBlockedUnauthorized int     `json:"total_blocked_unauthorized"`
	TotalAllowedCalls        int     `json:"total_allowed_calls"`
	ComputeSavedSeconds      float64 `json:"compute_saved_seconds"`
	WakeupsOnValidTraffic    int     `json:"wakeups_on_valid_traffic"`
	ActiveTuplesCount        int     `json:"active_tuples_count"`
}

type CanvasStateResponse struct {
	Scene                *SceneGraph         `json:"scene,omitempty"`
	DocID                string              `json:"doc_id"`
	Principal            string              `json:"principal"`
	PrincipalRole        string              `json:"principal_role"`
	CanEdit              bool                `json:"can_edit"`
	CanManage            bool                `json:"can_manage"`
	Tuples               []OpenFGATuple      `json:"tuples"`
	Metrics              PerimeterMetrics    `json:"metrics"`
	DecisionLog          []PerimeterLogEntry `json:"decision_log"`
	PolicyYAML           string              `json:"policy_yaml"`
	ActorState           string              `json:"actor_state"`
	IsDeniedBeforeWakeup bool                `json:"is_denied_before_wakeup"`
	DenialReason         string              `json:"denial_reason,omitempty"`
}

type MoveNodePayload struct {
	ID string `json:"id"`
	X  int    `json:"x"`
	Y  int    `json:"y"`
}

type DeletePayload struct {
	TargetType string `json:"target_type"` // "node", "edge", "note"
	ID         string `json:"id"`
}

type CanvasOp struct {
	Op        string           `json:"op"` // "add_node", "move_node", "add_note", "add_edge", "delete", "reset"
	Principal string           `json:"principal"`
	Node      *CanvasNode      `json:"node,omitempty"`
	Move      *MoveNodePayload `json:"move,omitempty"`
	Note      *CanvasNote      `json:"note,omitempty"`
	Edge      *CanvasEdge      `json:"edge,omitempty"`
	Delete    *DeletePayload   `json:"delete,omitempty"`
}

type DocumentCatalogEntry struct {
	DocID                  string
	Title                  string
	Subtitle               string
	SecurityClassification string
	InitialOwner           string
	InitialEditor          string
	InitialViewer          string
	DefaultNodes           []CanvasNode
	DefaultEdges           []CanvasEdge
	DefaultNotes           []CanvasNote
}

var DefaultDocumentCatalog = []DocumentCatalogEntry{
	{
		DocID:                  DocMergerAcquisition,
		Title:                  "Project Titan: M&A Topology & Asset Transfer",
		Subtitle:               "Acquisition Target Architecture & Secure Escrow Deal Flow",
		SecurityClassification: "CONFIDENTIAL // RESTRICTED",
		InitialOwner:           PrincipalAlice,
		InitialEditor:          "",
		InitialViewer:          "",
		DefaultNodes: []CanvasNode{
			{
				ID:       "node-target",
				Type:     "client",
				Label:    "Acme Cloud AI (Target)",
				SubLabel: "Proprietary 70B Model Weights",
				X:        60,
				Y:        190,
				Width:    220,
				Height:   80,
				Color:    "#ef4444",
				Author:   PrincipalAlice,
			},
			{
				ID:       "node-escrow",
				Type:     "database",
				Label:    "Sovereign Escrow Vault",
				SubLabel: "$2.4B Tranche Allocation",
				X:        390,
				Y:        60,
				Width:    220,
				Height:   80,
				Color:    "#f59e0b",
				Author:   PrincipalAlice,
			},
			{
				ID:       "node-legal",
				Type:     "gateway",
				Label:    "FTC / DOJ Regulatory Gateway",
				SubLabel: "HSR Antitrust Clearance",
				X:        730,
				Y:        190,
				Width:    220,
				Height:   80,
				Color:    "#8b5cf6",
				Author:   PrincipalAlice,
			},
			{
				ID:       "node-closing",
				Type:     "queue",
				Label:    "Day 1 Ingestion Pipeline",
				SubLabel: "Multi-Cloud Tenant Migration",
				X:        390,
				Y:        320,
				Width:    220,
				Height:   80,
				Color:    "#ec4899",
				Author:   PrincipalAlice,
			},
		},
		DefaultEdges: []CanvasEdge{
			{
				ID:     "edge-1",
				From:   "node-target",
				To:     "node-escrow",
				Label:  "Asset Transfer Valuation",
				Style:  "solid",
				Author: PrincipalAlice,
			},
			{
				ID:     "edge-2",
				From:   "node-escrow",
				To:     "node-legal",
				Label:  "HSR Clearance Filing",
				Style:  "dashed",
				Author: PrincipalAlice,
			},
			{
				ID:     "edge-3",
				From:   "node-target",
				To:     "node-closing",
				Label:  "IP & Weights Sync",
				Style:  "solid",
				Author: PrincipalAlice,
			},
		},
		DefaultNotes: []CanvasNote{
			{
				ID:     "note-secret",
				Text:   "DO NOT DISTRIBUTE — Board & Executive Eyes Only. Project Titan deal terms pending HSR clearance.",
				X:      60,
				Y:      30,
				Width:  210,
				Height: 100,
				Color:  "#fef08a",
				Author: PrincipalAlice,
			},
		},
	},
	{
		DocID:                  DocQ4Roadmap,
		Title:                  "Q4 Infrastructure Roadmap & Mesh Topology",
		Subtitle:               "Multi-Region Agent Substrate Runtime & Snapshot Mesh",
		SecurityClassification: "INTERNAL // ARCHITECTURE",
		InitialOwner:           PrincipalAlice,
		InitialEditor:          PrincipalBob,
		InitialViewer:          "",
		DefaultNodes: []CanvasNode{
			{
				ID:       "node-clients",
				Type:     "client",
				Label:    "Global Anycast Edge",
				SubLabel: "mTLS HTTP/3 Client Ingress",
				X:        40,
				Y:        200,
				Width:    220,
				Height:   80,
				Color:    "#06b6d4",
				Author:   PrincipalBob,
			},
			{
				ID:       "node-router-us",
				Type:     "gateway",
				Label:    "us-west3 Envoy Mesh",
				SubLabel: "Salt Lake City L7 Ingress",
				X:        330,
				Y:        70,
				Width:    220,
				Height:   80,
				Color:    "#38bdf8",
				Author:   PrincipalBob,
			},
			{
				ID:       "node-router-eu",
				Type:     "gateway",
				Label:    "eu-central1 Envoy Mesh",
				SubLabel: "Frankfurt L7 Ingress",
				X:        330,
				Y:        330,
				Width:    220,
				Height:   80,
				Color:    "#818cf8",
				Author:   PrincipalBob,
			},
			{
				ID:       "node-wp-us",
				Type:     "service",
				Label:    "WorkerPool (wp-canvas-us)",
				SubLabel: "3 Pods x 3 Rooms = 9 Slots",
				X:        630,
				Y:        70,
				Width:    220,
				Height:   80,
				Color:    "#10b981",
				Author:   PrincipalBob,
			},
			{
				ID:       "node-wp-eu",
				Type:     "service",
				Label:    "WorkerPool (wp-canvas-eu)",
				SubLabel: "gVisor Actor Sandbox Pods",
				X:        630,
				Y:        330,
				Width:    220,
				Height:   80,
				Color:    "#10b981",
				Author:   PrincipalBob,
			},
			{
				ID:       "node-spanner",
				Type:     "database",
				Label:    "GCS + Spanner Snapshot Store",
				SubLabel: "Multi-Region Memory Checkpoints",
				X:        930,
				Y:        200,
				Width:    220,
				Height:   80,
				Color:    "#f59e0b",
				Author:   PrincipalBob,
			},
		},
		DefaultEdges: []CanvasEdge{
			{
				ID:     "edge-1",
				From:   "node-clients",
				To:     "node-router-us",
				Label:  "Geo-DNS US",
				Style:  "solid",
				Author: PrincipalBob,
			},
			{
				ID:     "edge-2",
				From:   "node-clients",
				To:     "node-router-eu",
				Label:  "Geo-DNS EU",
				Style:  "solid",
				Author: PrincipalBob,
			},
			{
				ID:     "edge-3",
				From:   "node-router-us",
				To:     "node-wp-us",
				Label:  "Wake-on-HTTP",
				Style:  "solid",
				Author: PrincipalBob,
			},
			{
				ID:     "edge-4",
				From:   "node-router-eu",
				To:     "node-wp-eu",
				Label:  "Wake-on-HTTP",
				Style:  "solid",
				Author: PrincipalBob,
			},
			{
				ID:     "edge-5",
				From:   "node-wp-us",
				To:     "node-spanner",
				Label:  "Async Snapshot",
				Style:  "dashed",
				Author: PrincipalBob,
			},
			{
				ID:     "edge-6",
				From:   "node-wp-eu",
				To:     "node-spanner",
				Label:  "Async Snapshot",
				Style:  "dashed",
				Author: PrincipalBob,
			},
		},
		DefaultNotes: []CanvasNote{
			{
				ID:     "note-target",
				Text:   "Q4 GA Target: <100ms p99 cross-pod restore via Golden Snapshots with zero cold-start latency.",
				X:      40,
				Y:      30,
				Width:  210,
				Height: 100,
				Color:  "#bbf7d0",
				Author: PrincipalAlice,
			},
			{
				ID:     "note-rfc",
				Text:   "Bob's RFC: Enforce OpenFGA AccessPolicy at Envoy L7 perimeter before waking sleeping actors.",
				X:      930,
				Y:      350,
				Width:  210,
				Height: 100,
				Color:  "#bae6fd",
				Author: PrincipalBob,
			},
		},
	},
	{
		DocID:                  DocIncidentPostmortem,
		Title:                  "Sev-1 Incident Postmortem & DNS Cascade RCA",
		Subtitle:               "Third-Party Security Audit & Root Cause Analysis Timeline",
		SecurityClassification: "EXTERNAL AUDIT // POSTMORTEM",
		InitialOwner:           PrincipalAlice,
		InitialEditor:          PrincipalBob,
		InitialViewer:          PrincipalCharlie,
		DefaultNodes: []CanvasNode{
			{
				ID:       "node-alert",
				Type:     "milestone",
				Label:    "02:14 UTC: PagerDuty Sev-1",
				SubLabel: "5xx Upstream Latency Spike",
				X:        40,
				Y:        50,
				Width:    220,
				Height:   80,
				Color:    "#ef4444",
				Author:   PrincipalBob,
			},
			{
				ID:       "node-dns",
				Type:     "service",
				Label:    "02:19 UTC: CoreDNS Drop (3.2%)",
				SubLabel: "UDP Conntrack Table Exhaustion",
				X:        310,
				Y:        145,
				Width:    220,
				Height:   80,
				Color:    "#f97316",
				Author:   PrincipalBob,
			},
			{
				ID:       "node-retry",
				Type:     "queue",
				Label:    "02:26 UTC: Retry Storm (4.2x)",
				SubLabel: "Client Timeout Amplification",
				X:        590,
				Y:        145,
				Width:    220,
				Height:   80,
				Color:    "#eab308",
				Author:   PrincipalBob,
			},
			{
				ID:       "node-circuit",
				Type:     "gateway",
				Label:    "02:33 UTC: Circuit Breaker Open",
				SubLabel: "Shed Non-Critical Batch Load",
				X:        430,
				Y:        320,
				Width:    220,
				Height:   80,
				Color:    "#8b5cf6",
				Author:   PrincipalBob,
			},
			{
				ID:       "node-remedy",
				Type:     "milestone",
				Label:    "02:41 UTC: TCP Fallback Active",
				SubLabel: "All Clusters Recovered (0% Err)",
				X:        750,
				Y:        320,
				Width:    220,
				Height:   80,
				Color:    "#10b981",
				Author:   PrincipalBob,
			},
		},
		DefaultEdges: []CanvasEdge{
			{
				ID:     "edge-1",
				From:   "node-alert",
				To:     "node-dns",
				Label:  "Triage Trace",
				Style:  "solid",
				Author: PrincipalBob,
			},
			{
				ID:     "edge-2",
				From:   "node-dns",
				To:     "node-retry",
				Label:  "Timeout Flood",
				Style:  "dashed",
				Author: PrincipalBob,
			},
			{
				ID:     "edge-3",
				From:   "node-retry",
				To:     "node-circuit",
				Label:  "Load Shedding",
				Style:  "dashed",
				Author: PrincipalBob,
			},
			{
				ID:     "edge-4",
				From:   "node-circuit",
				To:     "node-remedy",
				Label:  "DNS-over-TCP Patch",
				Style:  "solid",
				Author: PrincipalBob,
			},
		},
		DefaultNotes: []CanvasNote{
			{
				ID:     "note-contractor",
				Text:   "Third-party audit review (Charlie Vance): Verified DNS timeout bounds & confirmed zero data exfiltration.",
				X:      60,
				Y:      290,
				Width:  210,
				Height: 100,
				Color:  "#e9d5ff",
				Author: PrincipalCharlie,
			},
			{
				ID:     "note-action",
				Text:   "Action Item #14 (Bob): Enforce ActorEgressPolicy DNS rate caps before Q4 production freeze.",
				X:      750,
				Y:      35,
				Width:  210,
				Height: 100,
				Color:  "#fef08a",
				Author: PrincipalBob,
			},
		},
	},
}
