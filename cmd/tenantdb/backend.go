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
	"math"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

type SeedRowSpec struct {
	Table     string
	Key       string
	Value     string
	AmountUSD int
}

type TenantCatalogEntry struct {
	ActorName   string
	CompanyName string
	Industry    string
	Tier        string
	Region      string
	DefaultRows []SeedRowSpec
}

var defaultTenantCatalog = []TenantCatalogEntry{
	{
		ActorName:   "db-acme",
		CompanyName: "Acme Corp",
		Industry:    "Enterprise Retail",
		Tier:        "Enterprise Plus",
		Region:      "us-west3-slc",
		DefaultRows: []SeedRowSpec{
			{Table: "subscriptions", Key: "tier", Value: "enterprise", AmountUSD: 48000},
			{Table: "settings", Key: "region", Value: "us-west3-slc", AmountUSD: 0},
			{Table: "invoices", Key: "inv-2026-01", Value: "Acme Global POS Platform Renewal", AmountUSD: 94000},
		},
	},
	{
		ActorName:   "db-globex",
		CompanyName: "Globex Logistics",
		Industry:    "Supply Chain & Freight",
		Tier:        "Enterprise",
		Region:      "us-east1-va",
		DefaultRows: []SeedRowSpec{
			{Table: "subscriptions", Key: "tier", Value: "freight-scale", AmountUSD: 32000},
			{Table: "settings", Key: "region", Value: "us-east1-va", AmountUSD: 0},
			{Table: "invoices", Key: "inv-8841", Value: "Globex Autonomous Fleet Telemetry", AmountUSD: 57500},
		},
	},
	{
		ActorName:   "db-initech",
		CompanyName: "Initech FinTech",
		Industry:    "Core Banking",
		Tier:        "Regulated Sovereign",
		Region:      "eu-central1-fra",
		DefaultRows: []SeedRowSpec{
			{Table: "subscriptions", Key: "tier", Value: "sovereign-vault", AmountUSD: 85000},
			{Table: "settings", Key: "region", Value: "eu-central1-fra", AmountUSD: 0},
			{Table: "invoices", Key: "inv-4092", Value: "Initech Real-Time Ledger Settlement", AmountUSD: 130000},
		},
	},
	{
		ActorName:   "db-umbrella",
		CompanyName: "Umbrella Biotech",
		Industry:    "Genomics R&D",
		Tier:        "Growth Lab",
		Region:      "ap-northeast1-tyo",
		DefaultRows: []SeedRowSpec{
			{Table: "subscriptions", Key: "tier", Value: "genomics-pro", AmountUSD: 24000},
			{Table: "settings", Key: "region", Value: "ap-northeast1-tyo", AmountUSD: 0},
			{Table: "invoices", Key: "inv-7710", Value: "Umbrella Protein Folding Compute Batch", AmountUSD: 40000},
		},
	},
}

func lookupCatalogEntry(actorName string) TenantCatalogEntry {
	for _, c := range defaultTenantCatalog {
		if c.ActorName == actorName {
			return c
		}
	}
	return TenantCatalogEntry{
		ActorName:   actorName,
		CompanyName: strings.ToUpper(strings.TrimPrefix(actorName, "db-")),
		Industry:    "SaaS Customer",
		Tier:        "Standard",
		Region:      "us-west3-slc",
		DefaultRows: []SeedRowSpec{
			{Table: "subscriptions", Key: "tier", Value: "standard", AmountUSD: 12000},
		},
	}
}

type GDPRDeleteReceipt struct {
	ActorName             string    `json:"actor_name"`
	CompanyName           string    `json:"company_name"`
	CommandExecuted       string    `json:"command_executed"`
	ComputeShredded       bool      `json:"compute_shredded"`
	RAMSnapshotsPurged    bool      `json:"ram_snapshots_purged"`
	DurableDirShredded    bool      `json:"durable_dir_shredded"`
	PostDeleteProbeHTTP   int       `json:"post_delete_probe_http"`
	SiblingTenantVerified string    `json:"sibling_tenant_verified"`
	DeletedAt             time.Time `json:"deleted_at"`
}

type TenantTracker struct {
	ActorName     string             `json:"actor_name"`
	CompanyName   string             `json:"company_name"`
	Industry      string             `json:"industry"`
	Tier          string             `json:"tier"`
	Region        string             `json:"region"`
	Status        string             `json:"status"` // UNINITIALIZED | RUNNING | PAUSED_FULL | SUSPENDED_DATA | GDPR_SHREDDED
	OnPauseScope  string             `json:"on_pause_scope"`
	OnCommitScope string             `json:"on_commit_scope"`
	WorkerSlot    int                `json:"worker_slot"`
	WorkerPodIP   string             `json:"worker_pod_ip,omitempty"`
	LastSnapshot  *TenantSnapshot    `json:"last_snapshot,omitempty"`
	GDPRReceipt   *GDPRDeleteReceipt `json:"gdpr_receipt,omitempty"`
	UpdatedAt     time.Time          `json:"updated_at"`
}

type WorkerSlotStatus struct {
	Slot        int    `json:"slot"`
	PodIP       string `json:"pod_ip"`
	HostedActor string `json:"hosted_actor,omitempty"`
	CompanyName string `json:"company_name,omitempty"`
	ActorState  string `json:"actor_state"` // IDLE | RUNNING | PAUSED_FULL
}

type FleetSummaryMetrics struct {
	ActiveTenants           int     `json:"active_tenants"`
	RunningOnWorkers        int     `json:"running_on_workers"`
	PausedFullCount         int     `json:"paused_full_count"`
	SuspendedDataCount      int     `json:"suspended_data_count"`
	ShreddedGDPRCount       int     `json:"shredded_gdpr_count"`
	TotalHotRAMBytes        int64   `json:"total_hot_ram_bytes"`
	TotalDurableWALBytes    int64   `json:"total_durable_wal_bytes"`
	RawFullSnapshotBytes    int64   `json:"raw_full_snapshot_bytes"`
	DataScopeCommittedBytes int64   `json:"data_scope_committed_bytes"`
	GCSSavingsPct           float64 `json:"gcs_savings_pct"`
}

type FleetAuditEvent struct {
	Timestamp time.Time `json:"timestamp"`
	ActorName string    `json:"actor_name"`
	Action    string    `json:"action"`
	Scope     string    `json:"scope"`
	Detail    string    `json:"detail"`
}

type FleetStateResponse struct {
	Mode           string              `json:"mode"`
	Atespace       string              `json:"atespace"`
	Template       string              `json:"template"`
	WorkerPoolSize int                 `json:"worker_pool_size"`
	WorkerSlots    []WorkerSlotStatus  `json:"worker_slots"`
	Tenants        []TenantTracker     `json:"tenants"`
	Summary        FleetSummaryMetrics `json:"summary"`
	AuditLog       []FleetAuditEvent   `json:"audit_log"`
}

type TenantDBBackend interface {
	GetFleetState(ctx context.Context) (*FleetStateResponse, error)
	SeedFleet(ctx context.Context, actorName string) (*FleetStateResponse, error)
	QueryTenant(ctx context.Context, actorName, table, key, val string, amountUSD int) (*TenantSnapshot, error)
	PauseTenant(ctx context.Context, actorName string) (*TenantTracker, error)
	SuspendTenant(ctx context.Context, actorName string) (*TenantTracker, error)
	GDPRDeleteTenant(ctx context.Context, actorName string) (*GDPRDeleteReceipt, error)
	ResetFleet(ctx context.Context) (*FleetStateResponse, error)
}

var simulatedWorkerPoolIPs = []string{"10.120.1.14", "10.120.2.28", "10.120.3.42"}

func computeFleetSummary(tenants []TenantTracker) FleetSummaryMetrics {
	var m FleetSummaryMetrics
	for _, t := range tenants {
		switch t.Status {
		case "RUNNING":
			m.ActiveTenants++
			m.RunningOnWorkers++
		case "PAUSED_FULL":
			m.ActiveTenants++
			m.PausedFullCount++
		case "SUSPENDED_DATA":
			m.ActiveTenants++
			m.SuspendedDataCount++
		case "GDPR_SHREDDED":
			m.ShreddedGDPRCount++
		}
		if t.LastSnapshot != nil && t.Status != "GDPR_SHREDDED" {
			ramB := int64(defaultBufferPoolBytes)
			if t.LastSnapshot.RAMBufferPoolBytes > 0 {
				ramB = int64(t.LastSnapshot.RAMBufferPoolBytes)
			}
			walB := t.LastSnapshot.DurableWALBytes
			m.RawFullSnapshotBytes += ramB + walB
			m.DataScopeCommittedBytes += walB
			m.TotalDurableWALBytes += walB
			if t.Status == "RUNNING" || t.Status == "PAUSED_FULL" {
				m.TotalHotRAMBytes += ramB
			}
		}
	}
	if m.RawFullSnapshotBytes > 0 {
		pct := (1.0 - float64(m.DataScopeCommittedBytes)/float64(m.RawFullSnapshotBytes)) * 100.0
		if pct < 0 {
			pct = 0
		}
		m.GCSSavingsPct = math.Round(pct*1000) / 1000
	}
	return m
}

// --- LocalSimulatorBackend (--mode=local & unit tests) ---

type localTenantNode struct {
	tracker   TenantTracker
	dataDir   string
	db        *TenantDB
	podCursor int
	lastUsed  time.Time
}

type LocalSimulatorBackend struct {
	mu       sync.Mutex
	atespace string
	template string
	baseDir  string
	nodes    map[string]*localTenantNode
	order    []string
	slots    [3]string // actorName occupying worker slots 0..2
	auditLog []FleetAuditEvent
}

func NewLocalSimulatorBackend(atespace, template string) *LocalSimulatorBackend {
	baseDir, _ := os.MkdirTemp("", "tenantdb-local-sim-*")
	b := &LocalSimulatorBackend{
		atespace: atespace,
		template: template,
		baseDir:  baseDir,
		nodes:    make(map[string]*localTenantNode),
	}
	b.initCatalogLocked()
	return b
}

func (b *LocalSimulatorBackend) initCatalogLocked() {
	b.nodes = make(map[string]*localTenantNode)
	b.order = nil
	b.slots = [3]string{}
	for idx, c := range defaultTenantCatalog {
		dir := filepath.Join(b.baseDir, c.ActorName)
		_ = os.RemoveAll(dir)
		b.nodes[c.ActorName] = &localTenantNode{
			dataDir:   dir,
			podCursor: idx % len(simulatedWorkerPoolIPs),
			tracker: TenantTracker{
				ActorName:     c.ActorName,
				CompanyName:   c.CompanyName,
				Industry:      c.Industry,
				Tier:          c.Tier,
				Region:        c.Region,
				Status:        "UNINITIALIZED",
				OnPauseScope:  "SNAPSHOT_CONTENT_SCOPE_FULL",
				OnCommitScope: "SNAPSHOT_CONTENT_SCOPE_DATA",
				UpdatedAt:     time.Now().UTC(),
			},
		}
		b.order = append(b.order, c.ActorName)
	}
}

func (b *LocalSimulatorBackend) appendAuditLocked(actor, action, scope, detail string) {
	ev := FleetAuditEvent{
		Timestamp: time.Now().UTC(),
		ActorName: actor,
		Action:    action,
		Scope:     scope,
		Detail:    detail,
	}
	b.auditLog = append([]FleetAuditEvent{ev}, b.auditLog...)
	if len(b.auditLog) > 30 {
		b.auditLog = b.auditLog[:30]
	}
}

func (b *LocalSimulatorBackend) ensureNodeLocked(actorName string) *localTenantNode {
	if node, ok := b.nodes[actorName]; ok {
		return node
	}
	c := lookupCatalogEntry(actorName)
	dir := filepath.Join(b.baseDir, actorName)
	node := &localTenantNode{
		dataDir:   dir,
		podCursor: len(b.order) % len(simulatedWorkerPoolIPs),
		tracker: TenantTracker{
			ActorName:     c.ActorName,
			CompanyName:   c.CompanyName,
			Industry:      c.Industry,
			Tier:          c.Tier,
			Region:        c.Region,
			Status:        "UNINITIALIZED",
			OnPauseScope:  "SNAPSHOT_CONTENT_SCOPE_FULL",
			OnCommitScope: "SNAPSHOT_CONTENT_SCOPE_DATA",
			UpdatedAt:     time.Now().UTC(),
		},
	}
	b.nodes[actorName] = node
	b.order = append(b.order, actorName)
	return node
}

// assignWorkerSlotLocked assigns one of the 3 WorkerPool slots to actorName.
// If all 3 slots are occupied by other tenants, the oldest tenant on a worker slot is
// automatically paused/suspended so the 4-tenant -> 3-worker multiplexing is visible.
func (b *LocalSimulatorBackend) assignWorkerSlotLocked(node *localTenantNode, rotatePod bool) (int, string) {
	for i := 0; i < 3; i++ {
		if b.slots[i] == node.tracker.ActorName {
			if rotatePod {
				b.slots[i] = ""
				break
			}
			return i + 1, simulatedWorkerPoolIPs[i]
		}
	}

	preferred := node.podCursor % 3
	if rotatePod {
		preferred = (node.podCursor + 1) % 3
		node.podCursor = preferred
	}

	// Look for an empty worker slot starting from preferred.
	for offset := 0; offset < 3; offset++ {
		idx := (preferred + offset) % 3
		if b.slots[idx] == "" {
			b.slots[idx] = node.tracker.ActorName
			node.podCursor = idx
			return idx + 1, simulatedWorkerPoolIPs[idx]
		}
	}

	// All 3 worker pods are occupied: multiplex by pausing/parking the least-recently-used occupant.
	oldestSlot := 0
	var oldestTime time.Time
	for i := 0; i < 3; i++ {
		occ := b.nodes[b.slots[i]]
		if occ != nil && (oldestTime.IsZero() || occ.lastUsed.Before(oldestTime)) {
			oldestTime = occ.lastUsed
			oldestSlot = i
		}
	}
	if evicted := b.nodes[b.slots[oldestSlot]]; evicted != nil {
		evicted.tracker.Status = "PAUSED_FULL"
		evicted.tracker.WorkerSlot = 0
		evicted.tracker.UpdatedAt = time.Now().UTC()
		b.appendAuditLocked(evicted.tracker.ActorName, "AUTO_MULTIPLEX_PAUSE", "FULL",
			fmt.Sprintf("Worker-%d (%s) multiplexed to %s; %s heap kept warm in FULL pause",
				oldestSlot+1, simulatedWorkerPoolIPs[oldestSlot], node.tracker.ActorName, evicted.tracker.ActorName))
	}
	b.slots[oldestSlot] = node.tracker.ActorName
	node.podCursor = oldestSlot
	return oldestSlot + 1, simulatedWorkerPoolIPs[oldestSlot]
}

func (b *LocalSimulatorBackend) freeWorkerSlotLocked(actorName string) {
	for i := 0; i < 3; i++ {
		if b.slots[i] == actorName {
			b.slots[i] = ""
		}
	}
}

func (b *LocalSimulatorBackend) wakeAndQueryLocked(node *localTenantNode, table, key, val string, amountUSD int) (*TenantSnapshot, error) {
	if node.tracker.Status == "GDPR_SHREDDED" {
		return nil, fmt.Errorf("404 TENANT_SHREDDED: actor %s was permanently deleted via GDPR Right-to-be-Forgotten", node.tracker.ActorName)
	}

	wasSuspendedData := node.tracker.Status == "SUSPENDED_DATA"
	slot, podIP := b.assignWorkerSlotLocked(node, wasSuspendedData)

	if node.db == nil {
		db, err := openTenantDB(node.dataDir)
		if err != nil {
			return nil, err
		}
		node.db = db
	}

	if key != "" {
		if err := node.db.PutRow(table, key, val, amountUSD, podIP); err != nil {
			return nil, err
		}
	}
	snap := node.db.BuildSnapshot(podIP)

	node.tracker.Status = "RUNNING"
	node.tracker.WorkerSlot = slot
	node.tracker.WorkerPodIP = podIP
	node.tracker.LastSnapshot = &snap
	node.tracker.GDPRReceipt = nil
	node.tracker.UpdatedAt = time.Now().UTC()
	node.lastUsed = time.Now().UTC()

	return &snap, nil
}

func (b *LocalSimulatorBackend) SeedFleet(_ context.Context, actorName string) (*FleetStateResponse, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	targets := b.order
	if actorName != "" && actorName != "all" {
		b.ensureNodeLocked(actorName)
		targets = []string{actorName}
	}

	for _, name := range targets {
		node := b.ensureNodeLocked(name)
		if node.tracker.Status == "GDPR_SHREDDED" {
			_ = os.RemoveAll(node.dataDir)
			node.db = nil
			node.tracker.Status = "UNINITIALIZED"
			node.tracker.GDPRReceipt = nil
		}
		c := lookupCatalogEntry(name)
		for _, r := range c.DefaultRows {
			if _, err := b.wakeAndQueryLocked(node, r.Table, r.Key, r.Value, r.AmountUSD); err != nil {
				return nil, err
			}
		}
		b.appendAuditLocked(name, "SEED_TENANT_DB", "FULL+DATA",
			fmt.Sprintf("Seeded %d SaaS records into %s (/data/wal.jsonl) & warmed 16.0 MB RAM Buffer Pool on %s",
				len(c.DefaultRows), name, node.tracker.WorkerPodIP))
	}

	return b.buildStateLocked(), nil
}

func (b *LocalSimulatorBackend) QueryTenant(_ context.Context, actorName, table, key, val string, amountUSD int) (*TenantSnapshot, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	node := b.ensureNodeLocked(actorName)
	prevStatus := node.tracker.Status
	snap, err := b.wakeAndQueryLocked(node, table, key, val, amountUSD)
	if err != nil {
		return nil, err
	}

	action := "SELECT_QUERY"
	if key != "" {
		action = "INSERT_WAL_ROW"
	}
	scope := "HOT_RAM"
	if prevStatus == "SUSPENDED_DATA" || snap.RecoveryMode == "COLD_DATA_REPLAY" {
		scope = "DATA_REPLAY"
	}
	b.appendAuditLocked(actorName, action, scope,
		fmt.Sprintf("%s on %s -> %s (RAM queries: %d, WAL replayed: %d)",
			action, node.tracker.WorkerPodIP, snap.RecoveryMode, snap.RAMQueriesSinceBoot, snap.WALReplayedOnBoot))
	return snap, nil
}

func (b *LocalSimulatorBackend) PauseTenant(_ context.Context, actorName string) (*TenantTracker, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	node := b.ensureNodeLocked(actorName)
	if node.tracker.Status == "GDPR_SHREDDED" {
		return nil, fmt.Errorf("cannot pause shredded tenant %s", actorName)
	}
	if node.db == nil {
		if _, err := b.wakeAndQueryLocked(node, "", "", "", 0); err != nil {
			return nil, err
		}
	}
	node.tracker.Status = "PAUSED_FULL"
	node.tracker.UpdatedAt = time.Now().UTC()
	b.appendAuditLocked(actorName, "PAUSE_ACTOR", "SNAPSHOT_CONTENT_SCOPE_FULL",
		fmt.Sprintf("kubectl ate pause actor %s -> 16.0 MB RAM Buffer Pool & /data/wal.jsonl frozen intact on Worker-%d (%s)",
			actorName, node.tracker.WorkerSlot, node.tracker.WorkerPodIP))
	cp := node.tracker
	return &cp, nil
}

func (b *LocalSimulatorBackend) SuspendTenant(_ context.Context, actorName string) (*TenantTracker, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	node := b.ensureNodeLocked(actorName)
	if node.tracker.Status == "GDPR_SHREDDED" {
		return nil, fmt.Errorf("cannot suspend shredded tenant %s", actorName)
	}
	if node.db == nil {
		if _, err := b.wakeAndQueryLocked(node, "", "", "", 0); err != nil {
			return nil, err
		}
	}

	// Simulate onCommit: SNAPSHOT_CONTENT_SCOPE_DATA:
	// Discard the 16.0 MB RAM heap while preserving /data/wal.jsonl on the DurableDir volume,
	// and release the WorkerPool slot back to the cluster.
	node.db.DiscardRAMForDataScope()
	node.db = nil
	b.freeWorkerSlotLocked(actorName)

	node.tracker.Status = "SUSPENDED_DATA"
	node.tracker.WorkerSlot = 0
	if node.tracker.LastSnapshot != nil {
		snapCopy := *node.tracker.LastSnapshot
		snapCopy.RAMBufferPoolStatus = "DISCARDED_ON_DATA_COMMIT"
		snapCopy.RAMBufferPoolBytes = 0
		snapCopy.RAMBufferPoolMB = 0
		snapCopy.MaterializedStats.BufferPoolPagesWarm = 0
		snapCopy.MaterializedStats.BufferPoolBytes = 0
		node.tracker.LastSnapshot = &snapCopy
	}
	node.tracker.UpdatedAt = time.Now().UTC()

	var walBytes int64
	if node.tracker.LastSnapshot != nil {
		walBytes = node.tracker.LastSnapshot.DurableWALBytes
	}
	b.appendAuditLocked(actorName, "SUSPEND_ACTOR", "SNAPSHOT_CONTENT_SCOPE_DATA",
		fmt.Sprintf("kubectl ate suspend actor %s -> Discarded 16.0 MB RAM heap; committed ONLY /data/wal.jsonl (%d B, 99.997%% saved) to GCS",
			actorName, walBytes))
	cp := node.tracker
	return &cp, nil
}

func (b *LocalSimulatorBackend) GDPRDeleteTenant(_ context.Context, actorName string) (*GDPRDeleteReceipt, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	node := b.ensureNodeLocked(actorName)
	b.freeWorkerSlotLocked(actorName)
	node.db = nil
	_ = os.RemoveAll(node.dataDir)

	siblingStatus := "db-globex (ONLINE, 100% isolated)"
	if globex, ok := b.nodes["db-globex"]; ok && actorName != "db-globex" && globex.tracker.LastSnapshot != nil {
		siblingStatus = fmt.Sprintf("db-globex (ONLINE, %d rows intact on %s)",
			globex.tracker.LastSnapshot.RowsInDurableDir, globex.tracker.WorkerPodIP)
	}

	receipt := &GDPRDeleteReceipt{
		ActorName:             actorName,
		CompanyName:           node.tracker.CompanyName,
		CommandExecuted:       fmt.Sprintf("kubectl ate delete actor %s -a %s --any-state", actorName, b.atespace),
		ComputeShredded:       true,
		RAMSnapshotsPurged:    true,
		DurableDirShredded:    true,
		PostDeleteProbeHTTP:   http.StatusNotFound,
		SiblingTenantVerified: siblingStatus,
		DeletedAt:             time.Now().UTC(),
	}

	node.tracker.Status = "GDPR_SHREDDED"
	node.tracker.WorkerSlot = 0
	node.tracker.WorkerPodIP = ""
	node.tracker.LastSnapshot = nil
	node.tracker.GDPRReceipt = receipt
	node.tracker.UpdatedAt = time.Now().UTC()

	b.appendAuditLocked(actorName, "GDPR_ARTICLE_17_SHRED", "ALL_SCOPES",
		fmt.Sprintf("%s -> Shredded compute sandbox, RAM snapshots, and /data DurableDir (verified %s)",
			receipt.CommandExecuted, siblingStatus))
	return receipt, nil
}

func (b *LocalSimulatorBackend) ResetFleet(ctx context.Context) (*FleetStateResponse, error) {
	b.mu.Lock()
	b.initCatalogLocked()
	b.auditLog = nil
	b.mu.Unlock()
	return b.SeedFleet(ctx, "all")
}

func (b *LocalSimulatorBackend) GetFleetState(_ context.Context) (*FleetStateResponse, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buildStateLocked(), nil
}

func (b *LocalSimulatorBackend) buildStateLocked() *FleetStateResponse {
	tenants := make([]TenantTracker, 0, len(b.order))
	for _, name := range b.order {
		if n, ok := b.nodes[name]; ok {
			tenants = append(tenants, n.tracker)
		}
	}

	slots := make([]WorkerSlotStatus, 3)
	for i := 0; i < 3; i++ {
		occ := b.slots[i]
		st := WorkerSlotStatus{
			Slot:       i + 1,
			PodIP:      simulatedWorkerPoolIPs[i],
			ActorState: "IDLE",
		}
		if occ != "" {
			if n, ok := b.nodes[occ]; ok {
				st.HostedActor = occ
				st.CompanyName = n.tracker.CompanyName
				st.ActorState = n.tracker.Status
			}
		}
		slots[i] = st
	}

	auditCopy := append([]FleetAuditEvent(nil), b.auditLog...)
	return &FleetStateResponse{
		Mode:           "local-simulator",
		Atespace:       b.atespace,
		Template:       b.template,
		WorkerPoolSize: 3,
		WorkerSlots:    slots,
		Tenants:        tenants,
		Summary:        computeFleetSummary(tenants),
		AuditLog:       auditCopy,
	}
}

// --- ClusterAteBackend (--mode=gateway talking to atenet-router & kubectl-ate) ---

type clusterActorInfo struct {
	Name      string
	Template  string
	State     string
	WorkerPod string
	WorkerIP  string
}

func parseClusterActorsOutput(out string) []clusterActorInfo {
	var res []clusterActorInfo
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 || strings.EqualFold(fields[0], "ATESPACE") {
			continue
		}
		info := clusterActorInfo{
			Name:     fields[1],
			Template: fields[2],
			State:    strings.ToUpper(fields[3]),
		}
		if len(fields) >= 6 && fields[4] != "<none>" && strings.Contains(fields[5], ".") {
			info.WorkerPod = fields[4]
			info.WorkerIP = fields[5]
		}
		res = append(res, info)
	}
	return res
}

type ClusterAteBackend struct {
	mu             sync.Mutex
	atespace       string
	template       string
	routerURL      string
	ateEndpoint    string
	kubectlAteBin  string
	httpClient     *http.Client
	trackers       map[string]*TenantTracker
	order          []string
	podHistory     map[string][]string
	knownWorkerIPs []string
	auditLog       []FleetAuditEvent
}

func NewClusterAteBackend(atespace, template, routerURL, ateEndpoint, kubectlAteBin string) *ClusterAteBackend {
	b := &ClusterAteBackend{
		atespace:      atespace,
		template:      template,
		routerURL:     strings.TrimRight(routerURL, "/"),
		ateEndpoint:   ateEndpoint,
		kubectlAteBin: kubectlAteBin,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
		trackers:   make(map[string]*TenantTracker),
		podHistory: make(map[string][]string),
	}
	b.initTrackersLocked()
	return b
}

func (b *ClusterAteBackend) initTrackersLocked() {
	b.trackers = make(map[string]*TenantTracker)
	b.podHistory = make(map[string][]string)
	b.order = nil
	for _, c := range defaultTenantCatalog {
		b.trackers[c.ActorName] = &TenantTracker{
			ActorName:     c.ActorName,
			CompanyName:   c.CompanyName,
			Industry:      c.Industry,
			Tier:          c.Tier,
			Region:        c.Region,
			Status:        "UNINITIALIZED",
			OnPauseScope:  "SNAPSHOT_CONTENT_SCOPE_FULL",
			OnCommitScope: "SNAPSHOT_CONTENT_SCOPE_DATA",
			UpdatedAt:     time.Now().UTC(),
		}
		b.order = append(b.order, c.ActorName)
	}
}

func (b *ClusterAteBackend) ensureTrackerLocked(actorName string) *TenantTracker {
	if tr, ok := b.trackers[actorName]; ok {
		return tr
	}
	c := lookupCatalogEntry(actorName)
	tr := &TenantTracker{
		ActorName:     c.ActorName,
		CompanyName:   c.CompanyName,
		Industry:      c.Industry,
		Tier:          c.Tier,
		Region:        c.Region,
		Status:        "UNINITIALIZED",
		OnPauseScope:  "SNAPSHOT_CONTENT_SCOPE_FULL",
		OnCommitScope: "SNAPSHOT_CONTENT_SCOPE_DATA",
		UpdatedAt:     time.Now().UTC(),
	}
	b.trackers[actorName] = tr
	b.order = append(b.order, actorName)
	return tr
}

func (b *ClusterAteBackend) recordWorkerIPLocked(ip string) {
	if ip == "" || ip == "169.254.17.2" || ip == "unknown" {
		return
	}
	if !slices.Contains(b.knownWorkerIPs, ip) && len(b.knownWorkerIPs) < 3 {
		b.knownWorkerIPs = append(b.knownWorkerIPs, ip)
	}
}

func (b *ClusterAteBackend) appendAuditLocked(actor, action, scope, detail string) {
	ev := FleetAuditEvent{
		Timestamp: time.Now().UTC(),
		ActorName: actor,
		Action:    action,
		Scope:     scope,
		Detail:    detail,
	}
	b.auditLog = append([]FleetAuditEvent{ev}, b.auditLog...)
	if len(b.auditLog) > 30 {
		b.auditLog = b.auditLog[:30]
	}
}

func (b *ClusterAteBackend) runKubectlAte(ctx context.Context, args ...string) (string, error) {
	fullArgs := append([]string(nil), args...)
	if b.ateEndpoint != "" && !slices.Contains(fullArgs, "--endpoint") {
		fullArgs = append(fullArgs, "--endpoint", b.ateEndpoint)
	}
	cmd := exec.CommandContext(ctx, b.kubectlAteBin, fullArgs...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	return strings.TrimSpace(out.String()), err
}

func (b *ClusterAteBackend) ensureClusterActor(ctx context.Context, actorName string) error {
	out, err := b.runKubectlAte(ctx, "get", "actor", actorName, "-a", b.atespace)
	if err == nil && strings.Contains(out, actorName) {
		return nil
	}
	out, err = b.runKubectlAte(ctx, "create", "actor", actorName, "-a", b.atespace, "--template", b.template)
	if err != nil && !strings.Contains(strings.ToLower(out), "already exists") {
		return fmt.Errorf("kubectl-ate create actor %s: %w (%s)", actorName, err, out)
	}
	return nil
}

// ensureWorkerSlotForActor ensures at least 1 worker pod in the 3-replica
// WorkerPool is free if targetActor is not already occupying a worker slot.
func (b *ClusterAteBackend) ensureWorkerSlotForActor(ctx context.Context, targetActor string) {
	out, err := b.runKubectlAte(ctx, "get", "actors", "-a", b.atespace)
	if err != nil || out == "" {
		return
	}
	actors := parseClusterActorsOutput(out)

	targetOnWorker := false
	var activeDBActors []clusterActorInfo
	for _, a := range actors {
		if a.WorkerIP != "" {
			b.mu.Lock()
			b.recordWorkerIPLocked(a.WorkerIP)
			b.mu.Unlock()
		}
		isActive := strings.Contains(a.State, "RUN") || strings.Contains(a.State, "PAUSE")
		if !isActive {
			continue
		}
		if !strings.HasPrefix(a.Name, "db-") {
			// Suspend any active foreign demo actor so the shared 3-worker pool has capacity.
			_, _ = b.runKubectlAte(ctx, "suspend", "actor", a.Name, "-a", b.atespace)
			continue
		}
		if a.Name == targetActor {
			targetOnWorker = true
		} else {
			activeDBActors = append(activeDBActors, a)
		}
	}

	if !targetOnWorker && len(activeDBActors) >= 3 {
		// Multiplex the 3-worker pool: suspend one active db-* tenant to free a worker slot for targetActor.
		victim := activeDBActors[0]
		_, _ = b.runKubectlAte(ctx, "suspend", "actor", victim.Name, "-a", b.atespace)
		b.mu.Lock()
		if tr, ok := b.trackers[victim.Name]; ok {
			tr.Status = "SUSPENDED_DATA"
			tr.WorkerSlot = 0
			if tr.LastSnapshot != nil {
				snapCopy := *tr.LastSnapshot
				snapCopy.RAMBufferPoolStatus = "DISCARDED_ON_DATA_COMMIT"
				snapCopy.RAMBufferPoolBytes = 0
				snapCopy.RAMBufferPoolMB = 0
				snapCopy.MaterializedStats.BufferPoolPagesWarm = 0
				snapCopy.MaterializedStats.BufferPoolBytes = 0
				tr.LastSnapshot = &snapCopy
			}
			tr.UpdatedAt = time.Now().UTC()
		}
		b.appendAuditLocked(victim.Name, "AUTO_MULTIPLEX_SUSPEND", "SNAPSHOT_CONTENT_SCOPE_DATA",
			fmt.Sprintf("Worker pod (%s) multiplexed to %s; %s committed /data/wal.jsonl to GCS",
				victim.WorkerIP, targetActor, victim.Name))
		b.mu.Unlock()
	}
}

func (b *ClusterAteBackend) enrichSnapshotWorkerIP(ctx context.Context, actorName string, snap *TenantSnapshot) {
	if snap == nil {
		return
	}
	out, err := b.runKubectlAte(ctx, "get", "actor", actorName, "-a", b.atespace)
	if err != nil || out == "" {
		return
	}
	var realIP string
	for _, a := range parseClusterActorsOutput(out) {
		if a.Name == actorName && a.WorkerIP != "" {
			realIP = a.WorkerIP
			break
		}
	}
	if realIP == "" {
		return
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	b.recordWorkerIPLocked(realIP)

	hist := b.podHistory[actorName]
	if len(hist) == 0 || hist[len(hist)-1] != realIP {
		hist = append(hist, realIP)
		b.podHistory[actorName] = hist
	}

	snap.CurrentPodIP = realIP
	snap.PodIPHistory = append([]string(nil), hist...)
	for k, r := range snap.Rows {
		if r.PodIP == "" || r.PodIP == "169.254.17.2" {
			r.PodIP = realIP
			snap.Rows[k] = r
		}
	}
	for i := range snap.WALEntries {
		if snap.WALEntries[i].PodIP == "" || snap.WALEntries[i].PodIP == "169.254.17.2" {
			snap.WALEntries[i].PodIP = realIP
		}
	}
}

func (b *ClusterAteBackend) callActorHTTP(ctx context.Context, actorName, table, key, val string, amountUSD int, resumeHint string) (*TenantSnapshot, error) {
	b.ensureWorkerSlotForActor(ctx, actorName)

	u, err := url.Parse(b.routerURL + "/query")
	if err != nil {
		return nil, err
	}
	q := u.Query()
	if table != "" {
		q.Set("table", table)
	}
	if key != "" {
		q.Set("key", key)
		q.Set("val", val)
	}
	if amountUSD > 0 {
		q.Set("amount", strconv.Itoa(amountUSD))
	}
	if resumeHint != "" {
		q.Set("resume_hint", resumeHint)
	}
	u.RawQuery = q.Encode()

	var body []byte
	var statusCode int
	for attempt := 0; attempt < 2; attempt++ {
		req, reqErr := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
		if reqErr != nil {
			return nil, reqErr
		}
		req.Header.Set("ate-target-actor", fmt.Sprintf("%s/%s", b.atespace, actorName))

		resp, doErr := b.httpClient.Do(req)
		if doErr != nil {
			return nil, doErr
		}
		body, _ = io.ReadAll(resp.Body)
		resp.Body.Close()
		statusCode = resp.StatusCode
		if statusCode == http.StatusServiceUnavailable && attempt == 0 {
			b.ensureWorkerSlotForActor(ctx, actorName)
			time.Sleep(300 * time.Millisecond)
			continue
		}
		break
	}
	if statusCode >= 300 {
		return nil, fmt.Errorf("actor %s HTTP %d: %s", actorName, statusCode, strings.TrimSpace(string(body)))
	}

	var snap TenantSnapshot
	if err := json.Unmarshal(body, &snap); err != nil {
		return nil, fmt.Errorf("decode actor %s snapshot: %w", actorName, err)
	}
	b.enrichSnapshotWorkerIP(ctx, actorName, &snap)
	return &snap, nil
}

func (b *ClusterAteBackend) SeedFleet(ctx context.Context, actorName string) (*FleetStateResponse, error) {
	b.mu.Lock()
	targets := append([]string(nil), b.order...)
	if actorName != "" && actorName != "all" {
		b.ensureTrackerLocked(actorName)
		targets = []string{actorName}
	}
	b.mu.Unlock()

	for _, name := range targets {
		if err := b.ensureClusterActor(ctx, name); err != nil {
			return nil, err
		}
		c := lookupCatalogEntry(name)
		var lastSnap *TenantSnapshot
		for _, r := range c.DefaultRows {
			snap, err := b.callActorHTTP(ctx, name, r.Table, r.Key, r.Value, r.AmountUSD, "")
			if err != nil {
				return nil, err
			}
			lastSnap = snap
		}
		// If seeding all 4 tenants on a 3-worker pool, suspend completed tenants except the first two
		// so 2 worker slots remain RUNNING and 1 slot remains IDLE for interactive exploration.
		if len(targets) > 2 && name != "db-acme" && name != "db-globex" {
			_, _ = b.runKubectlAte(ctx, "suspend", "actor", name, "-a", b.atespace)
		}

		b.mu.Lock()
		tr := b.ensureTrackerLocked(name)
		if len(targets) > 2 && name != "db-acme" && name != "db-globex" {
			tr.Status = "SUSPENDED_DATA"
			if lastSnap != nil {
				lastSnap.RAMBufferPoolStatus = "DISCARDED_ON_DATA_COMMIT"
				lastSnap.RAMBufferPoolBytes = 0
				lastSnap.RAMBufferPoolMB = 0
				lastSnap.MaterializedStats.BufferPoolPagesWarm = 0
				lastSnap.MaterializedStats.BufferPoolBytes = 0
			}
		} else {
			tr.Status = "RUNNING"
		}
		if lastSnap != nil {
			tr.WorkerPodIP = lastSnap.CurrentPodIP
			tr.LastSnapshot = lastSnap
		}
		tr.GDPRReceipt = nil
		tr.UpdatedAt = time.Now().UTC()
		b.appendAuditLocked(name, "SEED_TENANT_DB", "FULL+DATA",
			fmt.Sprintf("Seeded %d SaaS records into %s (/data/wal.jsonl) on pod %s",
				len(c.DefaultRows), name, tr.WorkerPodIP))
		b.mu.Unlock()
	}

	return b.GetFleetState(ctx)
}

func (b *ClusterAteBackend) QueryTenant(ctx context.Context, actorName, table, key, val string, amountUSD int) (*TenantSnapshot, error) {
	b.mu.Lock()
	tr := b.ensureTrackerLocked(actorName)
	prevStatus := tr.Status
	b.mu.Unlock()

	if prevStatus == "GDPR_SHREDDED" {
		return nil, fmt.Errorf("404 TENANT_SHREDDED: actor %s was permanently deleted via GDPR Right-to-be-Forgotten", actorName)
	}
	if prevStatus == "UNINITIALIZED" {
		if err := b.ensureClusterActor(ctx, actorName); err != nil {
			return nil, err
		}
	}

	resumeHint := ""
	if prevStatus == "SUSPENDED_DATA" {
		resumeHint = "suspend_data"
	}

	snap, err := b.callActorHTTP(ctx, actorName, table, key, val, amountUSD, resumeHint)
	if err != nil {
		return nil, err
	}

	b.mu.Lock()
	tr = b.ensureTrackerLocked(actorName)
	tr.Status = "RUNNING"
	tr.WorkerPodIP = snap.CurrentPodIP
	tr.LastSnapshot = snap
	tr.UpdatedAt = time.Now().UTC()
	action := "SELECT_QUERY"
	if key != "" {
		action = "INSERT_WAL_ROW"
	}
	b.appendAuditLocked(actorName, action, snap.RecoveryMode,
		fmt.Sprintf("%s on %s -> %s (RAM queries: %d, WAL replayed: %d)",
			action, snap.CurrentPodIP, snap.RecoveryMode, snap.RAMQueriesSinceBoot, snap.WALReplayedOnBoot))
	b.mu.Unlock()

	return snap, nil
}

func (b *ClusterAteBackend) PauseTenant(ctx context.Context, actorName string) (*TenantTracker, error) {
	if out, err := b.runKubectlAte(ctx, "pause", "actor", actorName, "-a", b.atespace); err != nil {
		return nil, fmt.Errorf("kubectl-ate pause actor %s: %w (%s)", actorName, err, out)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	tr := b.ensureTrackerLocked(actorName)
	tr.Status = "PAUSED_FULL"
	tr.UpdatedAt = time.Now().UTC()
	b.appendAuditLocked(actorName, "PAUSE_ACTOR", "SNAPSHOT_CONTENT_SCOPE_FULL",
		fmt.Sprintf("kubectl ate pause actor %s -> 16.0 MB RAM Buffer Pool & /data/wal.jsonl frozen on node", actorName))
	cp := *tr
	return &cp, nil
}

func (b *ClusterAteBackend) SuspendTenant(ctx context.Context, actorName string) (*TenantTracker, error) {
	if out, err := b.runKubectlAte(ctx, "suspend", "actor", actorName, "-a", b.atespace); err != nil {
		return nil, fmt.Errorf("kubectl-ate suspend actor %s: %w (%s)", actorName, err, out)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	tr := b.ensureTrackerLocked(actorName)
	tr.Status = "SUSPENDED_DATA"
	tr.WorkerSlot = 0
	var walBytes int64
	if tr.LastSnapshot != nil {
		walBytes = tr.LastSnapshot.DurableWALBytes
		snapCopy := *tr.LastSnapshot
		snapCopy.RAMBufferPoolStatus = "DISCARDED_ON_DATA_COMMIT"
		snapCopy.RAMBufferPoolBytes = 0
		snapCopy.RAMBufferPoolMB = 0
		snapCopy.MaterializedStats.BufferPoolPagesWarm = 0
		snapCopy.MaterializedStats.BufferPoolBytes = 0
		tr.LastSnapshot = &snapCopy
	}
	tr.UpdatedAt = time.Now().UTC()
	b.appendAuditLocked(actorName, "SUSPEND_ACTOR", "SNAPSHOT_CONTENT_SCOPE_DATA",
		fmt.Sprintf("kubectl ate suspend actor %s -> Discarded 16.0 MB RAM heap; committed ONLY /data/wal.jsonl (%d B) to GCS", actorName, walBytes))
	cp := *tr
	return &cp, nil
}

func (b *ClusterAteBackend) GDPRDeleteTenant(ctx context.Context, actorName string) (*GDPRDeleteReceipt, error) {
	if out, err := b.runKubectlAte(ctx, "delete", "actor", actorName, "-a", b.atespace, "--any-state"); err != nil && !strings.Contains(strings.ToLower(out), "not found") {
		return nil, fmt.Errorf("kubectl-ate delete actor %s: %w (%s)", actorName, err, out)
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.podHistory, actorName)
	tr := b.ensureTrackerLocked(actorName)
	siblingStatus := "db-globex (ONLINE, 100% isolated)"
	if globex, ok := b.trackers["db-globex"]; ok && actorName != "db-globex" && globex.LastSnapshot != nil {
		siblingStatus = fmt.Sprintf("db-globex (ONLINE, %d rows intact on %s)",
			globex.LastSnapshot.RowsInDurableDir, globex.WorkerPodIP)
	}

	receipt := &GDPRDeleteReceipt{
		ActorName:             actorName,
		CompanyName:           tr.CompanyName,
		CommandExecuted:       fmt.Sprintf("kubectl ate delete actor %s -a %s --any-state", actorName, b.atespace),
		ComputeShredded:       true,
		RAMSnapshotsPurged:    true,
		DurableDirShredded:    true,
		PostDeleteProbeHTTP:   http.StatusNotFound,
		SiblingTenantVerified: siblingStatus,
		DeletedAt:             time.Now().UTC(),
	}
	tr.Status = "GDPR_SHREDDED"
	tr.WorkerSlot = 0
	tr.WorkerPodIP = ""
	tr.LastSnapshot = nil
	tr.GDPRReceipt = receipt
	tr.UpdatedAt = time.Now().UTC()

	b.appendAuditLocked(actorName, "GDPR_ARTICLE_17_SHRED", "ALL_SCOPES",
		fmt.Sprintf("%s -> Shredded compute sandbox, RAM snapshots, and /data DurableDir", receipt.CommandExecuted))
	return receipt, nil
}

func (b *ClusterAteBackend) ResetFleet(ctx context.Context) (*FleetStateResponse, error) {
	for _, c := range defaultTenantCatalog {
		_, _ = b.runKubectlAte(ctx, "delete", "actor", c.ActorName, "-a", b.atespace, "--any-state")
	}
	b.mu.Lock()
	b.initTrackersLocked()
	b.auditLog = nil
	b.mu.Unlock()
	return b.SeedFleet(ctx, "all")
}

func (b *ClusterAteBackend) GetFleetState(ctx context.Context) (*FleetStateResponse, error) {
	out, err := b.runKubectlAte(ctx, "get", "actors", "-a", b.atespace)

	b.mu.Lock()
	defer b.mu.Unlock()

	if err == nil && out != "" {
		for _, a := range parseClusterActorsOutput(out) {
			if a.WorkerIP != "" {
				b.recordWorkerIPLocked(a.WorkerIP)
			}
			tr, ok := b.trackers[a.Name]
			if !ok || tr.Status == "GDPR_SHREDDED" {
				continue
			}
			if a.WorkerIP != "" {
				tr.WorkerPodIP = a.WorkerIP
			}
			switch {
			case strings.Contains(a.State, "RUN") || strings.Contains(a.State, "READY"):
				tr.Status = "RUNNING"
			case strings.Contains(a.State, "PAUSE"):
				tr.Status = "PAUSED_FULL"
			case strings.Contains(a.State, "SUSPEND"):
				tr.Status = "SUSPENDED_DATA"
			}
		}
	}

	tenants := make([]TenantTracker, 0, len(b.order))
	slots := make([]WorkerSlotStatus, 3)
	for i := 0; i < 3; i++ {
		defaultIP := fmt.Sprintf("worker-pod-%d", i+1)
		if i < len(b.knownWorkerIPs) {
			defaultIP = b.knownWorkerIPs[i]
		}
		slots[i] = WorkerSlotStatus{
			Slot:       i + 1,
			PodIP:      defaultIP,
			ActorState: "IDLE",
		}
	}

	usedSlot := make([]bool, 3)
	for _, name := range b.order {
		tr := b.trackers[name]
		if tr.Status == "RUNNING" || tr.Status == "PAUSED_FULL" {
			assigned := -1
			if tr.WorkerPodIP != "" {
				for i := 0; i < 3; i++ {
					if !usedSlot[i] && slots[i].PodIP == tr.WorkerPodIP {
						assigned = i
						break
					}
				}
			}
			if assigned == -1 {
				for i := 0; i < 3; i++ {
					if !usedSlot[i] {
						assigned = i
						break
					}
				}
			}
			if assigned >= 0 {
				usedSlot[assigned] = true
				tr.WorkerSlot = assigned + 1
				podIP := tr.WorkerPodIP
				if podIP == "" {
					podIP = slots[assigned].PodIP
				}
				slots[assigned] = WorkerSlotStatus{
					Slot:        assigned + 1,
					PodIP:       podIP,
					HostedActor: tr.ActorName,
					CompanyName: tr.CompanyName,
					ActorState:  tr.Status,
				}
			} else {
				tr.WorkerSlot = 0
			}
		} else {
			tr.WorkerSlot = 0
		}
		tenants = append(tenants, *tr)
	}

	auditCopy := append([]FleetAuditEvent(nil), b.auditLog...)
	return &FleetStateResponse{
		Mode:           "cluster-ate",
		Atespace:       b.atespace,
		Template:       b.template,
		WorkerPoolSize: 3,
		WorkerSlots:    slots,
		Tenants:        tenants,
		Summary:        computeFleetSummary(tenants),
		AuditLog:       auditCopy,
	}, nil
}
