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
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"time"
)

var clusterNodeNames = []string{"raft-node-1", "raft-node-2", "raft-node-3"}

var simulatedWorkerPodIPs = []string{"10.44.0.9", "10.44.1.14", "10.44.2.21"}

type NodeTrackerState struct {
	ActorName      string               `json:"actor_name"`
	SubstrateState string               `json:"substrate_state"` // RUNNING | SUSPENDED
	SuspendedAt    *time.Time           `json:"suspended_at,omitempty"`
	FrozenDriftMS  int64                `json:"frozen_drift_ms"`
	WorkerPodIP    string               `json:"worker_pod_ip"`
	ActorStatus    *ActorStatusResponse `json:"actor_status,omitempty"`
}

type ClusterEvent struct {
	Step        int       `json:"step"`
	Timestamp   time.Time `json:"timestamp"`
	Severity    string    `json:"severity"` // INFO | WARN | CRITICAL | GUARDED
	Title       string    `json:"title"`
	Description string    `json:"description"`
}

type KVConflict struct {
	Key         string `json:"key"`
	Node1Value  string `json:"node1_value"`
	Node1Rev    int    `json:"node1_rev"`
	Node1Term   int    `json:"node1_term"`
	QuorumValue string `json:"quorum_value"`
	QuorumRev   int    `json:"quorum_rev"`
	QuorumTerm  int    `json:"quorum_term"`
}

type ClusterLabState struct {
	ScenarioStep       int                `json:"scenario_step"`
	ScenarioPhase      string             `json:"scenario_phase"`
	ScenarioSummary    string             `json:"scenario_summary"`
	ElectionPending    bool               `json:"election_pending"`
	ClockJumpGuard     bool               `json:"clock_jump_guard"`
	UniverseWallTime   time.Time          `json:"universe_wall_time"`
	Node1HeapTime      time.Time          `json:"node1_heap_time"`
	ClockDriftMS       int64              `json:"clock_drift_ms"`
	LeaseTTLMS         int64              `json:"lease_ttl_ms"`
	SplitBrainActive   bool               `json:"split_brain_active"`
	SplitBrainGuarded  bool               `json:"split_brain_guarded"`
	VaultResourceID    string             `json:"vault_resource_id"`
	TotalPhysicalUnits int                `json:"total_physical_units"`
	AllocatedUnits     int                `json:"allocated_units"`
	AvailableUnits     int                `json:"available_units"`
	DoubleSpendActive  bool               `json:"double_spend_active"`
	LogForkDetected    bool               `json:"log_fork_detected"`
	ActiveLeaders      []string           `json:"active_leaders"`
	ConflictingLocks   []LockEntry        `json:"conflicting_locks"`
	KVConflicts        []KVConflict       `json:"kv_conflicts"`
	Nodes              []NodeTrackerState `json:"nodes"`
	Events             []ClusterEvent     `json:"events"`
}

type TimewarpBackend interface {
	GetState(ctx context.Context) (*ClusterLabState, error)
	Step1Quorum(ctx context.Context) (*ClusterLabState, error)
	Step2FreezeAndElect(ctx context.Context) (*ClusterLabState, error)
	Step2FreezeLeaderAsync(ctx context.Context) (*ClusterLabState, error)
	Step3WakeZombie(ctx context.Context) (*ClusterLabState, error)
	Step4ReplayGuardrail(ctx context.Context) (*ClusterLabState, error)
	PutKV(ctx context.Context, targetNode, key, value string) (*ClusterLabState, int, error)
	SetGuardrail(ctx context.Context, enabled bool) (*ClusterLabState, error)
	Reset(ctx context.Context) (*ClusterLabState, error)
}

func computeClusterSummary(
	step int,
	phase, summary string,
	electionPending bool,
	guard bool,
	universeNow time.Time,
	node1HeapTime time.Time,
	driftMS int64,
	nodes []NodeTrackerState,
	events []ClusterEvent,
) *ClusterLabState {
	var activeLeaders []string
	var conflictingLocks []LockEntry
	splitBrain := false
	guarded := false
	allocatedUnits := 0
	var index2Signatures []string

	var n1Status *ActorStatusResponse
	var quorumStatus *ActorStatusResponse

	for _, n := range nodes {
		if n.ActorStatus == nil {
			continue
		}
		role := n.ActorStatus.Role
		if n.SubstrateState == "RUNNING" && (role == "LEADER" || role == "ZOMBIE_LEADER") {
			activeLeaders = append(activeLeaders, fmt.Sprintf("%s (%s Term %d)", n.ActorName, role, n.ActorStatus.Term))
			if n.ActorStatus.CurrentLock != nil {
				conflictingLocks = append(conflictingLocks, *n.ActorStatus.CurrentLock)
				if n.ActorStatus.CurrentLock.LogIndex >= 2 {
					allocatedUnits++
				}
			}
			if n.ActorName == "raft-node-1" {
				n1Status = n.ActorStatus
			} else if quorumStatus == nil {
				quorumStatus = n.ActorStatus
			}
		}
		if role == "STALE_LEADER_FENCED" {
			guarded = true
		}
		for _, entry := range n.ActorStatus.CommitHistory {
			if entry.LogIndex == 2 {
				sig := fmt.Sprintf("T%d:%s", entry.Term, entry.Owner)
				if !slices.Contains(index2Signatures, sig) {
					index2Signatures = append(index2Signatures, sig)
				}
			}
		}
	}

	if len(activeLeaders) > 1 {
		splitBrain = true
	}

	var kvConflicts []KVConflict
	if splitBrain && n1Status != nil && quorumStatus != nil {
		n1Map := make(map[string]KVEntry, len(n1Status.KVStore))
		var keyOrder []string
		for _, e := range n1Status.KVStore {
			n1Map[e.Key] = e
			keyOrder = append(keyOrder, e.Key)
		}
		qMap := make(map[string]KVEntry, len(quorumStatus.KVStore))
		for _, e := range quorumStatus.KVStore {
			qMap[e.Key] = e
			if !slices.Contains(keyOrder, e.Key) {
				keyOrder = append(keyOrder, e.Key)
			}
		}
		for _, k := range keyOrder {
			e1, ok1 := n1Map[k]
			eq, okq := qMap[k]
			val1 := "(missing)"
			if ok1 {
				val1 = e1.Value
			}
			valQ := "(missing)"
			if okq {
				valQ = eq.Value
			}
			if val1 != valQ {
				kvConflicts = append(kvConflicts, KVConflict{
					Key:         k,
					Node1Value:  val1,
					Node1Rev:    e1.Revision,
					Node1Term:   e1.Term,
					QuorumValue: valQ,
					QuorumRev:   eq.Revision,
					QuorumTerm:  eq.Term,
				})
			}
		}
	}

	totalUnits := 1
	availableUnits := totalUnits - allocatedUnits
	doubleSpend := allocatedUnits > totalUnits || len(kvConflicts) > 0
	logFork := len(index2Signatures) > 1

	return &ClusterLabState{
		ScenarioStep:       step,
		ScenarioPhase:      phase,
		ScenarioSummary:    summary,
		ElectionPending:    electionPending,
		ClockJumpGuard:     guard,
		UniverseWallTime:   universeNow.UTC(),
		Node1HeapTime:      node1HeapTime.UTC(),
		ClockDriftMS:       driftMS,
		LeaseTTLMS:         3000,
		SplitBrainActive:   splitBrain,
		SplitBrainGuarded:  guarded,
		VaultResourceID:    "etcd-kv-store",
		TotalPhysicalUnits: totalUnits,
		AllocatedUnits:     allocatedUnits,
		AvailableUnits:     availableUnits,
		DoubleSpendActive:  doubleSpend,
		LogForkDetected:    logFork,
		ActiveLeaders:      activeLeaders,
		ConflictingLocks:   conflictingLocks,
		KVConflicts:        kvConflicts,
		Nodes:              nodes,
		Events:             slices.Clone(events),
	}
}

// --- LocalSimulatorBackend (--mode=local & unit tests) ---

type localSimNode struct {
	actor          *NaiveClusterNode
	substrateState string
	suspendedAt    *time.Time
	frozenHeapTime time.Time
	podIdx         int
	lastStatus     ActorStatusResponse
}

type LocalSimulatorBackend struct {
	mu              sync.Mutex
	atespace        string
	template        string
	simClock        time.Time
	scenarioStep    int
	scenarioPhase   string
	scenarioSummary string
	electionPending bool
	electionGen     uint64
	guard           bool
	nodes           map[string]*localSimNode
	events          []ClusterEvent
}

func NewLocalSimulatorBackend(atespace, template string) *LocalSimulatorBackend {
	t0 := time.Now().UTC().Truncate(time.Millisecond)
	b := &LocalSimulatorBackend{
		atespace: atespace,
		template: template,
		simClock: t0,
		nodes:    make(map[string]*localSimNode),
	}
	for i, name := range clusterNodeNames {
		role := "FOLLOWER"
		if i == 0 {
			role = "LEADER"
		}
		node := newNaiveClusterNode(t0)
		podIP := simulatedWorkerPodIPs[i%len(simulatedWorkerPodIPs)]
		st := node.Reset(t0, name, role, 1, "raft-node-1", false, podIP)
		b.nodes[name] = &localSimNode{
			actor:          node,
			substrateState: "RUNNING",
			frozenHeapTime: t0,
			podIdx:         i % len(simulatedWorkerPodIPs),
			lastStatus:     st,
		}
	}
	_, _ = b.Step1Quorum(context.Background())
	return b
}

func (b *LocalSimulatorBackend) appendEventLocked(step int, severity, title, desc string) {
	b.events = append(b.events, ClusterEvent{
		Step:        step,
		Timestamp:   b.simClock.UTC(),
		Severity:    severity,
		Title:       title,
		Description: desc,
	})
}

func (b *LocalSimulatorBackend) buildStateLocked() *ClusterLabState {
	outNodes := make([]NodeTrackerState, 0, len(clusterNodeNames))
	n1 := b.nodes["raft-node-1"]
	node1Heap := b.simClock
	var driftMS int64

	if n1 != nil {
		if n1.substrateState == "SUSPENDED" {
			node1Heap = n1.frozenHeapTime
			driftMS = b.simClock.Sub(n1.frozenHeapTime).Milliseconds()
		} else if n1.lastStatus.LastObservedGapMS >= 1500 {
			driftMS = n1.lastStatus.LastObservedGapMS
		}
	}

	for _, name := range clusterNodeNames {
		sn := b.nodes[name]
		stCopy := sn.lastStatus
		var nodeDrift int64
		if sn.substrateState == "SUSPENDED" {
			nodeDrift = b.simClock.Sub(sn.frozenHeapTime).Milliseconds()
		} else if stCopy.LastObservedGapMS >= 1500 {
			nodeDrift = stCopy.LastObservedGapMS
		}
		outNodes = append(outNodes, NodeTrackerState{
			ActorName:      name,
			SubstrateState: sn.substrateState,
			SuspendedAt:    sn.suspendedAt,
			FrozenDriftMS:  nodeDrift,
			WorkerPodIP:    simulatedWorkerPodIPs[sn.podIdx],
			ActorStatus:    &stCopy,
		})
	}

	return computeClusterSummary(
		b.scenarioStep,
		b.scenarioPhase,
		b.scenarioSummary,
		b.electionPending,
		b.guard,
		b.simClock,
		node1Heap,
		driftMS,
		outNodes,
		b.events,
	)
}

func (b *LocalSimulatorBackend) GetState(_ context.Context) (*ClusterLabState, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buildStateLocked(), nil
}

func (b *LocalSimulatorBackend) Step1Quorum(_ context.Context) (*ClusterLabState, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.electionGen++
	b.electionPending = false
	b.simClock = time.Now().UTC().Truncate(time.Millisecond)
	b.events = nil

	for i, name := range clusterNodeNames {
		role := "FOLLOWER"
		if i == 0 {
			role = "LEADER"
		}
		sn := b.nodes[name]
		sn.podIdx = i % len(simulatedWorkerPodIPs)
		podIP := simulatedWorkerPodIPs[sn.podIdx]
		sn.substrateState = "RUNNING"
		sn.suspendedAt = nil
		sn.frozenHeapTime = b.simClock
		sn.lastStatus = sn.actor.Reset(b.simClock, name, role, 1, "raft-node-1", b.guard, podIP)
	}

	// Advance 100ms normal tick and acquire Term 1 lock for Alice on raft-node-1 (Log Index #1)
	b.simClock = b.simClock.Add(100 * time.Millisecond)
	n1 := b.nodes["raft-node-1"]
	entry, err := n1.actor.AcquireLock(b.simClock, "Alice (checkout-svc)", 101, simulatedWorkerPodIPs[n1.podIdx])
	if err != nil {
		return nil, err
	}
	n1.lastStatus = n1.actor.SnapshotStatus(b.simClock, simulatedWorkerPodIPs[n1.podIdx])
	n1.frozenHeapTime = b.simClock

	for _, followerName := range []string{"raft-node-2", "raft-node-3"} {
		fn := b.nodes[followerName]
		fn.lastStatus = fn.actor.ReplicateEntry(b.simClock, 1, "raft-node-1", entry, simulatedWorkerPodIPs[fn.podIdx])
		fn.frozenHeapTime = b.simClock
	}

	b.scenarioStep = 1
	b.scenarioPhase = "STEP 1: HEALTHY TERM 1 QUORUM (REV=1 REPLICATED TO ALL 3 NODES)"
	b.scenarioSummary = "raft-node-1 is Term 1 LEADER (3.0s lease). KV database rev=1 (/config/db_primary=postgres-east-1, /escrow/acct_balance=$10,000, /lease/checkout_owner=Alice #101) is replicated across all 3 nodes."
	b.appendEventLocked(1, "INFO", "Term 1 Quorum Established (KV rev=1)", "raft-node-1 elected LEADER (Term 1) with 3.0s lease TTL; replicated KV rev=1 (/config/db_primary=postgres-east-1, /escrow/acct_balance=$10,000, /lease/checkout_owner='Alice #101') to raft-node-2 and raft-node-3.")
	return b.buildStateLocked(), nil
}

func (b *LocalSimulatorBackend) completeLocalTerm2Locked() error {
	b.simClock = b.simClock.Add(5200 * time.Millisecond)
	n2 := b.nodes["raft-node-2"]
	n3 := b.nodes["raft-node-3"]
	n2.lastStatus = n2.actor.PromoteRole(b.simClock, "raft-node-2", "LEADER", 2, "raft-node-2", simulatedWorkerPodIPs[n2.podIdx])
	n3.lastStatus = n3.actor.PromoteRole(b.simClock, "raft-node-3", "FOLLOWER", 2, "raft-node-2", simulatedWorkerPodIPs[n3.podIdx])

	entryBob, err := n2.actor.AcquireLock(b.simClock, "Bob (inventory-svc)", 102, simulatedWorkerPodIPs[n2.podIdx])
	if err != nil {
		return err
	}
	n2.lastStatus = n2.actor.SnapshotStatus(b.simClock, simulatedWorkerPodIPs[n2.podIdx])
	n3.lastStatus = n3.actor.ReplicateEntry(b.simClock, 2, "raft-node-2", entryBob, simulatedWorkerPodIPs[n3.podIdx])
	n2.frozenHeapTime = b.simClock
	n3.frozenHeapTime = b.simClock

	b.electionPending = false
	b.scenarioStep = 2
	b.scenarioPhase = "STEP 2: 3.0s LEASE EXPIRED -> TERM 2 QUORUM COMMITTED KV REV=2 (BOB)"
	b.scenarioSummary = "While raft-node-1 is frozen in GCS (0 CPU instructions, heap frozen at Term 1 rev=1 with $10,000 in escrow), its 3.0s lease expired in the real world. Quorum (raft-node-2 & raft-node-3) elected raft-node-2 as Term 2 LEADER and committed KV rev=2 (/config/db_primary=postgres-west-2, /escrow/acct_balance=$0 paid to Bob #102)."
	b.appendEventLocked(2, "WARN", "Term 1 Lease Expired -> Term 2 Quorum Committed KV rev=2 (Bob)", "After 3.0s of silence from frozen raft-node-1, quorum (raft-node-2 + raft-node-3) promoted raft-node-2 to LEADER (Term 2) and committed KV rev=2: /config/db_primary=postgres-west-2, /escrow/acct_balance=$0 (paid $10,000 to Bob #102).")
	return nil
}

func (b *LocalSimulatorBackend) Step2FreezeAndElect(ctx context.Context) (*ClusterLabState, error) {
	b.mu.Lock()
	if b.scenarioStep < 1 {
		b.mu.Unlock()
		if _, err := b.Step1Quorum(ctx); err != nil {
			return nil, err
		}
		b.mu.Lock()
	}
	defer b.mu.Unlock()

	b.electionGen++
	n1 := b.nodes["raft-node-1"]
	suspendTime := b.simClock
	n1.substrateState = "SUSPENDED"
	n1.suspendedAt = &suspendTime
	n1.frozenHeapTime = suspendTime
	b.appendEventLocked(2, "WARN", "kubectl ate suspend actor raft-node-1", "raft-node-1 frozen to GCS (SNAPSHOT_CONTENT_SCOPE_FULL). Process executes 0 CPU instructions; heap remains frozen at Role=LEADER (Term 1), KV rev=1 (/escrow/acct_balance=$10,000).")

	if err := b.completeLocalTerm2Locked(); err != nil {
		return nil, err
	}
	return b.buildStateLocked(), nil
}

func (b *LocalSimulatorBackend) Step2FreezeLeaderAsync(ctx context.Context) (*ClusterLabState, error) {
	b.mu.Lock()
	if b.scenarioStep < 1 {
		b.mu.Unlock()
		if _, err := b.Step1Quorum(ctx); err != nil {
			return nil, err
		}
		b.mu.Lock()
	}

	b.electionGen++
	gen := b.electionGen
	n1 := b.nodes["raft-node-1"]
	suspendTime := b.simClock
	n1.substrateState = "SUSPENDED"
	n1.suspendedAt = &suspendTime
	n1.frozenHeapTime = suspendTime
	b.electionPending = true
	b.scenarioStep = 2
	b.scenarioPhase = "STEP 2A: LEADER FROZEN IN GCS — 3.0s LEASE COUNTING DOWN..."
	b.scenarioSummary = "raft-node-1 is suspended in GCS (0 CPU instructions, heap clock frozen). Watch the 3.0s Leader Lease countdown expire live to trigger the Term 2 Quorum Election on raft-node-2 & raft-node-3!"
	b.appendEventLocked(2, "WARN", "kubectl ate suspend actor raft-node-1", "raft-node-1 frozen to GCS (SNAPSHOT_CONTENT_SCOPE_FULL). Process executes 0 CPU instructions; heap remains frozen at Role=LEADER (Term 1), KV rev=1 (/escrow/acct_balance=$10,000).")
	st := b.buildStateLocked()
	b.mu.Unlock()

	go func(expectedGen uint64) {
		time.Sleep(3200 * time.Millisecond)
		b.mu.Lock()
		defer b.mu.Unlock()
		if !b.electionPending || b.electionGen != expectedGen {
			return
		}
		_ = b.completeLocalTerm2Locked()
	}(gen)

	return st, nil
}

func (b *LocalSimulatorBackend) Step3WakeZombie(ctx context.Context) (*ClusterLabState, error) {
	b.mu.Lock()
	if b.scenarioStep < 2 {
		b.mu.Unlock()
		if _, err := b.Step2FreezeAndElect(ctx); err != nil {
			return nil, err
		}
		b.mu.Lock()
	}
	if b.electionPending {
		if err := b.completeLocalTerm2Locked(); err != nil {
			b.mu.Unlock()
			return nil, err
		}
	}
	defer b.mu.Unlock()

	n1 := b.nodes["raft-node-1"]
	if n1.substrateState != "SUSPENDED" {
		b.mu.Unlock()
		if _, err := b.Step2FreezeAndElect(ctx); err != nil {
			return nil, err
		}
		b.mu.Lock()
		n1 = b.nodes["raft-node-1"]
	}

	// Wake raft-node-1 on a new worker pod IP via atenet-router!
	n1.podIdx = (n1.podIdx + 1) % len(simulatedWorkerPodIPs)
	newPodIP := simulatedWorkerPodIPs[n1.podIdx]
	n1.substrateState = "RUNNING"
	n1.suspendedAt = nil

	_, err := n1.actor.AcquireLock(b.simClock, "Mallory (rogue-worker)", 101, newPodIP)
	n1.actor.mu.Lock()
	n1.lastStatus = n1.actor.snapshotStatusLocked(b.simClock)
	n1.actor.mu.Unlock()
	n1.frozenHeapTime = b.simClock

	if err != nil {
		b.scenarioStep = 3
		b.scenarioPhase = "STEP 3 (GUARDED): WAKEUP CLOCK-JUMP FENCED STALE LEADER"
		b.scenarioSummary = fmt.Sprintf("raft-node-1 woke on pod %s after a %dms clock jump. Wakeup Clock-Jump Guardrail immediately fenced its stale Term 1 lease (STALE_LEADER_FENCED), clamped token refill to +10 tokens, and rejected 'Mallory' with 409 Conflict (KV Divergence Prevented)!", newPodIP, n1.lastStatus.LastObservedGapMS)
		b.appendEventLocked(3, "GUARDED", "Wakeup Clock-Jump Guardrail Prevented KV Split-Brain", b.scenarioSummary)
		return b.buildStateLocked(), nil
	}

	b.scenarioStep = 3
	b.scenarioPhase = "STEP 3: ZOMBIE LEADER SPLIT-BRAIN & KV DATABASE DIVERGENCE (REV=2 FORK)!"
	b.scenarioSummary = fmt.Sprintf("CRITICAL KV DIVERGENCE: atenet woke raft-node-1 from GCS onto pod %s. Its 100ms ticker jumped +%dms, its token bucket minted +%.0f burst tokens, and its stale Term 1 heap committed a conflicting KV rev=2 (paying $10,000 escrow to 'Mallory' & setting db_primary=postgres-rogue-3) while Term 2 Quorum already committed rev=2 for 'Bob'!", newPodIP, n1.lastStatus.LastObservedGapMS, n1.lastStatus.TokenBucketValue-10.0)
	b.appendEventLocked(3, "CRITICAL", "ZOMBIE LEADER KV DIVERGENCE & FORKED LOG INDEX #2", b.scenarioSummary)
	return b.buildStateLocked(), nil
}

func (b *LocalSimulatorBackend) Step4ReplayGuardrail(ctx context.Context) (*ClusterLabState, error) {
	b.mu.Lock()
	b.guard = true
	b.mu.Unlock()

	if _, err := b.Step1Quorum(ctx); err != nil {
		return nil, err
	}
	if _, err := b.Step2FreezeAndElect(ctx); err != nil {
		return nil, err
	}
	if _, err := b.Step3WakeZombie(ctx); err != nil {
		return nil, err
	}
	b.mu.Lock()
	b.scenarioStep = 4
	b.scenarioPhase = "STEP 4: GUARDRAIL ACTIVE — STALE LEASE FENCED & KV CONSISTENCY PRESERVED"
	b.mu.Unlock()
	return b.GetState(ctx)
}

func (b *LocalSimulatorBackend) PutKV(_ context.Context, targetNode, key, value string) (*ClusterLabState, int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if targetNode == "" {
		targetNode = "raft-node-2"
	}
	sn, ok := b.nodes[targetNode]
	if !ok {
		return b.buildStateLocked(), http.StatusBadRequest, fmt.Errorf("unknown node %q", targetNode)
	}
	if key == "" {
		key = "/config/db_primary"
	}

	b.simClock = b.simClock.Add(100 * time.Millisecond)
	if sn.substrateState == "SUSPENDED" {
		sn.podIdx = (sn.podIdx + 1) % len(simulatedWorkerPodIPs)
		sn.substrateState = "RUNNING"
		sn.suspendedAt = nil
	}
	podIP := simulatedWorkerPodIPs[sn.podIdx]
	entry, err := sn.actor.PutKV(b.simClock, key, value, podIP)
	sn.actor.mu.Lock()
	sn.lastStatus = sn.actor.snapshotStatusLocked(b.simClock)
	sn.actor.mu.Unlock()
	sn.frozenHeapTime = b.simClock

	if err != nil {
		sev := "WARN"
		if sn.lastStatus.Role == "STALE_LEADER_FENCED" {
			sev = "GUARDED"
		}
		b.appendEventLocked(b.scenarioStep, sev, fmt.Sprintf("Rejected PUT %s on %s", key, targetNode), err.Error())
		return b.buildStateLocked(), http.StatusConflict, err
	}

	if sn.lastStatus.Role == "LEADER" {
		for _, name := range clusterNodeNames {
			if name == targetNode {
				continue
			}
			fn := b.nodes[name]
			if fn.substrateState == "RUNNING" && fn.lastStatus.Role == "FOLLOWER" && fn.lastStatus.Term == sn.lastStatus.Term {
				fn.lastStatus = fn.actor.ReplicateKV(b.simClock, sn.lastStatus.Term, targetNode, entry, simulatedWorkerPodIPs[fn.podIdx])
				fn.frozenHeapTime = b.simClock
			}
		}
		b.appendEventLocked(b.scenarioStep, "INFO", fmt.Sprintf("Quorum PUT %s=%q (rev=%d, Term %d)", key, value, entry.Revision, entry.Term),
			fmt.Sprintf("Committed on %s (LEADER Term %d) and replicated to quorum followers.", targetNode, entry.Term))
	} else {
		b.appendEventLocked(b.scenarioStep, "CRITICAL", fmt.Sprintf("SPLIT-BRAIN PUT %s=%q on %s (Term %d)", key, value, targetNode, entry.Term),
			fmt.Sprintf("%s (%s Term %d) accepted client write without quorum! Key %s now diverges from Term 2 Quorum!", targetNode, sn.lastStatus.Role, entry.Term, key))
	}

	return b.buildStateLocked(), http.StatusOK, nil
}

func (b *LocalSimulatorBackend) SetGuardrail(ctx context.Context, enabled bool) (*ClusterLabState, error) {
	b.mu.Lock()
	b.guard = enabled
	for _, name := range clusterNodeNames {
		sn := b.nodes[name]
		sn.actor.SetGuardrail(enabled)
		sn.actor.mu.Lock()
		sn.lastStatus = sn.actor.snapshotStatusLocked(b.simClock)
		sn.actor.mu.Unlock()
	}
	b.mu.Unlock()
	return b.GetState(ctx)
}

func (b *LocalSimulatorBackend) Reset(ctx context.Context) (*ClusterLabState, error) {
	b.mu.Lock()
	b.guard = false
	b.mu.Unlock()
	return b.Step1Quorum(ctx)
}

// --- ClusterAteBackend (--mode=gateway against real Agent Substrate cluster) ---

type ClusterAteBackend struct {
	mu              sync.Mutex
	atespace        string
	template        string
	routerURL       string
	ateEndpoint     string
	kubectlAteBin   string
	httpClient      *http.Client
	scenarioStep    int
	scenarioPhase   string
	scenarioSummary string
	electionPending bool
	electionGen     uint64
	guard           bool
	created         map[string]bool
	trackers        map[string]*NodeTrackerState
	node1FrozenAt   time.Time
	events          []ClusterEvent
}

func normalizeRouterURL(raw string) string {
	trimmed := strings.TrimRight(raw, "/")
	if _, err := os.Stat("/run/router-client/credential-bundle.pem"); err == nil {
		trimmed = strings.ReplaceAll(trimmed, "atenet-router.ate-system.svc.cluster.local", "atenet-router.ate-system.svc")
		if strings.HasPrefix(trimmed, "http://atenet-router.") {
			trimmed = "https://" + strings.TrimPrefix(trimmed, "http://")
			trimmed = strings.TrimSuffix(trimmed, ":80")
			if !strings.HasSuffix(trimmed, ":443") {
				trimmed += ":443"
			}
		}
	}
	return trimmed
}

func newRouterHTTPClient(timeout time.Duration) *http.Client {
	tr := &http.Transport{
		MaxIdleConns:        128,
		MaxIdleConnsPerHost: 32,
		IdleConnTimeout:     90 * time.Second,
	}
	credPath := "/run/router-client/credential-bundle.pem"
	caPath := "/run/router-client/trust-bundle.pem"
	if _, err := os.Stat(credPath); err == nil {
		pool := x509.NewCertPool()
		if caBytes, caErr := os.ReadFile(caPath); caErr == nil {
			pool.AppendCertsFromPEM(caBytes)
		}
		tr.TLSClientConfig = &tls.Config{
			GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
				cert, err := tls.LoadX509KeyPair(credPath, credPath)
				if err != nil {
					return nil, err
				}
				return &cert, nil
			},
			RootCAs:    pool,
			ServerName: "atenet-router.ate-system.svc",
			MinVersion: tls.VersionTLS12,
		}
	}
	return &http.Client{
		Timeout:   timeout,
		Transport: tr,
	}
}

func NewClusterAteBackend(atespace, template, routerURL, ateEndpoint, kubectlAteBin string) *ClusterAteBackend {
	now := time.Now().UTC()
	b := &ClusterAteBackend{
		atespace:        atespace,
		template:        template,
		routerURL:       normalizeRouterURL(routerURL),
		ateEndpoint:     ateEndpoint,
		kubectlAteBin:   kubectlAteBin,
		httpClient:      newRouterHTTPClient(20 * time.Second),
		scenarioStep:    0,
		scenarioPhase:   "READY TO INITIALIZE 3-NODE SUBSTRATE CLUSTER",
		scenarioSummary: "Click Step 1 to initialize raft-node-1, raft-node-2, and raft-node-3 on the Agent Substrate WorkerPool.",
		created:         make(map[string]bool),
		trackers:        make(map[string]*NodeTrackerState),
		node1FrozenAt:   now,
	}
	for _, name := range clusterNodeNames {
		b.trackers[name] = &NodeTrackerState{
			ActorName:      name,
			SubstrateState: "SUSPENDED",
		}
	}
	return b
}

func (b *ClusterAteBackend) runKubectlAte(ctx context.Context, args ...string) (string, error) {
	bin := b.kubectlAteBin
	var cmdArgs []string
	if bin == "kubectl" {
		cmdArgs = append(cmdArgs, "ate")
	}
	if b.ateEndpoint != "" {
		cmdArgs = append(cmdArgs, "--endpoint", b.ateEndpoint)
	}
	cmdArgs = append(cmdArgs, args...)
	cmd := exec.CommandContext(ctx, bin, cmdArgs...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		combined := strings.TrimSpace(stderr.String() + "\n" + stdout.String())
		return stdout.String(), fmt.Errorf("%s %v: %w (%s)", bin, cmdArgs, err, combined)
	}
	return stdout.String(), nil
}

func (b *ClusterAteBackend) ensureActor(ctx context.Context, actorName string) error {
	b.mu.Lock()
	already := b.created[actorName]
	b.mu.Unlock()
	if already {
		return nil
	}
	_, err := b.runKubectlAte(ctx, "create", "actor", actorName, "-a", b.atespace, "--template", b.template)
	if err != nil && !strings.Contains(strings.ToLower(err.Error()), "already exists") {
		return err
	}
	b.mu.Lock()
	b.created[actorName] = true
	b.mu.Unlock()
	return nil
}

func (b *ClusterAteBackend) inspectRunningActor(ctx context.Context, actorName string) string {
	raw, err := b.runKubectlAte(ctx, "get", "actors", actorName, "-a", b.atespace, "-o", "json")
	if err != nil {
		return ""
	}
	var parsed struct {
		Status struct {
			WorkerAssignment struct {
				WorkerPodIP  string   `json:"workerPodIp"`
				WorkerPodIPs []string `json:"workerPodIps"`
			} `json:"workerAssignment"`
		} `json:"status"`
	}
	if json.Unmarshal([]byte(raw), &parsed) != nil {
		return ""
	}
	if parsed.Status.WorkerAssignment.WorkerPodIP != "" {
		return parsed.Status.WorkerAssignment.WorkerPodIP
	}
	if len(parsed.Status.WorkerAssignment.WorkerPodIPs) > 0 {
		return parsed.Status.WorkerAssignment.WorkerPodIPs[0]
	}
	return ""
}

func (b *ClusterAteBackend) freeWorkerSlotFor(ctx context.Context, keepActors ...string) {
	out, err := b.runKubectlAte(ctx, "get", "actors", "-a", b.atespace)
	if err != nil {
		return
	}
	var nonTimewarp []string
	var timewarpCandidates []string
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 || fields[0] == "ATESPACE" || fields[0] == "NAMESPACE" || strings.HasPrefix(fields[0], "Warning:") {
			continue
		}
		name := fields[1]
		state := fields[3]
		if state == "ACTOR_STATE_SUSPENDED" {
			continue
		}
		if slices.Contains(keepActors, name) {
			continue
		}
		if strings.HasPrefix(name, "raft-node-") {
			timewarpCandidates = append(timewarpCandidates, name)
		} else {
			nonTimewarp = append(nonTimewarp, name)
		}
	}
	for _, name := range nonTimewarp {
		_, _ = b.runKubectlAte(ctx, "suspend", "actor", name, "-a", b.atespace, "--mode=async")
	}
	if len(nonTimewarp) == 0 && len(timewarpCandidates) > 0 {
		victim := timewarpCandidates[len(timewarpCandidates)-1]
		_, _ = b.runKubectlAte(ctx, "suspend", "actor", victim, "-a", b.atespace, "--mode=async")
	}
}

func (b *ClusterAteBackend) callActorJSON(ctx context.Context, actorName, method, path, workerPodIP string, body any, out any) (int, error) {
	var rawBody []byte
	if body != nil {
		var err error
		rawBody, err = json.Marshal(body)
		if err != nil {
			return 0, err
		}
	}

	for attempt := 0; attempt < 2; attempt++ {
		var bodyReader io.Reader
		if rawBody != nil {
			bodyReader = bytes.NewReader(rawBody)
		}
		req, err := http.NewRequestWithContext(ctx, method, b.routerURL+path, bodyReader)
		if err != nil {
			return 0, err
		}
		req.Header.Set("ate-target-actor", b.atespace+"/"+actorName)
		if workerPodIP != "" {
			req.Header.Set("X-Worker-Pod-IP", workerPodIP)
		}
		if rawBody != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := b.httpClient.Do(req)
		if err != nil {
			if attempt == 0 {
				b.httpClient.CloseIdleConnections()
				continue
			}
			return 0, err
		}
		rawResp, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()

		if resp.StatusCode == http.StatusServiceUnavailable && attempt == 0 {
			b.freeWorkerSlotFor(ctx, actorName)
			continue
		}
		if out != nil && len(rawResp) > 0 {
			_ = json.Unmarshal(rawResp, out)
		}
		if resp.StatusCode >= 300 && resp.StatusCode != http.StatusConflict {
			return resp.StatusCode, fmt.Errorf("actor %s %s returned %d: %s", actorName, path, resp.StatusCode, string(rawResp))
		}
		return resp.StatusCode, nil
	}
	return 0, fmt.Errorf("actor %s %s failed after retries", actorName, path)
}

func (b *ClusterAteBackend) appendEventLocked(step int, severity, title, desc string) {
	b.events = append(b.events, ClusterEvent{
		Step:        step,
		Timestamp:   time.Now().UTC(),
		Severity:    severity,
		Title:       title,
		Description: desc,
	})
}

func (b *ClusterAteBackend) buildStateLocked() *ClusterLabState {
	now := time.Now().UTC()
	outNodes := make([]NodeTrackerState, 0, len(clusterNodeNames))
	node1Heap := now
	var driftMS int64

	if tr1, ok := b.trackers["raft-node-1"]; ok {
		if tr1.SubstrateState == "SUSPENDED" && tr1.SuspendedAt != nil {
			node1Heap = *tr1.SuspendedAt
			driftMS = now.Sub(*tr1.SuspendedAt).Milliseconds()
		} else if tr1.ActorStatus != nil && tr1.ActorStatus.LastObservedGapMS >= 1500 {
			driftMS = tr1.ActorStatus.LastObservedGapMS
		}
	}

	for _, name := range clusterNodeNames {
		tr := b.trackers[name]
		trCopy := *tr
		if trCopy.SubstrateState == "SUSPENDED" && trCopy.SuspendedAt != nil {
			trCopy.FrozenDriftMS = now.Sub(*trCopy.SuspendedAt).Milliseconds()
		} else if trCopy.ActorStatus != nil && trCopy.ActorStatus.LastObservedGapMS >= 1500 {
			trCopy.FrozenDriftMS = trCopy.ActorStatus.LastObservedGapMS
		}
		outNodes = append(outNodes, trCopy)
	}

	return computeClusterSummary(
		b.scenarioStep,
		b.scenarioPhase,
		b.scenarioSummary,
		b.electionPending,
		b.guard,
		now,
		node1Heap,
		driftMS,
		outNodes,
		b.events,
	)
}

func (b *ClusterAteBackend) GetState(_ context.Context) (*ClusterLabState, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buildStateLocked(), nil
}

func (b *ClusterAteBackend) Step1Quorum(ctx context.Context) (*ClusterLabState, error) {
	b.freeWorkerSlotFor(ctx, "raft-node-1", "raft-node-2", "raft-node-3")
	for _, name := range clusterNodeNames {
		if err := b.ensureActor(ctx, name); err != nil {
			return nil, err
		}
	}

	b.mu.Lock()
	b.electionGen++
	b.electionPending = false
	guard := b.guard
	b.events = nil
	b.mu.Unlock()

	for i, name := range clusterNodeNames {
		role := "FOLLOWER"
		if i == 0 {
			role = "LEADER"
		}
		workerIP := b.inspectRunningActor(ctx, name)
		var st ActorStatusResponse
		if _, err := b.callActorJSON(ctx, name, http.MethodPost, "/reset", workerIP, map[string]any{
			"node_name": name,
			"role":      role,
			"term":      1,
			"leader_id": "raft-node-1",
			"guard":     guard,
		}, &st); err != nil {
			return nil, err
		}
		if workerIP == "" {
			workerIP = b.inspectRunningActor(ctx, name)
			if workerIP != "" {
				st.CurrentWorkerPodIP = workerIP
			}
		}
		b.mu.Lock()
		b.trackers[name] = &NodeTrackerState{
			ActorName:      name,
			SubstrateState: "RUNNING",
			WorkerPodIP:    st.CurrentWorkerPodIP,
			ActorStatus:    &st,
		}
		b.mu.Unlock()
	}

	// Acquire Term 1 lock for Alice on raft-node-1 (Log Index #1)
	worker1IP := b.inspectRunningActor(ctx, "raft-node-1")
	var st1 ActorStatusResponse
	if _, err := b.callActorJSON(ctx, "raft-node-1", http.MethodPost, "/lock/acquire", worker1IP, map[string]any{
		"owner":         "Alice (checkout-svc)",
		"fencing_token": 101,
	}, &st1); err != nil {
		return nil, err
	}

	b.mu.Lock()
	b.trackers["raft-node-1"].ActorStatus = &st1
	b.trackers["raft-node-1"].WorkerPodIP = st1.CurrentWorkerPodIP
	b.mu.Unlock()

	if st1.CurrentLock != nil {
		for _, follower := range []string{"raft-node-2", "raft-node-3"} {
			fIP := b.inspectRunningActor(ctx, follower)
			var fst ActorStatusResponse
			if _, err := b.callActorJSON(ctx, follower, http.MethodPost, "/raft/replicate", fIP, map[string]any{
				"term":      1,
				"leader_id": "raft-node-1",
				"entry":     *st1.CurrentLock,
			}, &fst); err == nil {
				b.mu.Lock()
				b.trackers[follower].ActorStatus = &fst
				b.trackers[follower].WorkerPodIP = fst.CurrentWorkerPodIP
				b.mu.Unlock()
			}
		}
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	b.scenarioStep = 1
	b.scenarioPhase = "STEP 1: HEALTHY TERM 1 QUORUM (REV=1 REPLICATED TO ALL 3 NODES)"
	b.scenarioSummary = "raft-node-1 is Term 1 LEADER (3.0s lease). KV database rev=1 (/config/db_primary=postgres-east-1, /escrow/acct_balance=$10,000, /lease/checkout_owner=Alice #101) is replicated across all 3 actors on the WorkerPool."
	b.appendEventLocked(1, "INFO", "Term 1 Quorum Established (KV rev=1)", b.scenarioSummary)
	return b.buildStateLocked(), nil
}

func (b *ClusterAteBackend) promoteTerm2AndClaimBob(ctx context.Context, expectedGen uint64) error {
	b.mu.Lock()
	if expectedGen != 0 && (!b.electionPending || b.electionGen != expectedGen) {
		b.mu.Unlock()
		return nil
	}
	b.mu.Unlock()

	ip2 := b.inspectRunningActor(ctx, "raft-node-2")
	var st2 ActorStatusResponse
	if _, err := b.callActorJSON(ctx, "raft-node-2", http.MethodPost, "/raft/promote", ip2, map[string]any{
		"node_name": "raft-node-2",
		"role":      "LEADER",
		"term":      2,
		"leader_id": "raft-node-2",
	}, &st2); err != nil {
		return err
	}

	ip3 := b.inspectRunningActor(ctx, "raft-node-3")
	var st3 ActorStatusResponse
	_, _ = b.callActorJSON(ctx, "raft-node-3", http.MethodPost, "/raft/promote", ip3, map[string]any{
		"node_name": "raft-node-3",
		"role":      "FOLLOWER",
		"term":      2,
		"leader_id": "raft-node-2",
	}, &st3)

	// Client "Bob (inventory-svc)" commits KV rev=2 (Log Index #2, Token #102) in Term 2
	if _, err := b.callActorJSON(ctx, "raft-node-2", http.MethodPost, "/lock/acquire", ip2, map[string]any{
		"owner":         "Bob (inventory-svc)",
		"fencing_token": 102,
	}, &st2); err != nil {
		return err
	}
	if st2.CurrentLock != nil {
		_, _ = b.callActorJSON(ctx, "raft-node-3", http.MethodPost, "/raft/replicate", ip3, map[string]any{
			"term":      2,
			"leader_id": "raft-node-2",
			"entry":     *st2.CurrentLock,
		}, &st3)
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	if expectedGen != 0 && b.electionGen != expectedGen {
		return nil
	}
	b.electionPending = false
	b.trackers["raft-node-2"].SubstrateState = "RUNNING"
	b.trackers["raft-node-2"].ActorStatus = &st2
	b.trackers["raft-node-2"].WorkerPodIP = st2.CurrentWorkerPodIP
	b.trackers["raft-node-3"].SubstrateState = "RUNNING"
	b.trackers["raft-node-3"].ActorStatus = &st3
	b.trackers["raft-node-3"].WorkerPodIP = st3.CurrentWorkerPodIP
	b.scenarioStep = 2
	b.scenarioPhase = "STEP 2: 3.0s LEASE EXPIRED -> TERM 2 QUORUM COMMITTED KV REV=2 (BOB)"
	b.scenarioSummary = "After 3.0s of silence from frozen raft-node-1, quorum (raft-node-2 & raft-node-3) elected raft-node-2 as Term 2 LEADER and committed KV rev=2 (/config/db_primary=postgres-west-2, /escrow/acct_balance=$0 paid to 'Bob'). Meanwhile raft-node-1's frozen RAM in GCS is still at KV rev=1 ($10,000 in escrow)!"
	b.appendEventLocked(2, "WARN", "Term 1 Lease Expired -> Term 2 Quorum Committed KV rev=2 (Bob)", b.scenarioSummary)
	return nil
}

func (b *ClusterAteBackend) Step2FreezeAndElect(ctx context.Context) (*ClusterLabState, error) {
	b.mu.Lock()
	step := b.scenarioStep
	b.mu.Unlock()
	if step < 1 {
		if _, err := b.Step1Quorum(ctx); err != nil {
			return nil, err
		}
	}

	if _, err := b.runKubectlAte(ctx, "suspend", "actor", "raft-node-1", "-a", b.atespace, "--mode=sync"); err != nil {
		return nil, err
	}
	suspendedAt := time.Now().UTC()
	b.mu.Lock()
	b.electionGen++
	b.electionPending = false
	b.node1FrozenAt = suspendedAt
	if tr1, ok := b.trackers["raft-node-1"]; ok {
		tr1.SubstrateState = "SUSPENDED"
		tr1.SuspendedAt = &suspendedAt
	}
	b.appendEventLocked(2, "WARN", "kubectl ate suspend actor raft-node-1", "raft-node-1 frozen in GCS (SNAPSHOT_CONTENT_SCOPE_FULL). 0 CPU instructions executing; heap frozen at LEADER (Term 1), KV rev=1 (/escrow/acct_balance=$10,000).")
	b.mu.Unlock()

	elapsed := time.Since(suspendedAt)
	if elapsed < 3200*time.Millisecond {
		time.Sleep(3200*time.Millisecond - elapsed)
	}

	if err := b.promoteTerm2AndClaimBob(ctx, 0); err != nil {
		return nil, err
	}

	if rem := 5000*time.Millisecond - time.Since(suspendedAt); rem > 0 {
		time.Sleep(rem)
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buildStateLocked(), nil
}

func (b *ClusterAteBackend) Step2FreezeLeaderAsync(ctx context.Context) (*ClusterLabState, error) {
	b.mu.Lock()
	step := b.scenarioStep
	b.mu.Unlock()
	if step < 1 {
		if _, err := b.Step1Quorum(ctx); err != nil {
			return nil, err
		}
	}

	if _, err := b.runKubectlAte(ctx, "suspend", "actor", "raft-node-1", "-a", b.atespace, "--mode=sync"); err != nil {
		return nil, err
	}
	suspendedAt := time.Now().UTC()
	b.mu.Lock()
	b.electionGen++
	gen := b.electionGen
	b.electionPending = true
	b.node1FrozenAt = suspendedAt
	if tr1, ok := b.trackers["raft-node-1"]; ok {
		tr1.SubstrateState = "SUSPENDED"
		tr1.SuspendedAt = &suspendedAt
	}
	b.scenarioStep = 2
	b.scenarioPhase = "STEP 2A: LEADER FROZEN IN GCS — 3.0s LEASE COUNTING DOWN..."
	b.scenarioSummary = "raft-node-1 is suspended in GCS (0 CPU instructions, heap clock frozen). Watch the 3.0s Leader Lease countdown expire live to trigger the Term 2 Quorum Election on raft-node-2 & raft-node-3!"
	b.appendEventLocked(2, "WARN", "kubectl ate suspend actor raft-node-1", "raft-node-1 frozen in GCS (SNAPSHOT_CONTENT_SCOPE_FULL). 0 CPU instructions executing; heap frozen at LEADER (Term 1), KV rev=1 (/escrow/acct_balance=$10,000).")
	st := b.buildStateLocked()
	b.mu.Unlock()

	go func(expectedGen uint64, frozenAt time.Time) {
		wait := 3200*time.Millisecond - time.Since(frozenAt)
		if wait > 0 {
			time.Sleep(wait)
		}
		_ = b.promoteTerm2AndClaimBob(context.Background(), expectedGen)
	}(gen, suspendedAt)

	return st, nil
}

func (b *ClusterAteBackend) Step3WakeZombie(ctx context.Context) (*ClusterLabState, error) {
	b.mu.Lock()
	step := b.scenarioStep
	pending := b.electionPending
	frozenAt := b.node1FrozenAt
	tr1 := b.trackers["raft-node-1"]
	isSuspended := tr1 != nil && tr1.SubstrateState == "SUSPENDED"
	b.mu.Unlock()

	if step < 2 || !isSuspended {
		if _, err := b.Step2FreezeAndElect(ctx); err != nil {
			return nil, err
		}
	} else if pending {
		if wait := 3200*time.Millisecond - time.Since(frozenAt); wait > 0 {
			time.Sleep(wait)
		}
		if err := b.promoteTerm2AndClaimBob(ctx, 0); err != nil {
			return nil, err
		}
	}

	b.mu.Lock()
	frozenAt = b.node1FrozenAt
	b.mu.Unlock()
	if rem := 3500*time.Millisecond - time.Since(frozenAt); rem > 0 {
		time.Sleep(rem)
	}

	// Send conflicting KV rev=2 write ("Mallory") to raft-node-1 via atenet-router!
	var rawResp map[string]any
	code, err := b.callActorJSON(ctx, "raft-node-1", http.MethodPost, "/lock/acquire", "", map[string]any{
		"owner":         "Mallory (rogue-worker)",
		"fencing_token": 101,
	}, &rawResp)
	if err != nil {
		return nil, err
	}

	worker1IP := b.inspectRunningActor(ctx, "raft-node-1")
	var st1 ActorStatusResponse
	if _, err := b.callActorJSON(ctx, "raft-node-1", http.MethodGet, "/status", worker1IP, nil, &st1); err != nil {
		return nil, err
	}
	if worker1IP != "" {
		st1.CurrentWorkerPodIP = worker1IP
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	b.trackers["raft-node-1"].SubstrateState = "RUNNING"
	b.trackers["raft-node-1"].SuspendedAt = nil
	b.trackers["raft-node-1"].WorkerPodIP = st1.CurrentWorkerPodIP
	b.trackers["raft-node-1"].ActorStatus = &st1

	if code == http.StatusConflict || st1.Role == "STALE_LEADER_FENCED" {
		b.scenarioStep = 3
		b.scenarioPhase = "STEP 3 (GUARDED): WAKEUP CLOCK-JUMP FENCED STALE LEADER"
		b.scenarioSummary = fmt.Sprintf("raft-node-1 woke on pod %s after a %dms clock jump. Wakeup Clock-Jump Guardrail immediately fenced its stale Term 1 lease (STALE_LEADER_FENCED), clamped token refill, and rejected 'Mallory' with 409 Conflict (KV Divergence Prevented)!", st1.CurrentWorkerPodIP, st1.LastObservedGapMS)
		b.appendEventLocked(3, "GUARDED", "Wakeup Clock-Jump Guardrail Prevented KV Split-Brain", b.scenarioSummary)
		return b.buildStateLocked(), nil
	}

	b.scenarioStep = 3
	b.scenarioPhase = "STEP 3: ZOMBIE LEADER SPLIT-BRAIN & KV DATABASE DIVERGENCE (REV=2 FORK)!"
	b.scenarioSummary = fmt.Sprintf("CRITICAL KV DIVERGENCE: atenet woke raft-node-1 from GCS onto pod %s. Its 100ms ticker jumped +%dms, its token bucket minted +%.0f burst tokens, and its stale Term 1 heap committed a conflicting KV rev=2 (paying $10,000 escrow to 'Mallory' & setting db_primary=postgres-rogue-3) while Term 2 Quorum already committed rev=2 for 'Bob'!", st1.CurrentWorkerPodIP, st1.LastObservedGapMS, st1.TokenBucketValue-10.0)
	b.appendEventLocked(3, "CRITICAL", "ZOMBIE LEADER KV DIVERGENCE & FORKED LOG INDEX #2", b.scenarioSummary)
	return b.buildStateLocked(), nil
}

func (b *ClusterAteBackend) Step4ReplayGuardrail(ctx context.Context) (*ClusterLabState, error) {
	b.mu.Lock()
	b.guard = true
	b.mu.Unlock()

	if _, err := b.Step1Quorum(ctx); err != nil {
		return nil, err
	}
	if _, err := b.Step2FreezeAndElect(ctx); err != nil {
		return nil, err
	}
	if _, err := b.Step3WakeZombie(ctx); err != nil {
		return nil, err
	}
	b.mu.Lock()
	b.scenarioStep = 4
	b.scenarioPhase = "STEP 4: GUARDRAIL ACTIVE — STALE LEASE FENCED & KV CONSISTENCY PRESERVED"
	b.mu.Unlock()
	return b.GetState(ctx)
}

func (b *ClusterAteBackend) PutKV(ctx context.Context, targetNode, key, value string) (*ClusterLabState, int, error) {
	if targetNode == "" {
		targetNode = "raft-node-2"
	}
	if !slices.Contains(clusterNodeNames, targetNode) {
		b.mu.Lock()
		defer b.mu.Unlock()
		return b.buildStateLocked(), http.StatusBadRequest, fmt.Errorf("unknown node %q", targetNode)
	}
	if key == "" {
		key = "/config/db_primary"
	}

	workerIP := b.inspectRunningActor(ctx, targetNode)
	var respPayload struct {
		Error      string              `json:"error"`
		KVEntry    KVEntry             `json:"kv_entry"`
		NodeStatus ActorStatusResponse `json:"node_status"`
	}
	code, err := b.callActorJSON(ctx, targetNode, http.MethodPost, "/kv/put", workerIP, map[string]any{
		"key":   key,
		"value": value,
	}, &respPayload)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	if workerIP == "" {
		workerIP = b.inspectRunningActor(ctx, targetNode)
		if workerIP != "" {
			respPayload.NodeStatus.CurrentWorkerPodIP = workerIP
		}
	}

	b.mu.Lock()
	b.trackers[targetNode].SubstrateState = "RUNNING"
	b.trackers[targetNode].SuspendedAt = nil
	b.trackers[targetNode].WorkerPodIP = respPayload.NodeStatus.CurrentWorkerPodIP
	stCopy := respPayload.NodeStatus
	b.trackers[targetNode].ActorStatus = &stCopy
	step := b.scenarioStep
	b.mu.Unlock()

	if code == http.StatusConflict {
		b.mu.Lock()
		defer b.mu.Unlock()
		sev := "WARN"
		if stCopy.Role == "STALE_LEADER_FENCED" {
			sev = "GUARDED"
		}
		b.appendEventLocked(step, sev, fmt.Sprintf("Rejected PUT %s on %s", key, targetNode), respPayload.Error)
		return b.buildStateLocked(), http.StatusConflict, fmt.Errorf("%s", respPayload.Error)
	}

	if stCopy.Role == "LEADER" {
		for _, name := range clusterNodeNames {
			if name == targetNode {
				continue
			}
			b.mu.Lock()
			tr := b.trackers[name]
			shouldReplicate := tr != nil && tr.SubstrateState == "RUNNING" && tr.ActorStatus != nil && tr.ActorStatus.Role == "FOLLOWER" && tr.ActorStatus.Term == stCopy.Term
			b.mu.Unlock()
			if shouldReplicate {
				fIP := b.inspectRunningActor(ctx, name)
				var fst ActorStatusResponse
				if _, err := b.callActorJSON(ctx, name, http.MethodPost, "/kv/replicate", fIP, map[string]any{
					"term":      stCopy.Term,
					"leader_id": targetNode,
					"entry":     respPayload.KVEntry,
				}, &fst); err == nil {
					b.mu.Lock()
					b.trackers[name].ActorStatus = &fst
					b.trackers[name].WorkerPodIP = fst.CurrentWorkerPodIP
					b.mu.Unlock()
				}
			}
		}
		b.mu.Lock()
		b.appendEventLocked(step, "INFO", fmt.Sprintf("Quorum PUT %s=%q (rev=%d, Term %d)", key, value, respPayload.KVEntry.Revision, respPayload.KVEntry.Term),
			fmt.Sprintf("Committed on %s (LEADER Term %d) and replicated to quorum followers.", targetNode, respPayload.KVEntry.Term))
		defer b.mu.Unlock()
		return b.buildStateLocked(), http.StatusOK, nil
	}

	b.mu.Lock()
	b.appendEventLocked(step, "CRITICAL", fmt.Sprintf("SPLIT-BRAIN PUT %s=%q on %s (Term %d)", key, value, targetNode, respPayload.KVEntry.Term),
		fmt.Sprintf("%s (%s Term %d) accepted client write without quorum! Key %s now diverges from Term 2 Quorum!", targetNode, stCopy.Role, respPayload.KVEntry.Term, key))
	defer b.mu.Unlock()
	return b.buildStateLocked(), http.StatusOK, nil
}

func (b *ClusterAteBackend) SetGuardrail(ctx context.Context, enabled bool) (*ClusterLabState, error) {
	b.mu.Lock()
	b.guard = enabled
	b.mu.Unlock()

	for _, name := range clusterNodeNames {
		b.mu.Lock()
		tr := b.trackers[name]
		running := tr != nil && tr.SubstrateState == "RUNNING"
		b.mu.Unlock()
		if running {
			var st ActorStatusResponse
			if _, err := b.callActorJSON(ctx, name, http.MethodPost, "/config", "", map[string]any{"guard": enabled}, &st); err == nil {
				b.mu.Lock()
				b.trackers[name].ActorStatus = &st
				b.mu.Unlock()
			}
		}
	}
	return b.GetState(ctx)
}

func (b *ClusterAteBackend) Reset(ctx context.Context) (*ClusterLabState, error) {
	b.mu.Lock()
	b.guard = false
	b.mu.Unlock()
	return b.Step1Quorum(ctx)
}
