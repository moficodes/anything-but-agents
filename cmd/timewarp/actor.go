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
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

type KVEntry struct {
	Key       string    `json:"key"`
	Value     string    `json:"value"`
	Revision  int       `json:"revision"`
	Term      int       `json:"term"`
	UpdatedBy string    `json:"updated_by"`
	Role      string    `json:"role"`
	Timestamp time.Time `json:"timestamp"`
}

type LockEntry struct {
	LogIndex         int       `json:"log_index"`
	Term             int       `json:"term"`
	FencingToken     int       `json:"fencing_token"`
	ResourceID       string    `json:"resource_id"`
	Action           string    `json:"action"`
	Owner            string    `json:"owner"`
	GrantedByNode    string    `json:"granted_by_node"`
	GrantedByRole    string    `json:"granted_by_role"`
	PodIP            string    `json:"pod_ip"`
	Timestamp        time.Time `json:"timestamp"`
	ZombieSplitBrain bool      `json:"zombie_split_brain"`
	LeaseExpiredByMS int64     `json:"lease_expired_by_ms,omitempty"`
}

type TimeJumpAnomaly struct {
	DetectedAt       time.Time `json:"detected_at"`
	ExpectedTickMS   int64     `json:"expected_tick_ms"`
	ObservedGapMS    int64     `json:"observed_gap_ms"`
	SplitBrainAlert  string    `json:"split_brain_alert"`
	TokenOverflow    float64   `json:"naive_rate_limiter_tokens_minted"`
	TokenClamped     bool      `json:"token_clamped"`
	GuardrailApplied bool      `json:"guardrail_applied"`
	PodIP            string    `json:"pod_ip"`
}

type ActorStatusResponse struct {
	Demo                 string            `json:"demo"`
	NodeName             string            `json:"node_name"`
	Status               string            `json:"status"`
	Role                 string            `json:"role"`
	Term                 int               `json:"term"`
	LeaderID             string            `json:"leader_id"`
	InMemoryRaftRole     string            `json:"in_memory_raft_role"`
	LeaderLeaseTTL       string            `json:"leader_lease_ttl"`
	LeaseTTLMS           int64             `json:"lease_ttl_ms"`
	LeaseRemainingMS     int64             `json:"lease_remaining_ms"`
	ExpectedTickMS       int64             `json:"expected_tick_ms"`
	LastObservedGapMS    int64             `json:"last_observed_gap_ms"`
	TotalHeartbeatTicks  int64             `json:"total_heartbeat_ticks"`
	LastHeartbeatAt      time.Time         `json:"last_heartbeat_at"`
	NaiveTokenBucket     string            `json:"naive_token_bucket"`
	TokenBucketValue     float64           `json:"token_bucket_value"`
	ClockJumpGuard       bool              `json:"clock_jump_guard"`
	VaultResourceID      string            `json:"vault_resource_id"`
	HeapAvailableUnits   int               `json:"heap_available_units"`
	KVRevision           int               `json:"kv_revision"`
	KVStore              []KVEntry         `json:"kv_store"`
	CurrentLock          *LockEntry        `json:"current_lock,omitempty"`
	CommitHistory        []LockEntry       `json:"commit_history"`
	TimeJumpAnomalies    []TimeJumpAnomaly `json:"time_jump_anomalies"`
	CurrentWorkerPodIP   string            `json:"current_worker_pod_ip"`
	PodIPHistory         []string          `json:"pod_ip_history"`
	WhyThisHappened      string            `json:"why_this_happened"`
}

type NaiveClusterNode struct {
	mu                 sync.Mutex
	NodeName           string
	Role               string // LEADER | FOLLOWER | ZOMBIE_LEADER | STALE_LEADER_FENCED
	Term               int
	LeaderID           string
	ExpectedTick       time.Duration
	LeaseTTL           time.Duration
	AnomalyThreshold   time.Duration
	LastHeartbeatAt    time.Time
	LeaseGrantedAt     time.Time
	LastObservedGapMS  int64
	InMemoryRaftRole   string
	TotalTicks         int64
	NaiveTokenBucket   float64
	ClockJumpGuard     bool
	HeapAvailableUnits int
	KVRevision         int
	KVMut              map[string]KVEntry
	KVOrder            []string
	CurrentLock        *LockEntry
	CommitHistory      []LockEntry
	Anomalies          []TimeJumpAnomaly
	CurrentPodIP       string
	PodIPHistory       []string
}

func newNaiveClusterNode(now time.Time) *NaiveClusterNode {
	n := &NaiveClusterNode{
		NodeName:           "raft-node-1",
		Role:               "LEADER",
		Term:               1,
		LeaderID:           "raft-node-1",
		ExpectedTick:       100 * time.Millisecond,
		LeaseTTL:           3 * time.Second,
		AnomalyThreshold:   1500 * time.Millisecond,
		LastHeartbeatAt:    now,
		LeaseGrantedAt:     now,
		InMemoryRaftRole:   "LEADER (Term 1) — stored in local RAM variable",
		NaiveTokenBucket:   10.0,
		HeapAvailableUnits: 1,
		KVMut:              make(map[string]KVEntry),
		CommitHistory:      []LockEntry{},
		Anomalies:          []TimeJumpAnomaly{},
		PodIPHistory:       []string{},
	}
	return n
}

func (n *NaiveClusterNode) recordPodIPLocked(podIP string) {
	if podIP == "" {
		return
	}
	isLinkLocal := strings.HasPrefix(podIP, "169.254.")
	if isLinkLocal && n.CurrentPodIP != "" && !strings.HasPrefix(n.CurrentPodIP, "169.254.") {
		return
	}
	n.CurrentPodIP = podIP
	if !isLinkLocal {
		// Remove any 169.254.* link-local sandbox veth placeholder from history and entries
		filtered := n.PodIPHistory[:0]
		for _, ip := range n.PodIPHistory {
			if !strings.HasPrefix(ip, "169.254.") {
				filtered = append(filtered, ip)
			}
		}
		n.PodIPHistory = filtered
		if n.CurrentLock != nil && strings.HasPrefix(n.CurrentLock.PodIP, "169.254.") {
			n.CurrentLock.PodIP = podIP
		}
		for i := range n.CommitHistory {
			if strings.HasPrefix(n.CommitHistory[i].PodIP, "169.254.") {
				n.CommitHistory[i].PodIP = podIP
			}
		}
		for i := range n.Anomalies {
			if strings.HasPrefix(n.Anomalies[i].PodIP, "169.254.") {
				n.Anomalies[i].PodIP = podIP
			}
		}
	}
	if !slices.Contains(n.PodIPHistory, podIP) {
		n.PodIPHistory = append(n.PodIPHistory, podIP)
	}
}

func (n *NaiveClusterNode) syncRoleDescriptionLocked() {
	switch n.Role {
	case "LEADER":
		n.InMemoryRaftRole = fmt.Sprintf("LEADER (Term %d) — stored in local RAM variable", n.Term)
	case "ZOMBIE_LEADER":
		n.InMemoryRaftRole = fmt.Sprintf("ZOMBIE LEADER (Term %d) — stale RAM lease expired while frozen in GCS!", n.Term)
	case "STALE_LEADER_FENCED":
		n.InMemoryRaftRole = fmt.Sprintf("STALE_LEADER_FENCED (Term %d) — Wakeup Clock-Jump Guardrail stepped down!", n.Term)
	default:
		n.InMemoryRaftRole = fmt.Sprintf("FOLLOWER (Term %d, following %s)", n.Term, n.LeaderID)
	}
}

func (n *NaiveClusterNode) ObserveClock(now time.Time, podIP string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.observeClockLocked(now, podIP)
}

// wallClockSub returns the wall-clock (CLOCK_REALTIME) delta between a and b by
// stripping Go's monotonic clock reading (.Round(0)). In gVisor 20261102.0+
// (Agent Substrate v1.0.0+, SandboxConfig v7), CLOCK_MONOTONIC pauses while
// an actor is checkpointed, whereas CLOCK_REALTIME jumps forward by the full
// suspend duration.
func wallClockSub(a, b time.Time) time.Duration {
	return a.Round(0).Sub(b.Round(0))
}

func (n *NaiveClusterNode) observeClockLocked(now time.Time, podIP string) {
	n.recordPodIPLocked(podIP)
	if n.LastHeartbeatAt.IsZero() {
		return
	}

	n.TotalTicks++
	gap := wallClockSub(now, n.LastHeartbeatAt)
	if gap < 0 {
		gap = 0
	}
	n.LastObservedGapMS = gap.Milliseconds()
	n.LastHeartbeatAt = now

	if gap >= n.AnomalyThreshold {
		if n.ClockJumpGuard {
			// Guardrail enabled: clamp rate-limiter refill to 1 tick and immediately fence any leader lease!
			minted := n.ExpectedTick.Seconds() * 100.0
			n.NaiveTokenBucket += minted
			if n.NaiveTokenBucket > 100.0 {
				n.NaiveTokenBucket = 100.0
			}
			fencedMsg := fmt.Sprintf(
				"GUARDRAIL TRIGGERED: Observed %dms wall-clock jump (expected %dms). Fenced stale %s lease before serving traffic and clamped token refill to +%.1f tokens.",
				gap.Milliseconds(),
				n.ExpectedTick.Milliseconds(),
				n.InMemoryRaftRole,
				minted,
			)
			if n.Role == "LEADER" || n.Role == "ZOMBIE_LEADER" {
				n.Role = "STALE_LEADER_FENCED"
				n.syncRoleDescriptionLocked()
			}
			n.Anomalies = append(n.Anomalies, TimeJumpAnomaly{
				DetectedAt:       now.UTC(),
				ExpectedTickMS:   n.ExpectedTick.Milliseconds(),
				ObservedGapMS:    gap.Milliseconds(),
				SplitBrainAlert:  fencedMsg,
				TokenOverflow:    minted,
				TokenClamped:     true,
				GuardrailApplied: true,
				PodIP:            n.CurrentPodIP,
			})
			return
		}

		// Anti-Pattern (ClockJumpGuard == false):
		// 1. Naive token bucket overflows by gap.Seconds() * 100 req/sec without clamping!
		minted := gap.Seconds() * 100.0
		n.NaiveTokenBucket += minted

		// 2. If gap >= LeaseTTL, the 3-second leader lease expired while frozen in GCS,
		//    yet the heap variable still believes it is the Term 1 leader!
		splitBrain := "LEASE_STILL_VALID"
		if gap >= n.LeaseTTL {
			prevDesc := n.InMemoryRaftRole
			if n.Role == "LEADER" || n.Role == "ZOMBIE_LEADER" {
				n.Role = "ZOMBIE_LEADER"
			}
			splitBrain = fmt.Sprintf(
				"CRITICAL SPLIT-BRAIN: Heap variable still says %q, but 3s leader lease expired %.1fs ago while frozen in snapshot storage! Cluster already elected Term %d leader!",
				prevDesc,
				(gap - n.LeaseTTL).Seconds(),
				n.Term+1,
			)
			n.syncRoleDescriptionLocked()
		}
		n.Anomalies = append(n.Anomalies, TimeJumpAnomaly{
			DetectedAt:       now.UTC(),
			ExpectedTickMS:   n.ExpectedTick.Milliseconds(),
			ObservedGapMS:    gap.Milliseconds(),
			SplitBrainAlert:  splitBrain,
			TokenOverflow:    minted,
			TokenClamped:     false,
			GuardrailApplied: false,
			PodIP:            n.CurrentPodIP,
		})
		return
	}

	// Normal tick (< 1.5s): steady-state traffic consumes refill at 1:1 rate,
	// while any prior un-clamped burst overflow remains visible.
	if n.Role == "LEADER" {
		n.LeaseGrantedAt = now
	}
}

func (n *NaiveClusterNode) PromoteRole(now time.Time, nodeName, role string, term int, leaderID, podIP string) ActorStatusResponse {
	n.mu.Lock()
	defer n.mu.Unlock()

	if nodeName != "" {
		n.NodeName = nodeName
	}
	if role != "" {
		n.Role = strings.ToUpper(role)
	}
	if term > 0 {
		n.Term = term
	}
	if leaderID != "" {
		n.LeaderID = leaderID
	}
	n.recordPodIPLocked(podIP)
	if !now.IsZero() {
		n.LastHeartbeatAt = now
		n.LeaseGrantedAt = now
	}
	n.LastObservedGapMS = n.ExpectedTick.Milliseconds()
	n.syncRoleDescriptionLocked()
	return n.snapshotStatusLocked(now)
}

func (n *NaiveClusterNode) setKVEntryLocked(entry KVEntry) {
	if n.KVMut == nil {
		n.KVMut = make(map[string]KVEntry)
	}
	if _, exists := n.KVMut[entry.Key]; !exists {
		n.KVOrder = append(n.KVOrder, entry.Key)
	}
	n.KVMut[entry.Key] = entry
	if entry.Revision > n.KVRevision {
		n.KVRevision = entry.Revision
	}
}

func (n *NaiveClusterNode) applyMilestoneKVLocked(entry LockEntry, now time.Time) {
	rev := entry.LogIndex
	if rev <= 0 {
		rev = n.KVRevision + 1
	}
	dbPrimary := "postgres-east-1"
	escrowBalance := "$10,000 (initial deposit)"
	if entry.LogIndex >= 2 {
		if entry.ZombieSplitBrain || entry.Term == 1 {
			dbPrimary = "postgres-rogue-3 (split-brain!)"
			escrowBalance = fmt.Sprintf("$0 (paid $10,000 to %s!)", entry.Owner)
		} else {
			dbPrimary = "postgres-west-2 (failover)"
			escrowBalance = fmt.Sprintf("$0 (paid $10,000 to %s)", entry.Owner)
		}
	}
	pairs := [][2]string{
		{"/config/db_primary", dbPrimary},
		{"/escrow/acct_balance", escrowBalance},
		{"/lease/checkout_owner", fmt.Sprintf("%s [#%d]", entry.Owner, entry.FencingToken)},
	}
	for _, kv := range pairs {
		n.setKVEntryLocked(KVEntry{
			Key:       kv[0],
			Value:     kv[1],
			Revision:  rev,
			Term:      entry.Term,
			UpdatedBy: entry.GrantedByNode,
			Role:      entry.GrantedByRole,
			Timestamp: now.UTC(),
		})
	}
}

func (n *NaiveClusterNode) PutKV(now time.Time, key, value, podIP string) (KVEntry, error) {
	n.mu.Lock()
	defer n.mu.Unlock()

	if n.LastHeartbeatAt.IsZero() {
		n.LastHeartbeatAt = now
		n.LeaseGrantedAt = now
	}
	n.observeClockLocked(now, podIP)

	if n.Role == "STALE_LEADER_FENCED" {
		return KVEntry{}, fmt.Errorf("409 Conflict: node %s stepped down (STALE_LEADER_FENCED) after %dms wakeup clock jump; refusing stale Term %d KV PUT %s=%q", n.NodeName, n.LastObservedGapMS, n.Term, key, value)
	}
	if n.Role == "FOLLOWER" {
		return KVEntry{}, fmt.Errorf("409 Conflict: node %s is FOLLOWER (Term %d, leader=%s); client PUT must go to LEADER", n.NodeName, n.Term, n.LeaderID)
	}

	n.KVRevision++
	entry := KVEntry{
		Key:       key,
		Value:     value,
		Revision:  n.KVRevision,
		Term:      n.Term,
		UpdatedBy: n.NodeName,
		Role:      n.Role,
		Timestamp: now.UTC(),
	}
	n.setKVEntryLocked(entry)
	return entry, nil
}

func (n *NaiveClusterNode) ReplicateKV(now time.Time, term int, leaderID string, entry KVEntry, podIP string) ActorStatusResponse {
	n.mu.Lock()
	defer n.mu.Unlock()

	if n.LastHeartbeatAt.IsZero() {
		n.LastHeartbeatAt = now
		n.LeaseGrantedAt = now
	}
	n.observeClockLocked(now, podIP)
	if term >= n.Term {
		n.Term = term
		if leaderID != "" && leaderID != n.NodeName {
			n.Role = "FOLLOWER"
			n.LeaderID = leaderID
		}
		n.syncRoleDescriptionLocked()
	}
	n.setKVEntryLocked(entry)
	return n.snapshotStatusLocked(now)
}

func (n *NaiveClusterNode) AcquireLock(now time.Time, owner string, fencingToken int, podIP string) (LockEntry, error) {
	n.mu.Lock()
	defer n.mu.Unlock()

	if n.LastHeartbeatAt.IsZero() {
		n.LastHeartbeatAt = now
		n.LeaseGrantedAt = now
	}
	n.observeClockLocked(now, podIP)

	if n.Role == "STALE_LEADER_FENCED" {
		return LockEntry{}, fmt.Errorf("409 Conflict: node %s stepped down (STALE_LEADER_FENCED) after %dms wakeup clock jump; refusing stale Term %d lock write for %q", n.NodeName, n.LastObservedGapMS, n.Term, owner)
	}
	if n.Role == "FOLLOWER" {
		return LockEntry{}, fmt.Errorf("409 Conflict: node %s is FOLLOWER (Term %d, leader=%s); cannot grant lock", n.NodeName, n.Term, n.LeaderID)
	}

	isZombie := n.Role == "ZOMBIE_LEADER"
	var expiredByMS int64
	if isZombie && n.LastObservedGapMS > n.LeaseTTL.Milliseconds() {
		expiredByMS = n.LastObservedGapMS - n.LeaseTTL.Milliseconds()
	}

	logIndex := len(n.CommitHistory) + 1
	action := "PUT rev=1 (/config, /escrow=$10k)"
	if logIndex >= 2 {
		action = "PUT rev=2 (/config, /escrow=$0)"
		n.HeapAvailableUnits = 0
	}

	entry := LockEntry{
		LogIndex:         logIndex,
		Term:             n.Term,
		FencingToken:     fencingToken,
		ResourceID:       "etcd-kv-store",
		Action:           action,
		Owner:            owner,
		GrantedByNode:    n.NodeName,
		GrantedByRole:    n.Role,
		PodIP:            n.CurrentPodIP,
		Timestamp:        now.UTC(),
		ZombieSplitBrain: isZombie,
		LeaseExpiredByMS: expiredByMS,
	}
	n.CurrentLock = &entry
	n.CommitHistory = append(n.CommitHistory, entry)
	n.applyMilestoneKVLocked(entry, now)
	return entry, nil
}

func (n *NaiveClusterNode) ReplicateEntry(now time.Time, term int, leaderID string, entry LockEntry, podIP string) ActorStatusResponse {
	n.mu.Lock()
	defer n.mu.Unlock()

	if n.LastHeartbeatAt.IsZero() {
		n.LastHeartbeatAt = now
		n.LeaseGrantedAt = now
	}
	n.observeClockLocked(now, podIP)
	if term >= n.Term {
		n.Term = term
		if leaderID != "" && leaderID != n.NodeName {
			n.Role = "FOLLOWER"
			n.LeaderID = leaderID
		}
		n.syncRoleDescriptionLocked()
	}
	entryCopy := entry
	if entryCopy.LogIndex <= 0 {
		entryCopy.LogIndex = len(n.CommitHistory) + 1
	}
	if entryCopy.ResourceID == "" {
		entryCopy.ResourceID = "etcd-kv-store"
	}
	if entryCopy.LogIndex >= 2 {
		if entryCopy.Action == "" {
			entryCopy.Action = "PUT rev=2 (/config, /escrow=$0)"
		}
		n.HeapAvailableUnits = 0
	} else if entryCopy.Action == "" {
		entryCopy.Action = "PUT rev=1 (/config, /escrow=$10k)"
	}
	n.CurrentLock = &entryCopy
	n.CommitHistory = append(n.CommitHistory, entryCopy)
	n.applyMilestoneKVLocked(entryCopy, now)
	return n.snapshotStatusLocked(now)
}

func (n *NaiveClusterNode) SetGuardrail(enabled bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.ClockJumpGuard = enabled
}

func (n *NaiveClusterNode) Reset(now time.Time, nodeName, role string, term int, leaderID string, guard bool, podIP string) ActorStatusResponse {
	n.mu.Lock()
	defer n.mu.Unlock()

	if nodeName != "" {
		n.NodeName = nodeName
	}
	if role == "" {
		role = "LEADER"
	}
	if term <= 0 {
		term = 1
	}
	if leaderID == "" {
		leaderID = n.NodeName
	}
	n.Role = strings.ToUpper(role)
	n.Term = term
	n.LeaderID = leaderID
	n.ClockJumpGuard = guard
	n.LastHeartbeatAt = now
	n.LeaseGrantedAt = now
	n.LastObservedGapMS = n.ExpectedTick.Milliseconds()
	n.TotalTicks = 1
	n.NaiveTokenBucket = 10.0
	n.HeapAvailableUnits = 1
	n.KVRevision = 0
	n.KVMut = make(map[string]KVEntry)
	n.KVOrder = nil
	n.CurrentLock = nil
	n.CommitHistory = []LockEntry{}
	n.Anomalies = []TimeJumpAnomaly{}
	n.PodIPHistory = []string{}
	n.recordPodIPLocked(podIP)
	n.syncRoleDescriptionLocked()
	return n.snapshotStatusLocked(now)
}

func (n *NaiveClusterNode) snapshotStatusLocked(now time.Time) ActorStatusResponse {
	health := "HEALTHY (wall clock advancing normally)"
	lastGapMS := n.LastObservedGapMS
	if len(n.Anomalies) > 0 {
		last := n.Anomalies[len(n.Anomalies)-1]
		if lastGapMS < n.AnomalyThreshold.Milliseconds() {
			lastGapMS = last.ObservedGapMS
		}
		if last.GuardrailApplied {
			health = "GUARDED: WAKEUP CLOCK-JUMP DETECTED — STALE LEASE FENCED & TOKENS CLAMPED"
		} else {
			health = "PANIC: TIME-TRAVEL SHOCK & RAFT SPLIT-BRAIN DETECTED AFTER RESUME!"
		}
	}

	leaseRemMS := n.LeaseTTL.Milliseconds()
	if !n.LeaseGrantedAt.IsZero() && !now.IsZero() {
		elapsed := wallClockSub(now, n.LeaseGrantedAt)
		rem := n.LeaseTTL - elapsed
		if rem < 0 {
			rem = 0
		}
		leaseRemMS = rem.Milliseconds()
	}
	if n.Role == "FOLLOWER" || n.Role == "STALE_LEADER_FENCED" {
		leaseRemMS = 0
	}

	var curLockCopy *LockEntry
	if n.CurrentLock != nil {
		c := *n.CurrentLock
		curLockCopy = &c
	}

	kvList := make([]KVEntry, 0, len(n.KVOrder))
	for _, k := range n.KVOrder {
		if v, ok := n.KVMut[k]; ok {
			kvList = append(kvList, v)
		}
	}

	return ActorStatusResponse{
		Demo:                "Anti-Pattern: Wall-Clock Naive & Distributed Consensus Node on Substrate",
		NodeName:            n.NodeName,
		Status:              health,
		Role:                n.Role,
		Term:                n.Term,
		LeaderID:            n.LeaderID,
		InMemoryRaftRole:    n.InMemoryRaftRole,
		LeaderLeaseTTL:      n.LeaseTTL.String(),
		LeaseTTLMS:          n.LeaseTTL.Milliseconds(),
		LeaseRemainingMS:    leaseRemMS,
		ExpectedTickMS:      n.ExpectedTick.Milliseconds(),
		LastObservedGapMS:   lastGapMS,
		TotalHeartbeatTicks: n.TotalTicks,
		LastHeartbeatAt:     n.LastHeartbeatAt,
		NaiveTokenBucket:    fmt.Sprintf("%.1f tokens", n.NaiveTokenBucket),
		TokenBucketValue:    n.NaiveTokenBucket,
		ClockJumpGuard:      n.ClockJumpGuard,
		VaultResourceID:     "etcd-kv-store",
		HeapAvailableUnits:  n.HeapAvailableUnits,
		KVRevision:          n.KVRevision,
		KVStore:             kvList,
		CurrentLock:         curLockCopy,
		CommitHistory:       slices.Clone(n.CommitHistory),
		TimeJumpAnomalies:   slices.Clone(n.Anomalies),
		CurrentWorkerPodIP:  n.CurrentPodIP,
		PodIPHistory:        slices.Clone(n.PodIPHistory),
		WhyThisHappened:     "Full snapshots freeze process memory, not the universe's wall clock. Never run Raft/Paxos voters or unguarded wall-clock delta loops as suspendable actors!",
	}
}

func (n *NaiveClusterNode) SnapshotStatus(now time.Time, podIP string) ActorStatusResponse {
	n.mu.Lock()
	if n.LastHeartbeatAt.IsZero() {
		n.LastHeartbeatAt = now
		n.LeaseGrantedAt = now
	}
	n.mu.Unlock()

	n.ObserveClock(now, podIP)

	n.mu.Lock()
	defer n.mu.Unlock()
	return n.snapshotStatusLocked(now)
}

func (n *NaiveClusterNode) Report(now time.Time, podIP string) map[string]any {
	st := n.SnapshotStatus(now, podIP)
	return map[string]any{
		"demo":                  st.Demo,
		"node_name":             st.NodeName,
		"status":                st.Status,
		"role":                  st.Role,
		"term":                  st.Term,
		"leader_id":             st.LeaderID,
		"in_memory_raft_role":   st.InMemoryRaftRole,
		"leader_lease_ttl":      st.LeaderLeaseTTL,
		"lease_ttl_ms":          st.LeaseTTLMS,
		"lease_remaining_ms":    st.LeaseRemainingMS,
		"expected_tick_ms":      st.ExpectedTickMS,
		"last_observed_gap_ms":  st.LastObservedGapMS,
		"total_heartbeat_ticks": st.TotalHeartbeatTicks,
		"naive_token_bucket":    st.NaiveTokenBucket,
		"token_bucket_value":    st.TokenBucketValue,
		"clock_jump_guard":      st.ClockJumpGuard,
		"vault_resource_id":     st.VaultResourceID,
		"heap_available_units":  st.HeapAvailableUnits,
		"kv_revision":           st.KVRevision,
		"kv_store":              st.KVStore,
		"current_lock":          st.CurrentLock,
		"commit_history":        st.CommitHistory,
		"time_jump_anomalies":   st.TimeJumpAnomalies,
		"current_worker_pod_ip": st.CurrentWorkerPodIP,
		"pod_ip_history":        st.PodIPHistory,
		"why_this_happened":     st.WhyThisHappened,
	}
}

func decodeOptionalJSON(r *http.Request, dst any) error {
	if r.Body == nil {
		return nil
	}
	err := json.NewDecoder(r.Body).Decode(dst)
	if errors.Is(err, io.EOF) {
		return nil
	}
	return err
}

func writePrettyJSON(w http.ResponseWriter, code int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(payload)
}

func effectivePodIP(r *http.Request) string {
	if hdr := strings.TrimSpace(r.Header.Get("X-Worker-Pod-IP")); hdr != "" {
		return hdr
	}
	return currentPodIP()
}

func newMux(node *NaiveClusterNode) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})

	statusHandler := func(w http.ResponseWriter, r *http.Request) {
		resp := node.SnapshotStatus(time.Now(), effectivePodIP(r))
		writePrettyJSON(w, http.StatusOK, resp)
	}
	mux.HandleFunc("/", statusHandler)
	mux.HandleFunc("/status", statusHandler)

	mux.HandleFunc("/raft/promote", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			NodeName string `json:"node_name"`
			Role     string `json:"role"`
			Term     int    `json:"term"`
			LeaderID string `json:"leader_id"`
		}
		_ = decodeOptionalJSON(r, &req)
		if q := r.URL.Query().Get("node_name"); q != "" {
			req.NodeName = q
		}
		if q := r.URL.Query().Get("role"); q != "" {
			req.Role = q
		}
		if q := r.URL.Query().Get("term"); q != "" {
			if v, err := strconv.Atoi(q); err == nil {
				req.Term = v
			}
		}
		if q := r.URL.Query().Get("leader_id"); q != "" {
			req.LeaderID = q
		}
		resp := node.PromoteRole(time.Now(), req.NodeName, req.Role, req.Term, req.LeaderID, effectivePodIP(r))
		writePrettyJSON(w, http.StatusOK, resp)
	})

	mux.HandleFunc("/raft/replicate", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Term     int       `json:"term"`
			LeaderID string    `json:"leader_id"`
			Entry    LockEntry `json:"entry"`
		}
		if err := decodeOptionalJSON(r, &req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		resp := node.ReplicateEntry(time.Now(), req.Term, req.LeaderID, req.Entry, effectivePodIP(r))
		writePrettyJSON(w, http.StatusOK, resp)
	})

	mux.HandleFunc("/kv/put", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Key   string `json:"key"`
			Value string `json:"value"`
		}
		_ = decodeOptionalJSON(r, &req)
		if q := r.URL.Query().Get("key"); q != "" {
			req.Key = q
		}
		if q := r.URL.Query().Get("value"); q != "" {
			req.Value = q
		}
		if req.Key == "" {
			req.Key = "/config/db_primary"
		}
		now := time.Now()
		podIP := effectivePodIP(r)
		kvEntry, err := node.PutKV(now, req.Key, req.Value, podIP)
		node.mu.Lock()
		st := node.snapshotStatusLocked(now)
		node.mu.Unlock()
		if err != nil {
			writePrettyJSON(w, http.StatusConflict, map[string]any{
				"error":       err.Error(),
				"node_status": st,
			})
			return
		}
		writePrettyJSON(w, http.StatusOK, map[string]any{
			"kv_entry":    kvEntry,
			"node_status": st,
		})
	})

	mux.HandleFunc("/kv/replicate", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Term     int     `json:"term"`
			LeaderID string  `json:"leader_id"`
			Entry    KVEntry `json:"entry"`
		}
		if err := decodeOptionalJSON(r, &req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		resp := node.ReplicateKV(time.Now(), req.Term, req.LeaderID, req.Entry, effectivePodIP(r))
		writePrettyJSON(w, http.StatusOK, resp)
	})

	mux.HandleFunc("/lock/acquire", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Owner        string `json:"owner"`
			FencingToken int    `json:"fencing_token"`
		}
		_ = decodeOptionalJSON(r, &req)
		if q := r.URL.Query().Get("owner"); q != "" {
			req.Owner = q
		}
		if q := r.URL.Query().Get("token"); q != "" {
			if v, err := strconv.Atoi(q); err == nil {
				req.FencingToken = v
			}
		}
		if req.Owner == "" {
			req.Owner = "Alice (checkout-svc)"
		}
		if req.FencingToken <= 0 {
			req.FencingToken = 101
		}

		now := time.Now()
		podIP := effectivePodIP(r)
		_, err := node.AcquireLock(now, req.Owner, req.FencingToken, podIP)
		node.mu.Lock()
		st := node.snapshotStatusLocked(now)
		node.mu.Unlock()
		if err != nil {
			writePrettyJSON(w, http.StatusConflict, map[string]any{
				"error":       err.Error(),
				"node_status": st,
			})
			return
		}
		writePrettyJSON(w, http.StatusOK, st)
	})

	mux.HandleFunc("/config", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Guard *bool `json:"guard"`
		}
		_ = decodeOptionalJSON(r, &req)
		guard := false
		if req.Guard != nil {
			guard = *req.Guard
		} else if q := r.URL.Query().Get("guard"); q != "" {
			guard = q == "1" || strings.EqualFold(q, "true")
		}
		node.SetGuardrail(guard)
		resp := node.SnapshotStatus(time.Now(), effectivePodIP(r))
		writePrettyJSON(w, http.StatusOK, resp)
	})

	mux.HandleFunc("/reset", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			NodeName string `json:"node_name"`
			Role     string `json:"role"`
			Term     int    `json:"term"`
			LeaderID string `json:"leader_id"`
			Guard    bool   `json:"guard"`
		}
		_ = decodeOptionalJSON(r, &req)
		resp := node.Reset(time.Now(), req.NodeName, req.Role, req.Term, req.LeaderID, req.Guard, effectivePodIP(r))
		writePrettyJSON(w, http.StatusOK, resp)
	})

	return mux
}

func currentPodIP() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return "unknown"
	}
	for _, addr := range addrs {
		if ipnet, ok := addr.(*net.IPNet); ok && !ipnet.IP.IsLoopback() && ipnet.IP.To4() != nil {
			return ipnet.IP.String()
		}
	}
	return "127.0.0.1"
}
