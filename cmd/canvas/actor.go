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
	"os"
	"slices"
	"strings"
	"sync"
	"time"
)

// CanvasActor is an in-memory collaborative document room server.
// Notice: There is ZERO JWT middleware, ZERO user tables, and ZERO RBAC checks
// inside this binary. All authorization is handled at the Substrate perimeter
// before this container is even woken from GCS.
type CanvasActor struct {
	mu              sync.RWMutex
	docID           string
	title           string
	classification  string
	nodes           map[string]CanvasNode
	edges           map[string]CanvasEdge
	notes           map[string]CanvasNote
	revision        int
	history         []HistoryEntry
	collaborators   map[string]Collaborator
	currentPodIP    string
	podIPHistory    []string
	processBootedAt time.Time
	wakeCount       int
	queryCount      int
	state           string
}

// NewCanvasActor creates a new in-memory canvas actor for the specified document ID.
func NewCanvasActor(docID string) *CanvasActor {
	podIP := resolvePodIP()
	bootTime := time.Now()

	actor := &CanvasActor{
		docID:           docID,
		nodes:           make(map[string]CanvasNode),
		edges:           make(map[string]CanvasEdge),
		notes:           make(map[string]CanvasNote),
		collaborators:   make(map[string]Collaborator),
		currentPodIP:    podIP,
		podIPHistory:    []string{podIP},
		processBootedAt: bootTime,
		wakeCount:       1,
		queryCount:      0,
		state:           ActorStateRunning,
	}

	actor.seedFromCatalog(docID)
	return actor
}

func resolvePodIP() string {
	if ip := os.Getenv("POD_IP"); ip != "" {
		return ip
	}
	if ip := os.Getenv("HOST_IP"); ip != "" {
		return ip
	}
	addrs, err := net.InterfaceAddrs()
	if err == nil {
		for _, addr := range addrs {
			if ipnet, ok := addr.(*net.IPNet); ok && !ipnet.IP.IsLoopback() {
				if ipnet.IP.To4() != nil {
					return ipnet.IP.String()
				}
			}
		}
	}
	return "127.0.0.1"
}

func (a *CanvasActor) seedFromCatalog(docID string) {
	var catalogEntry *DocumentCatalogEntry
	for _, entry := range DefaultDocumentCatalog {
		if entry.DocID == docID {
			catalogEntry = &entry
			break
		}
	}

	if catalogEntry == nil {
		a.title = fmt.Sprintf("Document: %s", docID)
		a.classification = "INTERNAL"
		return
	}

	a.title = catalogEntry.Title
	a.classification = catalogEntry.SecurityClassification

	for _, n := range catalogEntry.DefaultNodes {
		n.UpdatedAt = a.processBootedAt
		a.nodes[n.ID] = n
	}
	for _, e := range catalogEntry.DefaultEdges {
		e.UpdatedAt = a.processBootedAt
		a.edges[e.ID] = e
	}
	for _, n := range catalogEntry.DefaultNotes {
		n.UpdatedAt = a.processBootedAt
		a.notes[n.ID] = n
	}

	a.revision = 1
	a.history = []HistoryEntry{
		{
			Revision:  1,
			Timestamp: a.processBootedAt,
			Action:    "Document Initialized",
			Author:    catalogEntry.InitialOwner,
			Detail:    fmt.Sprintf("Loaded %d nodes, %d edges, %d notes into Go heap RAM", len(a.nodes), len(a.edges), len(a.notes)),
		},
	}
}

// BuildSceneGraph constructs the snapshot representation of the current document in heap RAM.
func (a *CanvasActor) BuildSceneGraph() SceneGraph {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.buildSceneGraphLocked()
}

func (a *CanvasActor) buildSceneGraphLocked() SceneGraph {
	var nodes []CanvasNode
	for _, n := range a.nodes {
		nodes = append(nodes, n)
	}
	slices.SortFunc(nodes, func(i, j CanvasNode) int {
		if i.Y != j.Y {
			return i.Y - j.Y
		}
		return i.X - j.X
	})

	var edges []CanvasEdge
	for _, e := range a.edges {
		edges = append(edges, e)
	}
	slices.SortFunc(edges, func(i, j CanvasEdge) int {
		return len(i.ID) - len(j.ID)
	})

	var notes []CanvasNote
	for _, n := range a.notes {
		notes = append(notes, n)
	}
	slices.SortFunc(notes, func(i, j CanvasNote) int {
		return i.Y - j.Y
	})

	var collabs []Collaborator
	for _, c := range a.collaborators {
		collabs = append(collabs, c)
	}

	var hist []HistoryEntry
	hist = append(hist, a.history...)

	objCount := len(nodes) + len(edges) + len(notes)
	// Approximate RAM footprint: struct headers + string buffers + slice storage
	ramEstimate := int64(objCount*512 + len(hist)*256 + 16384)

	return SceneGraph{
		DocID:                  a.docID,
		Title:                  a.title,
		SecurityClassification: a.classification,
		Revision:               a.revision,
		Nodes:                  nodes,
		Edges:                  edges,
		Notes:                  notes,
		History:                hist,
		Collaborators:          collabs,
		RAMObjectCount:         objCount,
		RAMBytesEstimate:       ramEstimate,
		CurrentPodIP:           a.currentPodIP,
		PodIPHistory:           a.podIPHistory,
		ProcessBootedAt:        a.processBootedAt.Format(time.RFC3339),
		WakeCount:              a.wakeCount,
		QueryCount:             a.queryCount,
		ActorState:             a.state,
	}
}

// Mutate applies a canvas operation directly to the Go heap scene graph.
func (a *CanvasActor) Mutate(op CanvasOp) (SceneGraph, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.queryCount++
	a.revision++
	now := time.Now()

	author := op.Principal
	if author == "" {
		author = "unknown"
	}

	// Update collaborator presence
	a.collaborators[author] = Collaborator{
		Principal: author,
		Name:      author,
		LastSeen:  now,
		IsActive:  true,
	}

	switch op.Op {
	case "add_node":
		if op.Node == nil || op.Node.ID == "" {
			return SceneGraph{}, fmt.Errorf("node or node id cannot be empty")
		}
		node := *op.Node
		node.Author = author
		node.UpdatedAt = now
		if node.Width == 0 {
			node.Width = 210
		}
		if node.Height == 0 {
			node.Height = 80
		}
		a.nodes[node.ID] = node
		a.history = append(a.history, HistoryEntry{
			Revision:  a.revision,
			Timestamp: now,
			Action:    "Node Added",
			Author:    author,
			Detail:    fmt.Sprintf("Added architecture node '%s' (%s)", node.Label, node.Type),
		})

	case "move_node":
		if op.Move == nil || op.Move.ID == "" {
			return SceneGraph{}, fmt.Errorf("move payload missing target node id")
		}
		node, ok := a.nodes[op.Move.ID]
		if !ok {
			return SceneGraph{}, fmt.Errorf("node %s not found in scene graph", op.Move.ID)
		}
		oldX, oldY := node.X, node.Y
		node.X = op.Move.X
		node.Y = op.Move.Y
		node.UpdatedAt = now
		a.nodes[node.ID] = node
		a.history = append(a.history, HistoryEntry{
			Revision:  a.revision,
			Timestamp: now,
			Action:    "Node Moved",
			Author:    author,
			Detail:    fmt.Sprintf("Moved '%s' from (%d, %d) to (%d, %d)", node.Label, oldX, oldY, node.X, node.Y),
		})

	case "add_note":
		if op.Note == nil || op.Note.ID == "" {
			return SceneGraph{}, fmt.Errorf("note or note id cannot be empty")
		}
		note := *op.Note
		note.Author = author
		note.UpdatedAt = now
		if note.Width == 0 {
			note.Width = 210
		}
		if note.Height == 0 {
			note.Height = 100
		}
		a.notes[note.ID] = note
		a.history = append(a.history, HistoryEntry{
			Revision:  a.revision,
			Timestamp: now,
			Action:    "Sticky Note Added",
			Author:    author,
			Detail:    fmt.Sprintf("Added note: \"%s\"", truncateStr(note.Text, 40)),
		})

	case "add_edge":
		if op.Edge == nil || op.Edge.ID == "" {
			return SceneGraph{}, fmt.Errorf("edge or edge id cannot be empty")
		}
		edge := *op.Edge
		edge.Author = author
		edge.UpdatedAt = now
		a.edges[edge.ID] = edge
		a.history = append(a.history, HistoryEntry{
			Revision:  a.revision,
			Timestamp: now,
			Action:    "Edge Connected",
			Author:    author,
			Detail:    fmt.Sprintf("Connected '%s' -> '%s' (%s)", edge.From, edge.To, edge.Label),
		})

	case "delete":
		if op.Delete == nil || op.Delete.ID == "" {
			return SceneGraph{}, fmt.Errorf("delete payload missing target id")
		}
		targetID := op.Delete.ID
		switch op.Delete.TargetType {
		case "node":
			delete(a.nodes, targetID)
			// Remove associated edges
			for eID, e := range a.edges {
				if e.From == targetID || e.To == targetID {
					delete(a.edges, eID)
				}
			}
			a.history = append(a.history, HistoryEntry{
				Revision:  a.revision,
				Timestamp: now,
				Action:    "Node Deleted",
				Author:    author,
				Detail:    fmt.Sprintf("Deleted node '%s' and related edges", targetID),
			})
		case "edge":
			delete(a.edges, targetID)
			a.history = append(a.history, HistoryEntry{
				Revision:  a.revision,
				Timestamp: now,
				Action:    "Edge Deleted",
				Author:    author,
				Detail:    fmt.Sprintf("Deleted edge '%s'", targetID),
			})
		case "note":
			delete(a.notes, targetID)
			a.history = append(a.history, HistoryEntry{
				Revision:  a.revision,
				Timestamp: now,
				Action:    "Note Deleted",
				Author:    author,
				Detail:    fmt.Sprintf("Deleted sticky note '%s'", targetID),
			})
		default:
			return SceneGraph{}, fmt.Errorf("unknown target type: %s", op.Delete.TargetType)
		}

	case "reset":
		a.nodes = make(map[string]CanvasNode)
		a.edges = make(map[string]CanvasEdge)
		a.notes = make(map[string]CanvasNote)
		a.seedFromCatalog(a.docID)
		a.history = append(a.history, HistoryEntry{
			Revision:  a.revision,
			Timestamp: now,
			Action:    "Document Reset",
			Author:    author,
			Detail:    "Reset document scene graph to seed state",
		})

	default:
		return SceneGraph{}, fmt.Errorf("unknown operation: %s", op.Op)
	}

	// Keep history bounded to 50 items
	if len(a.history) > 50 {
		a.history = a.history[len(a.history)-50:]
	}

	return a.buildSceneGraphLocked(), nil
}

// Suspend sets the actor state to SUSPENDED.
func (a *CanvasActor) Suspend() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.state = ActorStateSuspended
}

// Wake simulates resuming the actor, optionally registering a new worker pod IP.
func (a *CanvasActor) Wake(newPodIP string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.state = ActorStateRunning
	a.wakeCount++
	if newPodIP != "" && newPodIP != a.currentPodIP {
		a.currentPodIP = newPodIP
		a.podIPHistory = append(a.podIPHistory, newPodIP)
	}
}

// BindDocID ensures that an actor cloned from the shared ActorTemplate/canvas
// Golden Snapshot re-seeds its in-memory catalog to match its assigned document ID
// on first request, and refreshes its worker pod IP across cross-pod migrations.
func (a *CanvasActor) BindDocID(docID string) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if ip := resolvePodIP(); ip != "" && ip != a.currentPodIP {
		a.currentPodIP = ip
		if len(a.podIPHistory) == 0 || a.podIPHistory[len(a.podIPHistory)-1] != ip {
			a.podIPHistory = append(a.podIPHistory, ip)
		}
	}

	if docID == "" || docID == a.docID {
		return
	}
	a.docID = docID
	a.nodes = make(map[string]CanvasNode)
	a.edges = make(map[string]CanvasEdge)
	a.notes = make(map[string]CanvasNote)
	a.collaborators = make(map[string]Collaborator)
	a.seedFromCatalog(docID)
}

func requestedDocID(r *http.Request) string {
	if qdoc := strings.TrimSpace(r.URL.Query().Get("doc")); qdoc != "" {
		return qdoc
	}
	if hdoc := strings.TrimSpace(r.Header.Get("X-Doc-ID")); hdoc != "" {
		return hdoc
	}
	if target := strings.TrimSpace(r.Header.Get("ate-target-actor")); target != "" {
		if idx := strings.LastIndexByte(target, '/'); idx >= 0 && idx+1 < len(target) {
			return target[idx+1:]
		}
		return target
	}
	return ""
}

func truncateStr(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

// writeJSON formats and sends pretty-printed JSON as required by talk conventions.
func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(data)
}

// newActorMux sets up the HTTP router for the actor.
// Notice again: ZERO auth middleware! The actor responds directly to all endpoints.
func newActorMux(actor *CanvasActor) *http.ServeMux {
	mux := http.NewServeMux()

	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})

	mux.HandleFunc("/canvas", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		actor.BindDocID(requestedDocID(r))
		actor.mu.Lock()
		actor.queryCount++
		actor.mu.Unlock()
		writeJSON(w, http.StatusOK, actor.BuildSceneGraph())
	})

	mux.HandleFunc("/canvas/node", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		actor.BindDocID(requestedDocID(r))
		var node CanvasNode
		if err := json.NewDecoder(r.Body).Decode(&node); err != nil {
			http.Error(w, fmt.Sprintf("invalid json: %v", err), http.StatusBadRequest)
			return
		}
		author := r.Header.Get("X-Principal")
		if author == "" {
			author = node.Author
		}
		sg, err := actor.Mutate(CanvasOp{
			Op:        "add_node",
			Principal: author,
			Node:      &node,
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, http.StatusOK, sg)
	})

	mux.HandleFunc("/canvas/move", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		actor.BindDocID(requestedDocID(r))
		var move MoveNodePayload
		if err := json.NewDecoder(r.Body).Decode(&move); err != nil {
			http.Error(w, fmt.Sprintf("invalid json: %v", err), http.StatusBadRequest)
			return
		}
		author := r.Header.Get("X-Principal")
		sg, err := actor.Mutate(CanvasOp{
			Op:        "move_node",
			Principal: author,
			Move:      &move,
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, http.StatusOK, sg)
	})

	mux.HandleFunc("/canvas/note", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		actor.BindDocID(requestedDocID(r))
		var note CanvasNote
		if err := json.NewDecoder(r.Body).Decode(&note); err != nil {
			http.Error(w, fmt.Sprintf("invalid json: %v", err), http.StatusBadRequest)
			return
		}
		author := r.Header.Get("X-Principal")
		if author == "" {
			author = note.Author
		}
		sg, err := actor.Mutate(CanvasOp{
			Op:        "add_note",
			Principal: author,
			Note:      &note,
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, http.StatusOK, sg)
	})

	mux.HandleFunc("/canvas/edge", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		actor.BindDocID(requestedDocID(r))
		var edge CanvasEdge
		if err := json.NewDecoder(r.Body).Decode(&edge); err != nil {
			http.Error(w, fmt.Sprintf("invalid json: %v", err), http.StatusBadRequest)
			return
		}
		author := r.Header.Get("X-Principal")
		if author == "" {
			author = edge.Author
		}
		sg, err := actor.Mutate(CanvasOp{
			Op:        "add_edge",
			Principal: author,
			Edge:      &edge,
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, http.StatusOK, sg)
	})

	mux.HandleFunc("/canvas/delete", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		actor.BindDocID(requestedDocID(r))
		var del DeletePayload
		if err := json.NewDecoder(r.Body).Decode(&del); err != nil {
			http.Error(w, fmt.Sprintf("invalid json: %v", err), http.StatusBadRequest)
			return
		}
		author := r.Header.Get("X-Principal")
		sg, err := actor.Mutate(CanvasOp{
			Op:        "delete",
			Principal: author,
			Delete:    &del,
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, http.StatusOK, sg)
	})

	mux.HandleFunc("/canvas/reset", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		actor.BindDocID(requestedDocID(r))
		author := r.Header.Get("X-Principal")
		sg, err := actor.Mutate(CanvasOp{
			Op:        "reset",
			Principal: author,
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, http.StatusOK, sg)
	})

	mux.HandleFunc("/suspend", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		actor.Suspend()
		writeJSON(w, http.StatusOK, map[string]string{
			"status":   "suspended",
			"actor_id": actor.docID,
		})
	})

	return mux
}
