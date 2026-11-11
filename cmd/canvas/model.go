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
				Type:     "service",
				Label:    "Acme Cloud AI (Target)",
				SubLabel: "Proprietary Model Weights",
				X:        80,
				Y:        120,
				Width:    210,
				Height:   80,
				Color:    "#ef4444",
				Author:   PrincipalAlice,
			},
			{
				ID:       "node-escrow",
				Type:     "database",
				Label:    "Sovereign Escrow Vault",
				SubLabel: "$2.4B Tranche Allocation",
				X:        380,
				Y:        120,
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
				X:        690,
				Y:        120,
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
				X:        380,
				Y:        300,
				Width:    220,
				Height:   80,
				Color:    "#38bdf8",
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
				Label:  "IP & Data Sync",
				Style:  "solid",
				Author: PrincipalAlice,
			},
		},
		DefaultNotes: []CanvasNote{
			{
				ID:     "note-secret",
				Text:   "DO NOT DISTRIBUTE - Board & Executive Eyes Only. Deal terms pending clearance.",
				X:      80,
				Y:      290,
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
		Subtitle:               "High-Scale Agent Substrate Runtime Architecture",
		SecurityClassification: "INTERNAL // ARCHITECTURE",
		InitialOwner:           PrincipalAlice,
		InitialEditor:          PrincipalBob,
		InitialViewer:          "",
		DefaultNodes: []CanvasNode{
			{
				ID:       "node-clients",
				Type:     "client",
				Label:    "Global Multi-Region Edge",
				SubLabel: "mTLS HTTP/2 Clients",
				X:        80,
				Y:        130,
				Width:    210,
				Height:   80,
				Color:    "#06b6d4",
				Author:   PrincipalBob,
			},
			{
				ID:       "node-router",
				Type:     "gateway",
				Label:    "Envoy L7 Mesh Router",
				SubLabel: "atenet-router Ingress",
				X:        380,
				Y:        130,
				Width:    210,
				Height:   80,
				Color:    "#8b5cf6",
				Author:   PrincipalBob,
			},
			{
				ID:       "node-workers",
				Type:     "service",
				Label:    "WorkerPool (wp-canvas)",
				SubLabel: "3 Pods x 3 Rooms = 9 Slots",
				X:        680,
				Y:        130,
				Width:    220,
				Height:   80,
				Color:    "#10b981",
				Author:   PrincipalBob,
			},
			{
				ID:       "node-spanner",
				Type:     "database",
				Label:    "Cloud Spanner KV",
				SubLabel: "Multi-Region Replicated",
				X:        680,
				Y:        310,
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
				To:     "node-router",
				Label:  "gRPC mTLS",
				Style:  "solid",
				Author: PrincipalBob,
			},
			{
				ID:     "edge-2",
				From:   "node-router",
				To:     "node-workers",
				Label:  "Wake-on-HTTP",
				Style:  "solid",
				Author: PrincipalBob,
			},
			{
				ID:     "edge-3",
				From:   "node-workers",
				To:     "node-spanner",
				Label:  "State Commit",
				Style:  "dashed",
				Author: PrincipalBob,
			},
		},
		DefaultNotes: []CanvasNote{
			{
				ID:     "note-target",
				Text:   "Target GA Launch: ATECon Nov 2026. Zero cold starts via Golden Snapshots.",
				X:      80,
				Y:      300,
				Width:  210,
				Height: 100,
				Color:  "#bbf7d0",
				Author: PrincipalAlice,
			},
		},
	},
	{
		DocID:                  DocIncidentPostmortem,
		Title:                  "Sev-1 Incident Postmortem & DNS Cascade RCA",
		Subtitle:               "Third-Party Security Audit & Root Cause Analysis",
		SecurityClassification: "INTERNAL // POSTMORTEM",
		InitialOwner:           PrincipalAlice,
		InitialEditor:          PrincipalBob,
		InitialViewer:          PrincipalCharlie,
		DefaultNodes: []CanvasNode{
			{
				ID:       "node-alert",
				Type:     "milestone",
				Label:    "02:14 UTC: PagerDuty Alert",
				SubLabel: "High Upstream Latency",
				X:        80,
				Y:        130,
				Width:    210,
				Height:   80,
				Color:    "#ef4444",
				Author:   PrincipalBob,
			},
			{
				ID:       "node-dns",
				Type:     "service",
				Label:    "CoreDNS Packet Drop (3.2%)",
				SubLabel: "UDP Buffer Exhaustion",
				X:        380,
				Y:        130,
				Width:    220,
				Height:   80,
				Color:    "#f97316",
				Author:   PrincipalBob,
			},
			{
				ID:       "node-circuit",
				Type:     "gateway",
				Label:    "Circuit Breaker Tripped",
				SubLabel: "4 Cluster Cascades",
				X:        690,
				Y:        130,
				Width:    210,
				Height:   80,
				Color:    "#eab308",
				Author:   PrincipalBob,
			},
			{
				ID:       "node-remedy",
				Type:     "milestone",
				Label:    "02:41 UTC: Fallback Active",
				SubLabel: "TCP Retries & Local Cache",
				X:        380,
				Y:        310,
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
				Label:  "Triage Notification",
				Style:  "solid",
				Author: PrincipalBob,
			},
			{
				ID:     "edge-2",
				From:   "node-dns",
				To:     "node-circuit",
				Label:  "Upstream Timeouts",
				Style:  "dashed",
				Author: PrincipalBob,
			},
			{
				ID:     "edge-3",
				From:   "node-circuit",
				To:     "node-remedy",
				Label:  "Manual Patch",
				Style:  "solid",
				Author: PrincipalBob,
			},
		},
		DefaultNotes: []CanvasNote{
			{
				ID:     "note-contractor",
				Text:   "Third-party audit review (Charlie Vance): Verified DNS timeout limits and retry jitter.",
				X:      80,
				Y:      300,
				Width:  210,
				Height: 100,
				Color:  "#e0e7ff",
				Author: PrincipalCharlie,
			},
		},
	},
}
