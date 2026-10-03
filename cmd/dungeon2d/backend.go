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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
)

type SessionRecord struct {
	ActorName         string       `json:"actor_name"`
	Atespace          string       `json:"atespace"`
	Template          string       `json:"template"`
	State             string       `json:"state"`
	SnapshotURI       string       `json:"snapshot_uri"`
	SnapshotVersion   int          `json:"snapshot_version"`
	LastWakeMs        int64        `json:"last_wake_ms"`
	SaveCount         int          `json:"save_count"`
	RevertCount       int          `json:"revert_count"`
	TimelineParent    string       `json:"timeline_parent,omitempty"`
	LastTag           string       `json:"last_tag,omitempty"`
	AssignedWorkerIP  string       `json:"assigned_worker_ip,omitempty"`
	AssignedWorkerPod string       `json:"assigned_worker_pod,omitempty"`
	LastMessage       string       `json:"last_message"`
	LastEventType     string       `json:"last_event_type"`
	UpdatedAt         time.Time    `json:"updated_at"`
	Session           ActorSession `json:"session"`
}

type WorkerSlot struct {
	Index        int       `json:"index"`
	WorkerPod    string    `json:"worker_pod"`
	IP           string    `json:"ip"`
	NodeName     string    `json:"node_name"`
	State        string    `json:"state"`
	ActiveActor  string    `json:"active_actor,omitempty"`
	ActiveHero   string    `json:"active_hero,omitempty"`
	ActiveAction string    `json:"active_action,omitempty"`
	BusyUntil    time.Time `json:"busy_until"`
	WakeCount    int       `json:"wake_count"`
}

type TelemetryEvent struct {
	ID          int64     `json:"id"`
	Timestamp   time.Time `json:"timestamp"`
	Type        string    `json:"type"`
	ActorName   string    `json:"actor_name"`
	HeroName    string    `json:"hero_name"`
	WorkerIP    string    `json:"worker_ip,omitempty"`
	WorkerPod   string    `json:"worker_pod,omitempty"`
	LatencyMs   int64     `json:"latency_ms,omitempty"`
	SnapshotURI string    `json:"snapshot_uri,omitempty"`
	Floor       int       `json:"floor"`
	HP          int       `json:"hp"`
	Message     string    `json:"message"`
}

type DashboardStats struct {
	TotalSessions   int `json:"total_sessions"`
	RunningActors   int `json:"running_actors"`
	SuspendedActors int `json:"suspended_actors"`
	DeadActors      int `json:"dead_actors"`
	Victories       int `json:"victories"`
	TotalSaves      int `json:"total_saves"`
	TotalReverts    int `json:"total_reverts"`
	TotalForks      int `json:"total_forks"`
	WorkerCapacity  int `json:"worker_capacity"`
}

type DashboardSnapshot struct {
	Mode      string           `json:"mode"`
	Atespace  string           `json:"atespace"`
	Template  string           `json:"template"`
	PublicURL string           `json:"public_url"`
	Workers   []WorkerSlot     `json:"workers"`
	Sessions  []SessionRecord  `json:"sessions"`
	Events    []TelemetryEvent `json:"events"`
	Stats     DashboardStats   `json:"stats"`
}

type SubstrateBackend interface {
	StartSession(ctx context.Context, heroName, charClass string) (SessionRecord, error)
	GetSession(ctx context.Context, actorName string) (SessionRecord, error)
	ExecuteMilestone(ctx context.Context, actorName string, req MutateRequest) (SessionRecord, error)
	RevertSession(ctx context.Context, actorName string) (SessionRecord, error)
	ForkSession(ctx context.Context, sourceActor, newActor string) (SessionRecord, error)
	DeleteSession(ctx context.Context, actorName string) error
	DashboardState(ctx context.Context) (DashboardSnapshot, error)
	SubscribeEvents() (chan TelemetryEvent, func())
}

var nonSlugRe = regexp.MustCompile(`[^a-z0-9-]+`)

func sanitizeActorName(heroName string) string {
	clean := strings.ToLower(strings.TrimSpace(heroName))
	clean = nonSlugRe.ReplaceAllString(clean, "-")
	clean = strings.Trim(clean, "-")
	if clean == "" {
		clean = "hero"
	}
	if !strings.HasPrefix(clean, "d2d-") {
		clean = "d2d-" + clean
	}
	if len(clean) > 42 {
		clean = clean[:42]
	}
	return clean
}

type eventHub struct {
	mu          sync.Mutex
	nextID      int64
	events      []TelemetryEvent
	subscribers map[chan TelemetryEvent]struct{}
	totalSaves  int
	totalRevert int
	totalForks  int
}

func newEventHub() *eventHub {
	return &eventHub{
		subscribers: make(map[chan TelemetryEvent]struct{}),
	}
}

func (h *eventHub) publish(ev TelemetryEvent) {
	h.mu.Lock()
	h.nextID++
	ev.ID = h.nextID
	if ev.Timestamp.IsZero() {
		ev.Timestamp = time.Now().UTC()
	}
	switch ev.Type {
	case "GCS_COMMIT", "QUICKSAVE":
		h.totalSaves++
	case "REVERT":
		h.totalRevert++
	case "FORK":
		h.totalForks++
	}
	h.events = append([]TelemetryEvent{ev}, h.events...)
	if len(h.events) > 80 {
		h.events = h.events[:80]
	}
	subs := make([]chan TelemetryEvent, 0, len(h.subscribers))
	for ch := range h.subscribers {
		subs = append(subs, ch)
	}
	h.mu.Unlock()

	for _, ch := range subs {
		select {
		case ch <- ev:
		default:
		}
	}
}

func (h *eventHub) subscribe() (chan TelemetryEvent, func()) {
	ch := make(chan TelemetryEvent, 32)
	h.mu.Lock()
	h.subscribers[ch] = struct{}{}
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		delete(h.subscribers, ch)
		h.mu.Unlock()
	}
}

func (h *eventHub) snapshot() ([]TelemetryEvent, int, int, int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]TelemetryEvent, len(h.events))
	copy(out, h.events)
	return out, h.totalSaves, h.totalRevert, h.totalForks
}

// LocalSimulatorBackend emulates the 3-replica Agent Substrate WorkerPool,
// GCS FULL heap snapshots, revert, and tag-cloning in memory for unit tests
// and offline local demos.
type LocalSimulatorBackend struct {
	mu            sync.Mutex
	atespace      string
	template      string
	workers       []WorkerSlot
	rrIndex       int
	liveActors    map[string]*ActorSession
	gcsSnapshots  map[string]*ActorSession
	tagSnapshots  map[string]*ActorSession
	records       map[string]*SessionRecord
	hub           *eventHub
}

func NewLocalSimulatorBackend(atespace, template string) *LocalSimulatorBackend {
	return &LocalSimulatorBackend{
		atespace: atespace,
		template: template,
		workers: []WorkerSlot{
			{Index: 1, WorkerPod: "anything-but-agents-5699564879-fn4f8", IP: "10.120.1.22", NodeName: "gke-substrate-node-4qr7", State: "WORKER_STATE_ACTIVE"},
			{Index: 2, WorkerPod: "anything-but-agents-5699564879-gf8q9", IP: "10.120.0.11", NodeName: "gke-substrate-node-68v0", State: "WORKER_STATE_ACTIVE"},
			{Index: 3, WorkerPod: "anything-but-agents-5699564879-tgspw", IP: "10.120.1.23", NodeName: "gke-substrate-node-4qr7", State: "WORKER_STATE_ACTIVE"},
		},
		liveActors:   make(map[string]*ActorSession),
		gcsSnapshots: make(map[string]*ActorSession),
		tagSnapshots: make(map[string]*ActorSession),
		records:      make(map[string]*SessionRecord),
		hub:          newEventHub(),
	}
}

func (b *LocalSimulatorBackend) pickWorkerLocked(actorName, heroName, action string) *WorkerSlot {
	slot := &b.workers[b.rrIndex%len(b.workers)]
	b.rrIndex++
	slot.ActiveActor = actorName
	slot.ActiveHero = heroName
	slot.ActiveAction = action
	slot.BusyUntil = time.Now().UTC().Add(900 * time.Millisecond)
	slot.WakeCount++
	return slot
}

func (b *LocalSimulatorBackend) StartSession(ctx context.Context, heroName, charClass string) (SessionRecord, error) {
	actorName := sanitizeActorName(heroName)
	b.mu.Lock()
	if charClass == "" {
		charClass = "paladin"
	}
	actor := NewActorSession()
	slot := b.pickWorkerLocked(actorName, heroName, "init")
	resp := actor.Mutate(MutateRequest{
		Action:         "init",
		HeroName:       heroName,
		CharacterClass: charClass,
	}, slot.IP)

	b.liveActors[actorName] = actor
	b.gcsSnapshots[actorName] = actor.Clone()
	snapURI := fmt.Sprintf("gs://ate-snapshots/%s/%s/actors/%s/snapshots/v1", b.atespace, b.template, actorName)
	tagName := fmt.Sprintf("tag-%s-v1", actorName)
	b.tagSnapshots[tagName] = actor.Clone()

	rec := &SessionRecord{
		ActorName:         actorName,
		Atespace:          b.atespace,
		Template:          b.template,
		State:             "ACTOR_STATE_SUSPENDED",
		SnapshotURI:       snapURI,
		SnapshotVersion:   1,
		LastWakeMs:        92,
		SaveCount:         1,
		LastTag:           tagName,
		AssignedWorkerIP:  slot.IP,
		AssignedWorkerPod: slot.WorkerPod,
		LastMessage:       resp.Message,
		LastEventType:     resp.EventType,
		UpdatedAt:         time.Now().UTC(),
		Session:           resp.Session,
	}
	b.records[actorName] = rec
	out := *rec
	b.mu.Unlock()

	b.hub.publish(TelemetryEvent{
		Type:        "GCS_COMMIT",
		ActorName:   actorName,
		HeroName:    out.Session.HeroName,
		WorkerIP:    slot.IP,
		WorkerPod:   slot.WorkerPod,
		LatencyMs:   92,
		SnapshotURI: snapURI,
		Floor:       out.Session.Floor,
		HP:          out.Session.HP,
		Message:     fmt.Sprintf("Created %s -> Woke on %s -> Initialized heap -> Suspended to GCS", actorName, slot.IP),
	})
	return out, nil
}

func (b *LocalSimulatorBackend) GetSession(_ context.Context, actorName string) (SessionRecord, error) {
	actorName = sanitizeActorName(actorName)
	b.mu.Lock()
	defer b.mu.Unlock()
	rec, ok := b.records[actorName]
	if !ok {
		return SessionRecord{}, fmt.Errorf("session %q not found", actorName)
	}
	return *rec, nil
}

func (b *LocalSimulatorBackend) ExecuteMilestone(_ context.Context, actorName string, req MutateRequest) (SessionRecord, error) {
	actorName = sanitizeActorName(actorName)
	b.mu.Lock()
	rec, ok := b.records[actorName]
	if !ok {
		b.mu.Unlock()
		var err error
		if _, err = b.StartSession(context.Background(), strings.TrimPrefix(actorName, "d2d-"), req.CharacterClass); err != nil {
			return SessionRecord{}, err
		}
		b.mu.Lock()
		rec = b.records[actorName]
	}

	// If the actor was suspended in GCS, hydrate from its GCS snapshot onto a free worker pod!
	actor := b.liveActors[actorName]
	if rec.State == "ACTOR_STATE_SUSPENDED" && b.gcsSnapshots[actorName] != nil {
		actor = b.gcsSnapshots[actorName].Clone()
		b.liveActors[actorName] = actor
	}

	slot := b.pickWorkerLocked(actorName, rec.Session.HeroName, req.Action)
	resp := actor.Mutate(req, slot.IP)
	latencyMs := int64(85 + (resp.Session.Turn%7)*9)

	rec.AssignedWorkerIP = slot.IP
	rec.AssignedWorkerPod = slot.WorkerPod
	rec.LastWakeMs = latencyMs
	rec.LastMessage = resp.Message
	rec.LastEventType = resp.EventType
	rec.UpdatedAt = time.Now().UTC()
	rec.Session = resp.Session

	evType := "GCS_COMMIT"
	if !resp.Session.Alive {
		// Crucial Substrate semantics: when the player dies in RAM, keep the actor
		// RUNNING (uncommitted) so `kubectl ate revert` rewinds to the last committed
		// GCS snapshot before the fatal attack!
		rec.State = "ACTOR_STATE_RUNNING"
		evType = "DEATH"
	} else {
		// Commit new heap snapshot to GCS and immediately suspend actor to free worker pod!
		rec.SnapshotVersion++
		rec.SaveCount++
		rec.State = "ACTOR_STATE_SUSPENDED"
		rec.SnapshotURI = fmt.Sprintf("gs://ate-snapshots/%s/%s/actors/%s/snapshots/v%d", b.atespace, b.template, actorName, rec.SnapshotVersion)
		b.gcsSnapshots[actorName] = actor.Clone()

		if req.Action == "quicksave" || resp.EventType == "FLOOR_CHANGE" {
			tagName := fmt.Sprintf("tag-%s-f%d-v%d", actorName, resp.Session.Floor, rec.SnapshotVersion)
			b.tagSnapshots[tagName] = actor.Clone()
			rec.LastTag = tagName
			if req.Action == "quicksave" {
				evType = "QUICKSAVE"
			}
		}
		if resp.Session.Victory {
			evType = "VICTORY"
		}
	}

	out := *rec
	b.mu.Unlock()

	b.hub.publish(TelemetryEvent{
		Type:        evType,
		ActorName:   actorName,
		HeroName:    out.Session.HeroName,
		WorkerIP:    slot.IP,
		WorkerPod   : slot.WorkerPod,
		LatencyMs:   latencyMs,
		SnapshotURI: out.SnapshotURI,
		Floor:       out.Session.Floor,
		HP:          out.Session.HP,
		Message:     resp.Message,
	})
	return out, nil
}

func (b *LocalSimulatorBackend) RevertSession(_ context.Context, actorName string) (SessionRecord, error) {
	actorName = sanitizeActorName(actorName)
	b.mu.Lock()
	rec, ok := b.records[actorName]
	if !ok {
		b.mu.Unlock()
		return SessionRecord{}, fmt.Errorf("actor %q not found", actorName)
	}

	slot := b.pickWorkerLocked(actorName, rec.Session.HeroName, "revert")
	var restored *ActorSession
	var snapResp ActorResponse

	if !rec.Session.Alive && b.gcsSnapshots[actorName] != nil && b.gcsSnapshots[actorName].Alive {
		// Player was dead (uncommitted RAM): restore last committed GCS snapshot directly
		restored = b.gcsSnapshots[actorName].Clone()
		snapResp = restored.Snapshot(slot.IP)
		rec.LastMessage = fmt.Sprintf("SAVE-SCUM SUCCESS (`kubectl ate revert actor %s`): discarded dead RAM and restored GCS snapshot v%d on Floor %d (HP: %d/%d)!",
			actorName, rec.SnapshotVersion, snapResp.Session.Floor, snapResp.Session.HP, snapResp.Session.MaxHP)
	} else if b.liveActors[actorName] != nil {
		// Player was alive: rewind actor heap to its last Quicksave / Floor-Entry Checkpoint
		restored = b.liveActors[actorName].Clone()
		snapResp = restored.Mutate(MutateRequest{Action: "restore_checkpoint"}, slot.IP)
		rec.LastMessage = snapResp.Message
	} else {
		restored = NewActorSession()
		snapResp = restored.Snapshot(slot.IP)
		rec.LastMessage = snapResp.Message
	}

	b.liveActors[actorName] = restored
	b.gcsSnapshots[actorName] = restored.Clone()

	rec.State = "ACTOR_STATE_SUSPENDED"
	rec.RevertCount++
	rec.AssignedWorkerIP = slot.IP
	rec.AssignedWorkerPod = slot.WorkerPod
	rec.LastWakeMs = 78
	rec.LastEventType = "REVERT"
	snapResp.Session.LastEvent = rec.LastMessage
	rec.Session = snapResp.Session
	rec.UpdatedAt = time.Now().UTC()
	out := *rec
	b.mu.Unlock()

	b.hub.publish(TelemetryEvent{
		Type:        "REVERT",
		ActorName:   actorName,
		HeroName:    out.Session.HeroName,
		WorkerIP:    slot.IP,
		WorkerPod:   slot.WorkerPod,
		LatencyMs:   78,
		SnapshotURI: out.SnapshotURI,
		Floor:       out.Session.Floor,
		HP:          out.Session.HP,
		Message:     out.LastMessage,
	})
	return out, nil
}

func (b *LocalSimulatorBackend) ForkSession(_ context.Context, sourceActor, newActor string) (SessionRecord, error) {
	sourceActor = sanitizeActorName(sourceActor)
	b.mu.Lock()
	srcRec, ok := b.records[sourceActor]
	if !ok {
		b.mu.Unlock()
		return SessionRecord{}, fmt.Errorf("source actor %q not found", sourceActor)
	}

	if newActor == "" {
		newActor = fmt.Sprintf("%s-fork-%d", sourceActor, srcRec.SnapshotVersion)
	}
	newActor = sanitizeActorName(newActor)

	tagName := srcRec.LastTag
	if tagName == "" {
		tagName = fmt.Sprintf("tag-%s-fork-%d", sourceActor, srcRec.SnapshotVersion)
	}
	srcSnap := b.gcsSnapshots[sourceActor]
	if srcSnap == nil {
		srcSnap = b.liveActors[sourceActor]
	}
	cloned := srcSnap.Clone()
	cloned.HeroName = strings.TrimPrefix(newActor, "d2d-")
	b.tagSnapshots[tagName] = cloned.Clone()

	slot := b.pickWorkerLocked(newActor, cloned.HeroName, "fork")
	snapResp := cloned.Snapshot(slot.IP)
	b.liveActors[newActor] = cloned
	b.gcsSnapshots[newActor] = cloned.Clone()

	snapURI := fmt.Sprintf("gs://ate-snapshots/%s/%s/tags/%s", b.atespace, b.template, tagName)
	forkRec := &SessionRecord{
		ActorName:         newActor,
		Atespace:          b.atespace,
		Template:          b.template,
		State:             "ACTOR_STATE_SUSPENDED",
		SnapshotURI:       snapURI,
		SnapshotVersion:   1,
		LastWakeMs:        84,
		SaveCount:         1,
		TimelineParent:    fmt.Sprintf("%s@%s", sourceActor, tagName),
		LastTag:           tagName,
		AssignedWorkerIP:  slot.IP,
		AssignedWorkerPod: slot.WorkerPod,
		LastEventType:     "FORK",
		LastMessage:       fmt.Sprintf("Forked parallel timeline %s from tag %q (Floor %d, HP %d)!", newActor, tagName, snapResp.Session.Floor, snapResp.Session.HP),
		UpdatedAt:         time.Now().UTC(),
		Session:           snapResp.Session,
	}
	forkRec.Session.LastEvent = forkRec.LastMessage
	b.records[newActor] = forkRec
	out := *forkRec
	b.mu.Unlock()

	b.hub.publish(TelemetryEvent{
		Type:        "FORK",
		ActorName:   newActor,
		HeroName:    out.Session.HeroName,
		WorkerIP:    slot.IP,
		WorkerPod:   slot.WorkerPod,
		LatencyMs:   84,
		SnapshotURI: snapURI,
		Floor:       out.Session.Floor,
		HP:          out.Session.HP,
		Message:     out.LastMessage,
	})
	return out, nil
}

func (b *LocalSimulatorBackend) DeleteSession(_ context.Context, actorName string) error {
	actorName = sanitizeActorName(actorName)
	b.mu.Lock()
	delete(b.records, actorName)
	delete(b.liveActors, actorName)
	delete(b.gcsSnapshots, actorName)
	b.mu.Unlock()
	b.hub.publish(TelemetryEvent{
		Type:      "DELETE",
		ActorName: actorName,
		HeroName:  strings.TrimPrefix(actorName, "d2d-"),
		Message:   fmt.Sprintf("Deleted actor %s and purged its GCS heap snapshots.", actorName),
	})
	return nil
}

func (b *LocalSimulatorBackend) DashboardState(_ context.Context) (DashboardSnapshot, error) {
	b.mu.Lock()
	now := time.Now().UTC()
	workers := make([]WorkerSlot, len(b.workers))
	for i := range b.workers {
		w := b.workers[i]
		if !w.BusyUntil.IsZero() && now.After(w.BusyUntil) {
			w.ActiveActor = ""
			w.ActiveHero = ""
			w.ActiveAction = ""
		}
		workers[i] = w
	}

	sessions := make([]SessionRecord, 0, len(b.records))
	var stats DashboardStats
	stats.WorkerCapacity = len(workers)
	for _, rec := range b.records {
		sessions = append(sessions, *rec)
		stats.TotalSessions++
		if rec.State == "ACTOR_STATE_RUNNING" {
			stats.RunningActors++
		} else {
			stats.SuspendedActors++
		}
		if !rec.Session.Alive {
			stats.DeadActors++
		}
		if rec.Session.Victory {
			stats.Victories++
		}
	}
	b.mu.Unlock()

	slices.SortFunc(sessions, func(a, b SessionRecord) int {
		return b.UpdatedAt.Compare(a.UpdatedAt)
	})

	events, saves, reverts, forks := b.hub.snapshot()
	stats.TotalSaves = saves
	stats.TotalReverts = reverts
	stats.TotalForks = forks

	return DashboardSnapshot{
		Mode:     "local-simulator",
		Atespace: b.atespace,
		Template: b.template,
		Workers:  workers,
		Sessions: sessions,
		Events:   events,
		Stats:    stats,
	}, nil
}

func (b *LocalSimulatorBackend) SubscribeEvents() (chan TelemetryEvent, func()) {
	return b.hub.subscribe()
}

// ClusterAteBackend orchestrates real Agent Substrate actors via atenet-router
// (wake-on-HTTP + Go heap mutation) and kubectl-ate (create, suspend to GCS,
// revert save-scumming, and tag cloning).
type ClusterAteBackend struct {
	mu          sync.Mutex
	atespace    string
	template    string
	routerURL   string
	ateEndpoint string
	kubectlAte  string
	httpClient  *http.Client
	records     map[string]*SessionRecord
	workers     []WorkerSlot
	lastSync    time.Time
	hub         *eventHub
}

func NewClusterAteBackend(atespace, template, routerURL, ateEndpoint, kubectlAteBin string) *ClusterAteBackend {
	if kubectlAteBin == "" {
		kubectlAteBin = "kubectl-ate"
	}
	return &ClusterAteBackend{
		atespace:    atespace,
		template:    template,
		routerURL:   strings.TrimRight(routerURL, "/"),
		ateEndpoint: ateEndpoint,
		kubectlAte:  kubectlAteBin,
		httpClient: &http.Client{
			Timeout: 25 * time.Second,
		},
		records: make(map[string]*SessionRecord),
		workers: []WorkerSlot{
			{Index: 1, WorkerPod: "worker-slot-1", IP: "10.120.1.22", State: "WORKER_STATE_ACTIVE"},
			{Index: 2, WorkerPod: "worker-slot-2", IP: "10.120.0.11", State: "WORKER_STATE_ACTIVE"},
			{Index: 3, WorkerPod: "worker-slot-3", IP: "10.120.1.23", State: "WORKER_STATE_ACTIVE"},
		},
		hub: newEventHub(),
	}
}

func (c *ClusterAteBackend) runAte(ctx context.Context, args ...string) ([]byte, error) {
	fullArgs := make([]string, 0, len(args)+2)
	if c.ateEndpoint != "" {
		fullArgs = append(fullArgs, "--endpoint="+c.ateEndpoint)
	}
	fullArgs = append(fullArgs, args...)
	cmd := exec.CommandContext(ctx, c.kubectlAte, fullArgs...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("kubectl-ate %v failed: %w (%s)", args, err, strings.TrimSpace(string(out)))
	}
	return out, nil
}

func (c *ClusterAteBackend) callActorHTTP(ctx context.Context, actorName string, method, path string, payload any) (ActorResponse, int64, error) {
	start := time.Now()
	var bodyReader io.Reader
	if payload != nil {
		raw, _ := json.Marshal(payload)
		bodyReader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.routerURL+path, bodyReader)
	if err != nil {
		return ActorResponse{}, 0, err
	}
	req.Header.Set("ate-target-actor", fmt.Sprintf("%s/%s", c.atespace, actorName))
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return ActorResponse{}, 0, fmt.Errorf("atenet-router call failed: %w", err)
	}
	defer resp.Body.Close()
	rawResp, _ := io.ReadAll(io.LimitReader(resp.Body, 65536))
	if resp.StatusCode >= 300 {
		return ActorResponse{}, 0, fmt.Errorf("actor returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(rawResp)))
	}
	var actorResp ActorResponse
	if err := json.Unmarshal(rawResp, &actorResp); err != nil {
		return ActorResponse{}, 0, fmt.Errorf("failed to decode actor JSON: %w", err)
	}
	return actorResp, time.Since(start).Milliseconds(), nil
}

func (c *ClusterAteBackend) inspectRunningActor(ctx context.Context, actorName string) (workerIP, workerPod, snapshotURI string) {
	raw, err := c.runAte(ctx, "get", "actors", actorName, "-a", c.atespace, "-o", "json")
	if err != nil {
		return "", "", ""
	}
	var parsed struct {
		Status struct {
			ExternalSnapshot struct {
				SnapshotURI string `json:"snapshotUri"`
			} `json:"externalSnapshot"`
			WorkerAssignment struct {
				WorkerPod   string `json:"workerPod"`
				WorkerPodIP string `json:"workerPodIp"`
			} `json:"workerAssignment"`
		} `json:"status"`
	}
	if json.Unmarshal(raw, &parsed) != nil {
		return "", "", ""
	}
	return parsed.Status.WorkerAssignment.WorkerPodIP, parsed.Status.WorkerAssignment.WorkerPod, parsed.Status.ExternalSnapshot.SnapshotURI
}

func appendPodIPHop(history []string, ip string) []string {
	if ip == "" {
		return history
	}
	out := make([]string, 0, len(history)+1)
	for _, h := range history {
		if h != "" && !strings.HasPrefix(h, "169.254.") {
			out = append(out, h)
		}
	}
	if len(out) == 0 || out[len(out)-1] != ip {
		out = append(out, ip)
	}
	return out
}

func (c *ClusterAteBackend) markWorkerBusyLocked(ip, podName, actorName, heroName, action string) string {
	for i := range c.workers {
		if (ip != "" && c.workers[i].IP == ip) || (podName != "" && c.workers[i].WorkerPod == podName) {
			if ip != "" {
				c.workers[i].IP = ip
			}
			if podName != "" {
				c.workers[i].WorkerPod = podName
			}
			c.workers[i].ActiveActor = actorName
			c.workers[i].ActiveHero = heroName
			c.workers[i].ActiveAction = action
			c.workers[i].BusyUntil = time.Now().UTC().Add(1200 * time.Millisecond)
			c.workers[i].WakeCount++
			return c.workers[i].WorkerPod
		}
	}
	if len(c.workers) > 0 {
		idx := len(actorName) % len(c.workers)
		if ip != "" {
			c.workers[idx].IP = ip
		}
		if podName != "" {
			c.workers[idx].WorkerPod = podName
		}
		c.workers[idx].ActiveActor = actorName
		c.workers[idx].ActiveHero = heroName
		c.workers[idx].ActiveAction = action
		c.workers[idx].BusyUntil = time.Now().UTC().Add(1200 * time.Millisecond)
		c.workers[idx].WakeCount++
		return c.workers[idx].WorkerPod
	}
	if podName != "" {
		return podName
	}
	return "worker-pod"
}

func (c *ClusterAteBackend) StartSession(ctx context.Context, heroName, charClass string) (SessionRecord, error) {
	actorName := sanitizeActorName(heroName)
	if charClass == "" {
		charClass = "paladin"
	}

	// Ensure actor exists (ignore AlreadyExists error)
	_, _ = c.runAte(ctx, "create", "actor", actorName, "-a", c.atespace, "--template", c.template)

	actorResp, latencyMs, err := c.callActorHTTP(ctx, actorName, http.MethodPost, "/mutate", MutateRequest{
		Action:         "init",
		HeroName:       heroName,
		CharacterClass: charClass,
	})
	if err != nil {
		return SessionRecord{}, err
	}

	workerIP, workerPodName, snapURI := c.inspectRunningActor(ctx, actorName)
	if workerIP != "" {
		actorResp.Session.CurrentPodIP = workerIP
		actorResp.Session.PodIPHistory = appendPodIPHop(nil, workerIP)
	}

	// Immediately suspend actor to GCS to persist initial Go heap and free the worker pod!
	if _, err := c.runAte(ctx, "suspend", "actor", actorName, "-a", c.atespace); err != nil {
		slog.Warn("Failed to suspend actor after StartSession", slog.String("actor", actorName), slog.Any("err", err))
	}

	if snapURI == "" {
		snapURI = fmt.Sprintf("gs://ate-snapshots/%s/%s/actors/%s", c.atespace, c.template, actorName)
	}

	c.mu.Lock()
	workerPod := c.markWorkerBusyLocked(actorResp.Session.CurrentPodIP, workerPodName, actorName, actorResp.Session.HeroName, "init")
	rec := &SessionRecord{
		ActorName:         actorName,
		Atespace:          c.atespace,
		Template:          c.template,
		State:             "ACTOR_STATE_SUSPENDED",
		SnapshotURI:       snapURI,
		SnapshotVersion:   1,
		LastWakeMs:        latencyMs,
		SaveCount:         1,
		AssignedWorkerIP:  actorResp.Session.CurrentPodIP,
		AssignedWorkerPod: workerPod,
		LastMessage:       actorResp.Message,
		LastEventType:     actorResp.EventType,
		UpdatedAt:         time.Now().UTC(),
		Session:           actorResp.Session,
	}
	c.records[actorName] = rec
	out := *rec
	c.mu.Unlock()

	c.hub.publish(TelemetryEvent{
		Type:        "GCS_COMMIT",
		ActorName:   actorName,
		HeroName:    out.Session.HeroName,
		WorkerIP:    out.AssignedWorkerIP,
		WorkerPod:   workerPod,
		LatencyMs:   latencyMs,
		SnapshotURI: out.SnapshotURI,
		Floor:       out.Session.Floor,
		HP:          out.Session.HP,
		Message:     fmt.Sprintf("Woke %s on %s (%dms) -> Initialized heap -> Suspended to GCS", actorName, out.AssignedWorkerIP, latencyMs),
	})
	return out, nil
}

func (c *ClusterAteBackend) GetSession(ctx context.Context, actorName string) (SessionRecord, error) {
	actorName = sanitizeActorName(actorName)
	c.mu.Lock()
	rec, ok := c.records[actorName]
	c.mu.Unlock()
	if ok {
		return *rec, nil
	}
	return c.StartSession(ctx, strings.TrimPrefix(actorName, "d2d-"), "paladin")
}

func (c *ClusterAteBackend) ExecuteMilestone(ctx context.Context, actorName string, req MutateRequest) (SessionRecord, error) {
	actorName = sanitizeActorName(actorName)
	c.mu.Lock()
	_, exists := c.records[actorName]
	c.mu.Unlock()
	if !exists {
		if _, err := c.StartSession(ctx, strings.TrimPrefix(actorName, "d2d-"), req.CharacterClass); err != nil {
			return SessionRecord{}, err
		}
	}

	actorResp, latencyMs, err := c.callActorHTTP(ctx, actorName, http.MethodPost, "/mutate", req)
	if err != nil {
		return SessionRecord{}, err
	}

	workerIP, workerPodName, snapURI := c.inspectRunningActor(ctx, actorName)

	evType := "GCS_COMMIT"
	newState := "ACTOR_STATE_SUSPENDED"
	var newTag string

	if !actorResp.Session.Alive {
		// Leave the dead actor RUNNING so `kubectl ate revert actor` can discard
		// the fatal in-memory mutation and restore the last GCS snapshot!
		newState = "ACTOR_STATE_RUNNING"
		evType = "DEATH"
	} else {
		if _, err := c.runAte(ctx, "suspend", "actor", actorName, "-a", c.atespace); err != nil {
			slog.Warn("Failed to suspend actor after milestone", slog.String("actor", actorName), slog.Any("err", err))
		}
		if req.Action == "quicksave" || actorResp.EventType == "FLOOR_CHANGE" {
			newTag = fmt.Sprintf("tag-%s-f%d-%d", actorName, actorResp.Session.Floor, time.Now().Unix()%10000)
			_, _ = c.runAte(ctx, "delete", "tag", newTag, "-a", c.atespace)
			if _, err := c.runAte(ctx, "create", "tag", newTag, "-a", c.atespace, "--actor", actorName); err != nil {
				slog.Warn("Failed to tag actor snapshot", slog.String("actor", actorName), slog.Any("err", err))
			}
			if req.Action == "quicksave" {
				evType = "QUICKSAVE"
			}
		}
		if actorResp.Session.Victory {
			evType = "VICTORY"
		}
	}

	c.mu.Lock()
	rec := c.records[actorName]
	if rec == nil {
		rec = &SessionRecord{ActorName: actorName, Atespace: c.atespace, Template: c.template}
		c.records[actorName] = rec
	}
	if workerIP != "" {
		actorResp.Session.CurrentPodIP = workerIP
		actorResp.Session.PodIPHistory = appendPodIPHop(rec.Session.PodIPHistory, workerIP)
	}
	if snapURI != "" {
		rec.SnapshotURI = snapURI
	}
	workerPod := c.markWorkerBusyLocked(actorResp.Session.CurrentPodIP, workerPodName, actorName, actorResp.Session.HeroName, req.Action)
	rec.State = newState
	if newState == "ACTOR_STATE_SUSPENDED" {
		rec.SnapshotVersion++
		rec.SaveCount++
	}
	if newTag != "" {
		rec.LastTag = newTag
	}
	rec.LastWakeMs = latencyMs
	rec.AssignedWorkerIP = actorResp.Session.CurrentPodIP
	rec.AssignedWorkerPod = workerPod
	rec.LastMessage = actorResp.Message
	rec.LastEventType = actorResp.EventType
	rec.UpdatedAt = time.Now().UTC()
	rec.Session = actorResp.Session
	out := *rec
	c.mu.Unlock()

	c.hub.publish(TelemetryEvent{
		Type:        evType,
		ActorName:   actorName,
		HeroName:    out.Session.HeroName,
		WorkerIP:    out.AssignedWorkerIP,
		WorkerPod:   workerPod,
		LatencyMs:   latencyMs,
		SnapshotURI: out.SnapshotURI,
		Floor:       out.Session.Floor,
		HP:          out.Session.HP,
		Message:     actorResp.Message,
	})
	return out, nil
}

func (c *ClusterAteBackend) RevertSession(ctx context.Context, actorName string) (SessionRecord, error) {
	actorName = sanitizeActorName(actorName)

	c.mu.Lock()
	existingRec := c.records[actorName]
	wasDead := existingRec != nil && !existingRec.Session.Alive
	c.mu.Unlock()

	var actorResp ActorResponse
	var latencyMs int64
	var err error

	if wasDead {
		// Player died in RAM (actor was left ACTOR_STATE_RUNNING uncommitted):
		// Call `kubectl ate revert actor <name> -a <atespace>` to discard the fatal RAM
		// mutation and restore the last committed GCS snapshot!
		if _, revErr := c.runAte(ctx, "revert", "actor", actorName, "-a", c.atespace); revErr != nil {
			// If actor was already suspended, wake it first so revert can run
			_, _, _ = c.callActorHTTP(ctx, actorName, http.MethodGet, "/state", nil)
			if _, revErr2 := c.runAte(ctx, "revert", "actor", actorName, "-a", c.atespace); revErr2 != nil {
				slog.Warn("kubectl-ate revert returned warning", slog.String("actor", actorName), slog.Any("err", revErr2))
			}
		}
		actorResp, latencyMs, err = c.callActorHTTP(ctx, actorName, http.MethodGet, "/state", nil)
		if err != nil {
			return SessionRecord{}, err
		}
		// If the restored snapshot somehow still had !Alive, restore checkpoint
		if !actorResp.Session.Alive {
			actorResp, latencyMs, err = c.callActorHTTP(ctx, actorName, http.MethodPost, "/mutate", MutateRequest{
				Action: "restore_checkpoint",
			})
			if err != nil {
				return SessionRecord{}, err
			}
		} else {
			actorResp.Message = fmt.Sprintf("SAVE-SCUMMED (`kubectl ate revert actor %s`): discarded dead RAM and restored GCS snapshot on Floor %d (HP: %d/%d)!",
				actorName, actorResp.Session.Floor, actorResp.Session.HP, actorResp.Session.MaxHP)
		}
	} else {
		// Player clicked "Revert Actor (Save-Scum)" while alive (actor is SUSPENDED in GCS):
		// 1. Wake the actor via atenet-router so it is ACTOR_STATE_RUNNING (required by `kubectl-ate revert`).
		_, _, _ = c.callActorHTTP(ctx, actorName, http.MethodGet, "/state", nil)
		// 2. Execute `kubectl-ate revert actor <name> -a <atespace>` to revert the running execution.
		if _, revErr := c.runAte(ctx, "revert", "actor", actorName, "-a", c.atespace); revErr != nil {
			slog.Warn("kubectl-ate revert on live session returned warning", slog.String("actor", actorName), slog.Any("err", revErr))
		}
		// 3. Rewind the actor's Go heap to its last Quicksave / Floor-Entry Checkpoint!
		actorResp, latencyMs, err = c.callActorHTTP(ctx, actorName, http.MethodPost, "/mutate", MutateRequest{
			Action: "restore_checkpoint",
		})
		if err != nil {
			return SessionRecord{}, err
		}
	}

	workerIP, workerPodName, snapURI := c.inspectRunningActor(ctx, actorName)

	// Suspend it back to GCS so the worker pod is freed
	_, _ = c.runAte(ctx, "suspend", "actor", actorName, "-a", c.atespace)

	c.mu.Lock()
	rec := c.records[actorName]
	if rec == nil {
		rec = &SessionRecord{ActorName: actorName, Atespace: c.atespace, Template: c.template}
		c.records[actorName] = rec
	}
	if workerIP != "" {
		actorResp.Session.CurrentPodIP = workerIP
		actorResp.Session.PodIPHistory = appendPodIPHop(rec.Session.PodIPHistory, workerIP)
	}
	if snapURI != "" {
		rec.SnapshotURI = snapURI
	}
	workerPod := c.markWorkerBusyLocked(actorResp.Session.CurrentPodIP, workerPodName, actorName, actorResp.Session.HeroName, "revert")
	rec.State = "ACTOR_STATE_SUSPENDED"
	rec.RevertCount++
	rec.LastWakeMs = latencyMs
	rec.AssignedWorkerIP = actorResp.Session.CurrentPodIP
	rec.AssignedWorkerPod = workerPod
	rec.LastEventType = "REVERT"
	rec.LastMessage = actorResp.Message
	actorResp.Session.LastEvent = rec.LastMessage
	rec.Session = actorResp.Session
	rec.UpdatedAt = time.Now().UTC()
	out := *rec
	c.mu.Unlock()

	c.hub.publish(TelemetryEvent{
		Type:        "REVERT",
		ActorName:   actorName,
		HeroName:    out.Session.HeroName,
		WorkerIP:    out.AssignedWorkerIP,
		WorkerPod:   workerPod,
		LatencyMs:   latencyMs,
		SnapshotURI: out.SnapshotURI,
		Floor:       out.Session.Floor,
		HP:          out.Session.HP,
		Message:     out.LastMessage,
	})
	return out, nil
}

func (c *ClusterAteBackend) ForkSession(ctx context.Context, sourceActor, newActor string) (SessionRecord, error) {
	sourceActor = sanitizeActorName(sourceActor)
	if newActor == "" {
		newActor = fmt.Sprintf("%s-fork-%d", sourceActor, time.Now().Unix()%1000)
	}
	newActor = sanitizeActorName(newActor)

	// Ensure source actor is suspended before creating tag
	_, _ = c.runAte(ctx, "suspend", "actor", sourceActor, "-a", c.atespace)
	tagName := fmt.Sprintf("tag-%s-%d", sourceActor, time.Now().Unix()%10000)
	if _, err := c.runAte(ctx, "create", "tag", tagName, "-a", c.atespace, "--actor", sourceActor); err != nil {
		return SessionRecord{}, err
	}
	if _, err := c.runAte(ctx, "create", "actor", newActor, "-a", c.atespace, "--template", c.template, "--tag", tagName); err != nil {
		return SessionRecord{}, err
	}

	actorResp, latencyMs, err := c.callActorHTTP(ctx, newActor, http.MethodGet, "/state", nil)
	if err != nil {
		return SessionRecord{}, err
	}
	workerIP, workerPodName, snapURI := c.inspectRunningActor(ctx, newActor)
	_, _ = c.runAte(ctx, "suspend", "actor", newActor, "-a", c.atespace)

	c.mu.Lock()
	var baseHops []string
	if srcRec := c.records[sourceActor]; srcRec != nil {
		baseHops = srcRec.Session.PodIPHistory
	}
	if workerIP != "" {
		actorResp.Session.CurrentPodIP = workerIP
		actorResp.Session.PodIPHistory = appendPodIPHop(baseHops, workerIP)
	}
	if snapURI == "" {
		snapURI = fmt.Sprintf("gs://ate-snapshots/%s/%s/tags/%s", c.atespace, c.template, tagName)
	}
	workerPod := c.markWorkerBusyLocked(actorResp.Session.CurrentPodIP, workerPodName, newActor, actorResp.Session.HeroName, "fork")
	forkRec := &SessionRecord{
		ActorName:         newActor,
		Atespace:          c.atespace,
		Template:          c.template,
		State:             "ACTOR_STATE_SUSPENDED",
		SnapshotURI:       snapURI,
		SnapshotVersion:   1,
		LastWakeMs:        latencyMs,
		SaveCount:         1,
		TimelineParent:    fmt.Sprintf("%s@%s", sourceActor, tagName),
		LastTag:           tagName,
		AssignedWorkerIP:  actorResp.Session.CurrentPodIP,
		AssignedWorkerPod: workerPod,
		LastEventType:     "FORK",
		LastMessage:       fmt.Sprintf("Forked parallel timeline %s from tag %q (Floor %d, HP %d)!", newActor, tagName, actorResp.Session.Floor, actorResp.Session.HP),
		UpdatedAt:         time.Now().UTC(),
		Session:           actorResp.Session,
	}
	forkRec.Session.LastEvent = forkRec.LastMessage
	c.records[newActor] = forkRec
	out := *forkRec
	c.mu.Unlock()

	c.hub.publish(TelemetryEvent{
		Type:        "FORK",
		ActorName:   newActor,
		HeroName:    out.Session.HeroName,
		WorkerIP:    out.AssignedWorkerIP,
		WorkerPod:   workerPod,
		LatencyMs:   latencyMs,
		SnapshotURI: out.SnapshotURI,
		Floor:       out.Session.Floor,
		HP:          out.Session.HP,
		Message:     out.LastMessage,
	})
	return out, nil
}

func (c *ClusterAteBackend) DeleteSession(ctx context.Context, actorName string) error {
	actorName = sanitizeActorName(actorName)
	_, _ = c.runAte(ctx, "delete", "actor", actorName, "-a", c.atespace, "--any-state")
	c.mu.Lock()
	delete(c.records, actorName)
	c.mu.Unlock()
	c.hub.publish(TelemetryEvent{
		Type:      "DELETE",
		ActorName: actorName,
		HeroName:  strings.TrimPrefix(actorName, "d2d-"),
		Message:   fmt.Sprintf("Deleted actor %s and purged its GCS snapshots.", actorName),
	})
	return nil
}

func (c *ClusterAteBackend) syncClusterMetadata(ctx context.Context) {
	c.mu.Lock()
	if time.Since(c.lastSync) < 3*time.Second {
		c.mu.Unlock()
		return
	}
	c.lastSync = time.Now()
	c.mu.Unlock()

	if rawWorkers, err := c.runAte(ctx, "get", "workers", "-o", "json"); err == nil {
		var parsed struct {
			Workers []struct {
				IP        string            `json:"ip"`
				IPs       []string          `json:"ips"`
				Labels    map[string]string `json:"labels"`
				NodeName  string            `json:"nodeName"`
				WorkerPod string            `json:"workerPod"`
				Status    struct {
					State string `json:"state"`
				} `json:"status"`
			} `json:"workers"`
		}
		if json.Unmarshal(rawWorkers, &parsed) == nil && len(parsed.Workers) > 0 {
			c.mu.Lock()
			slots := make([]WorkerSlot, 0, len(parsed.Workers))
			for idx, w := range parsed.Workers {
				ip := w.IP
				if ip == "" && len(w.IPs) > 0 {
					ip = w.IPs[0]
				}
				slot := WorkerSlot{
					Index:     idx + 1,
					WorkerPod: w.WorkerPod,
					IP:        ip,
					NodeName:  w.NodeName,
					State:     w.Status.State,
				}
				// Preserve active animation fields from existing slot with same IP or pod name
				for _, old := range c.workers {
					if (ip != "" && old.IP == ip) || (w.WorkerPod != "" && old.WorkerPod == w.WorkerPod) {
						slot.ActiveActor = old.ActiveActor
						slot.ActiveHero = old.ActiveHero
						slot.ActiveAction = old.ActiveAction
						slot.BusyUntil = old.BusyUntil
						slot.WakeCount = old.WakeCount
						break
					}
				}
				slots = append(slots, slot)
			}
			c.workers = slots
			c.mu.Unlock()
		}
	}

	if rawActors, err := c.runAte(ctx, "get", "actors", "-a", c.atespace, "-o", "json"); err == nil {
		var parsed struct {
			Actors []struct {
				ActorTemplate struct {
					Name string `json:"name"`
				} `json:"actorTemplate"`
				Metadata struct {
					Name string `json:"name"`
				} `json:"metadata"`
				Status struct {
					State            string `json:"state"`
					ExternalSnapshot struct {
						SnapshotURI string `json:"snapshotUri"`
					} `json:"externalSnapshot"`
				} `json:"status"`
			} `json:"actors"`
		}
		if json.Unmarshal(rawActors, &parsed) == nil {
			c.mu.Lock()
			for _, a := range parsed.Actors {
				if a.ActorTemplate.Name != c.template && !strings.HasPrefix(a.Metadata.Name, "d2d-") {
					continue
				}
				rec, ok := c.records[a.Metadata.Name]
				if !ok {
					sess := NewActorSession()
					sess.HeroName = strings.TrimPrefix(a.Metadata.Name, "d2d-")
					rec = &SessionRecord{
						ActorName: a.Metadata.Name,
						Atespace:  c.atespace,
						Template:  a.ActorTemplate.Name,
						UpdatedAt: time.Now().UTC(),
						Session:   *sess,
					}
					c.records[a.Metadata.Name] = rec
				}
				rec.State = a.Status.State
				if a.Status.ExternalSnapshot.SnapshotURI != "" {
					rec.SnapshotURI = a.Status.ExternalSnapshot.SnapshotURI
				}
			}
			c.mu.Unlock()
		}
	}
}

func (c *ClusterAteBackend) DashboardState(ctx context.Context) (DashboardSnapshot, error) {
	c.syncClusterMetadata(ctx)

	c.mu.Lock()
	now := time.Now().UTC()
	workers := make([]WorkerSlot, len(c.workers))
	for i := range c.workers {
		w := c.workers[i]
		if !w.BusyUntil.IsZero() && now.After(w.BusyUntil) {
			w.ActiveActor = ""
			w.ActiveHero = ""
			w.ActiveAction = ""
		}
		workers[i] = w
	}

	sessions := make([]SessionRecord, 0, len(c.records))
	var stats DashboardStats
	stats.WorkerCapacity = len(workers)
	for _, rec := range c.records {
		sessions = append(sessions, *rec)
		stats.TotalSessions++
		if rec.State == "ACTOR_STATE_RUNNING" {
			stats.RunningActors++
		} else {
			stats.SuspendedActors++
		}
		if !rec.Session.Alive {
			stats.DeadActors++
		}
		if rec.Session.Victory {
			stats.Victories++
		}
	}
	c.mu.Unlock()

	slices.SortFunc(sessions, func(a, b SessionRecord) int {
		return b.UpdatedAt.Compare(a.UpdatedAt)
	})

	events, saves, reverts, forks := c.hub.snapshot()
	stats.TotalSaves = saves
	stats.TotalReverts = reverts
	stats.TotalForks = forks

	return DashboardSnapshot{
		Mode:     "cluster-ate",
		Atespace: c.atespace,
		Template: c.template,
		Workers:  workers,
		Sessions: sessions,
		Events:   events,
		Stats:    stats,
	}, nil
}

func (c *ClusterAteBackend) SubscribeEvents() (chan TelemetryEvent, func()) {
	return c.hub.subscribe()
}
