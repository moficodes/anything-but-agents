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
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestTimeWarpDetectsSuspendSplitBrain(t *testing.T) {
	t0 := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	node := newNaiveClusterNode(t0)

	// Normal 100ms tick -> 0 anomalies.
	node.ObserveClock(t0.Add(100*time.Millisecond), "10.0.0.1")
	if len(node.Anomalies) != 0 {
		t.Fatalf("expected 0 anomalies on normal 100ms tick, got %d", len(node.Anomalies))
	}

	// Simulate 12-second Suspend in GCS followed by Resume on pod 10.0.0.2.
	resp := node.Report(t0.Add(12100*time.Millisecond), "10.0.0.2")
	if len(node.Anomalies) != 1 {
		t.Fatalf("expected 1 anomaly after 12s suspend time jump, got %d", len(node.Anomalies))
	}
	if !strings.Contains(node.Anomalies[0].SplitBrainAlert, "CRITICAL SPLIT-BRAIN") {
		t.Fatalf("expected split-brain alert, got %s", node.Anomalies[0].SplitBrainAlert)
	}
	if !strings.HasPrefix(resp["status"].(string), "PANIC:") {
		t.Fatalf("expected PANIC status after time jump, got %v", resp["status"])
	}
}

func TestTimeWarpStartsHealthyAfterGoldenSnapshot(t *testing.T) {
	node := newNaiveClusterNode(time.Time{})

	// Ticks during Golden Snapshot creation or before first HTTP request are ignored.
	t0 := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	node.ObserveClock(t0, "10.0.0.1")

	// 40 minutes later, Step 1's first HTTP request arrives -> must start HEALTHY.
	t1 := t0.Add(40 * time.Minute)
	resp1 := node.Report(t1, "10.0.0.2")
	if !strings.HasPrefix(resp1["status"].(string), "HEALTHY") {
		t.Fatalf("expected HEALTHY on first request after Golden Snapshot, got %v", resp1["status"])
	}
	if len(node.Anomalies) != 0 {
		t.Fatalf("expected 0 anomalies on first request, got %d", len(node.Anomalies))
	}

	// Step 2 & 3: 5-second suspend after first request -> triggers PANIC & Split-Brain.
	resp2 := node.Report(t1.Add(5*time.Second), "10.0.0.3")
	if !strings.HasPrefix(resp2["status"].(string), "PANIC:") {
		t.Fatalf("expected PANIC after 5s suspend, got %v", resp2["status"])
	}
	if len(node.Anomalies) != 1 {
		t.Fatalf("expected 1 anomaly after 5s suspend, got %d", len(node.Anomalies))
	}
}

func TestActorZombieLeaderAcceptsConflictingLock(t *testing.T) {
	t0 := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	node := newNaiveClusterNode(t0)
	node.PromoteRole(t0, "raft-node-1", "LEADER", 1, "raft-node-1", "10.44.0.9")

	// Step 1: Healthy Term 1 lock acquired by Alice.
	entry1, err := node.AcquireLock(t0.Add(100*time.Millisecond), "Alice (checkout-svc)", 101, "10.44.0.9")
	if err != nil {
		t.Fatalf("unexpected error acquiring healthy lock: %v", err)
	}
	if entry1.ZombieSplitBrain {
		t.Fatalf("expected healthy lock not to be flagged as zombie split-brain")
	}

	// Step 2 & 3: Freeze node for 5.2 seconds (past 3s LeaseTTL), then wake on a new worker pod IP
	// with a conflicting write ("Mallory"). Because ClockJumpGuard is false, the restored heap
	// still believes it was LEADER (Term 1) and accepts the write as a ZOMBIE_LEADER!
	tWake := t0.Add(5300 * time.Millisecond)
	entry2, err := node.AcquireLock(tWake, "Mallory (rogue-worker)", 101, "10.44.1.14")
	if err != nil {
		t.Fatalf("expected unguarded zombie leader to accept conflicting lock, got error: %v", err)
	}
	if !entry2.ZombieSplitBrain {
		t.Fatalf("expected wakeup write after 5.2s freeze to be flagged as ZombieSplitBrain=true")
	}
	if node.Role != "ZOMBIE_LEADER" {
		t.Fatalf("expected Role=ZOMBIE_LEADER, got %s", node.Role)
	}
	if len(node.PodIPHistory) != 2 || node.PodIPHistory[1] != "10.44.1.14" {
		t.Fatalf("expected pod_ip_history to record migration to 10.44.1.14, got %v", node.PodIPHistory)
	}
	if node.NaiveTokenBucket < 500 {
		t.Fatalf("expected naive token bucket to overflow (>500 tokens), got %.1f", node.NaiveTokenBucket)
	}
}

func TestActorClockJumpGuardrailFencesStaleLeaseAndClampsTokens(t *testing.T) {
	t0 := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	node := newNaiveClusterNode(t0)
	node.PromoteRole(t0, "raft-node-1", "LEADER", 1, "raft-node-1", "10.44.0.9")
	node.SetGuardrail(true)

	_, err := node.AcquireLock(t0.Add(100*time.Millisecond), "Alice (checkout-svc)", 101, "10.44.0.9")
	if err != nil {
		t.Fatalf("unexpected error acquiring initial lock: %v", err)
	}

	// Freeze for 5.2 seconds, then attempt conflicting lock acquisition with ClockJumpGuard=true.
	tWake := t0.Add(5300 * time.Millisecond)
	_, err = node.AcquireLock(tWake, "Mallory (rogue-worker)", 101, "10.44.1.14")
	if err == nil {
		t.Fatalf("expected guarded actor to fence stale lease and reject conflicting lock write")
	}
	if node.Role != "STALE_LEADER_FENCED" {
		t.Fatalf("expected Role=STALE_LEADER_FENCED, got %s", node.Role)
	}
	// Token bucket was 20.0 after first 100ms tick; clamped wakeup tick adds only +10.0 tokens (total 30.0).
	if node.NaiveTokenBucket > 35.0 {
		t.Fatalf("expected guarded token bucket to clamp refill (<=35 tokens), got %.1f", node.NaiveTokenBucket)
	}
	if node.CurrentLock == nil || !strings.HasPrefix(node.CurrentLock.Owner, "Alice") {
		t.Fatalf("expected current lock on fenced node to remain Alice, got %+v", node.CurrentLock)
	}
}

func TestActorHTTPMuxEndpoints(t *testing.T) {
	node := newNaiveClusterNode(time.Time{})
	srv := httptest.NewServer(newMux(node))
	defer srv.Close()

	// Promote to Term 1 Leader
	promoteBody, _ := json.Marshal(map[string]any{
		"node_name": "raft-node-1",
		"role":      "LEADER",
		"term":      1,
		"leader_id": "raft-node-1",
	})
	resp, err := http.Post(srv.URL+"/raft/promote", "application/json", bytes.NewReader(promoteBody))
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /raft/promote failed: status=%v err=%v", resp, err)
	}
	_ = resp.Body.Close()

	// Acquire Lock for Alice
	lockBody, _ := json.Marshal(map[string]any{
		"owner":         "Alice",
		"fencing_token": 101,
	})
	resp, err = http.Post(srv.URL+"/lock/acquire", "application/json", bytes.NewReader(lockBody))
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /lock/acquire failed: status=%v err=%v", resp, err)
	}
	_ = resp.Body.Close()

	// Inspect status
	resp, err = http.Get(srv.URL + "/status")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /status failed: status=%v err=%v", resp, err)
	}
	defer resp.Body.Close()
	var parsed ActorStatusResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		t.Fatalf("failed to decode /status: %v", err)
	}
	if parsed.CurrentLock == nil || parsed.CurrentLock.Owner != "Alice" {
		t.Fatalf("expected current_lock.owner == Alice, got %+v", parsed.CurrentLock)
	}
}

func TestGatewayFourStepSplitBrainAndGuardrailFlow(t *testing.T) {
	backend := NewLocalSimulatorBackend("anything-but-agents", "timewarp")
	gw := NewGatewayServer(backend)
	srv := httptest.NewServer(gw.Mux())
	defer srv.Close()

	// Verify /dashboard serves HTML with Projector Light Mode toggle
	dashResp, err := http.Get(srv.URL + "/dashboard")
	if err != nil || dashResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /dashboard failed: %v", err)
	}
	dashBytes, _ := io.ReadAll(dashResp.Body)
	_ = dashResp.Body.Close()
	if !strings.Contains(string(dashBytes), "btnThemeToggle") {
		t.Fatalf("expected /dashboard HTML to contain Projector Light Mode toggle (btnThemeToggle)")
	}

	callStep := func(path string) ClusterLabState {
		r, err := http.Post(srv.URL+path, "application/json", nil)
		if err != nil || r.StatusCode != http.StatusOK {
			t.Fatalf("POST %s failed: status=%v err=%v", path, r, err)
		}
		defer r.Body.Close()
		var st ClusterLabState
		if err := json.NewDecoder(r.Body).Decode(&st); err != nil {
			t.Fatalf("decode %s failed: %v", path, err)
		}
		return st
	}

	// Step 1: Healthy Term 1 Quorum ("Alice")
	st1 := callStep("/api/step/1-quorum")
	if st1.SplitBrainActive || len(st1.ActiveLeaders) != 1 {
		t.Fatalf("Step 1 expected 1 active leader and no split-brain, got %+v", st1)
	}

	// Step 2: Freeze raft-node-1 in GCS (5.2s) & Elect Term 2 Leader ("Bob")
	st2 := callStep("/api/step/2-freeze-elect")
	if st2.Nodes[0].SubstrateState != "SUSPENDED" {
		t.Fatalf("Step 2 expected raft-node-1 to be SUSPENDED, got %s", st2.Nodes[0].SubstrateState)
	}
	if st2.ClockDriftMS < 5000 {
		t.Fatalf("Step 2 expected clock drift >= 5000ms, got %dms", st2.ClockDriftMS)
	}
	if st2.Nodes[1].ActorStatus.Role != "LEADER" || st2.Nodes[1].ActorStatus.Term != 2 {
		t.Fatalf("Step 2 expected raft-node-2 to be LEADER (Term 2), got %+v", st2.Nodes[1].ActorStatus)
	}

	// Step 3: Wake Zombie Leader raft-node-1 ("Mallory") -> Split-Brain!
	st3 := callStep("/api/step/3-wake-zombie")
	if !st3.SplitBrainActive {
		t.Fatalf("Step 3 expected SplitBrainActive=true, got false")
	}
	if len(st3.ActiveLeaders) != 2 {
		t.Fatalf("Step 3 expected 2 active leaders, got %v", st3.ActiveLeaders)
	}
	if !strings.HasPrefix(st3.Nodes[0].ActorStatus.CurrentLock.Owner, "Mallory") {
		t.Fatalf("Step 3 expected raft-node-1 lock owner Mallory, got %+v", st3.Nodes[0].ActorStatus.CurrentLock)
	}
	if !strings.HasPrefix(st3.Nodes[1].ActorStatus.CurrentLock.Owner, "Bob") {
		t.Fatalf("Step 3 expected raft-node-2 lock owner Bob, got %+v", st3.Nodes[1].ActorStatus.CurrentLock)
	}

	// Step 4: Replay with Wakeup Clock-Jump Guardrail -> Split-Brain Prevented!
	st4 := callStep("/api/step/4-guardrail")
	if st4.SplitBrainActive {
		t.Fatalf("Step 4 expected SplitBrainActive=false with guardrail enabled")
	}
	if !st4.SplitBrainGuarded {
		t.Fatalf("Step 4 expected SplitBrainGuarded=true")
	}
	if st4.Nodes[0].ActorStatus.Role != "STALE_LEADER_FENCED" {
		t.Fatalf("Step 4 expected raft-node-1 Role=STALE_LEADER_FENCED, got %s", st4.Nodes[0].ActorStatus.Role)
	}
}

func TestInteractiveDoubleSpendVaultAndForkedRaftLog(t *testing.T) {
	backend := NewLocalSimulatorBackend("anything-but-agents", "timewarp")
	gw := NewGatewayServer(backend)
	srv := httptest.NewServer(gw.Mux())
	defer srv.Close()

	callStep := func(path string) ClusterLabState {
		r, err := http.Post(srv.URL+path, "application/json", nil)
		if err != nil || r.StatusCode != http.StatusOK {
			t.Fatalf("POST %s failed: status=%v err=%v", path, r, err)
		}
		defer r.Body.Close()
		var st ClusterLabState
		if err := json.NewDecoder(r.Body).Decode(&st); err != nil {
			t.Fatalf("decode %s failed: %v", path, err)
		}
		return st
	}

	// Step 1: Escrow & KV database initialized at Log Index #1 across all 3 nodes
	st1 := callStep("/api/step/1-quorum")
	if st1.Nodes[0].ActorStatus.CurrentLock.LogIndex != 1 {
		t.Fatalf("Step 1 expected LogIndex=1 on raft-node-1, got %d", st1.Nodes[0].ActorStatus.CurrentLock.LogIndex)
	}
	if len(st1.Nodes[0].ActorStatus.KVStore) < 3 {
		t.Fatalf("Step 1 expected >=3 KVStore entries on raft-node-1, got %d", len(st1.Nodes[0].ActorStatus.KVStore))
	}
	if len(st1.KVConflicts) != 0 {
		t.Fatalf("Step 1 expected 0 KVConflicts, got %d", len(st1.KVConflicts))
	}

	// Step 2: Freeze raft-node-1 -> Term 2 Quorum (nodes 2 & 3) commits rev=2 for Bob
	st2 := callStep("/api/step/2-freeze-elect")
	if st2.Nodes[1].ActorStatus.CurrentLock.LogIndex != 2 {
		t.Fatalf("Step 2 expected LogIndex=2 on raft-node-2 for Bob, got %d", st2.Nodes[1].ActorStatus.CurrentLock.LogIndex)
	}
	if st2.AllocatedUnits != 1 || st2.AvailableUnits != 0 || st2.DoubleSpendActive {
		t.Fatalf("Step 2 expected 1 allocated / 0 available / no double-spend yet, got alloc=%d avail=%d double=%v",
			st2.AllocatedUnits, st2.AvailableUnits, st2.DoubleSpendActive)
	}

	// Step 3: Wake Zombie Leader raft-node-1 -> commits conflicting rev=2 (Term 1) for Mallory -> KV DATA CORRUPTION!
	st3 := callStep("/api/step/3-wake-zombie")
	if !st3.DoubleSpendActive || !st3.LogForkDetected {
		t.Fatalf("Step 3 expected DoubleSpendActive=true and LogForkDetected=true, got double=%v fork=%v",
			st3.DoubleSpendActive, st3.LogForkDetected)
	}
	if len(st3.KVConflicts) < 3 {
		t.Fatalf("Step 3 expected >=3 KVConflicts between Zombie Leader (Term 1) and Quorum Leader (Term 2), got %d", len(st3.KVConflicts))
	}

	// Test interactive POST /api/kv/put against Follower (409) vs Leaders (200)
	putFollowerBody, _ := json.Marshal(map[string]any{
		"target_node": "raft-node-3",
		"key":         "/app/feature_x",
		"value":       "follower-attempt",
	})
	respF, err := http.Post(srv.URL+"/api/kv/put", "application/json", bytes.NewReader(putFollowerBody))
	if err != nil || respF.StatusCode != http.StatusConflict {
		t.Fatalf("expected POST /api/kv/put to follower raft-node-3 to return 409 Conflict, got %v err=%v", respF, err)
	}
	_ = respF.Body.Close()

	putQuorumBody, _ := json.Marshal(map[string]any{
		"target_node": "raft-node-2",
		"key":         "/app/feature_x",
		"value":       "enabled-by-term2-quorum",
	})
	respQ, err := http.Post(srv.URL+"/api/kv/put", "application/json", bytes.NewReader(putQuorumBody))
	if err != nil || respQ.StatusCode != http.StatusOK {
		t.Fatalf("expected POST /api/kv/put to Term 2 leader raft-node-2 to succeed, got %v err=%v", respQ, err)
	}
	_ = respQ.Body.Close()

	putZombieBody, _ := json.Marshal(map[string]any{
		"target_node": "raft-node-1",
		"key":         "/app/feature_x",
		"value":       "corrupted-by-zombie-term1",
	})
	respZ, err := http.Post(srv.URL+"/api/kv/put", "application/json", bytes.NewReader(putZombieBody))
	if err != nil || respZ.StatusCode != http.StatusOK {
		t.Fatalf("expected POST /api/kv/put to unguarded Zombie Leader raft-node-1 to succeed, got %v err=%v", respZ, err)
	}
	var stAfterPut ClusterLabState
	_ = json.NewDecoder(respZ.Body).Decode(&stAfterPut)
	_ = respZ.Body.Close()

	foundCustomConflict := false
	for _, c := range stAfterPut.KVConflicts {
		if c.Key == "/app/feature_x" && c.Node1Value == "corrupted-by-zombie-term1" && c.QuorumValue == "enabled-by-term2-quorum" {
			foundCustomConflict = true
		}
	}
	if !foundCustomConflict {
		t.Fatalf("expected custom key /app/feature_x conflict in KVConflicts, got %+v", stAfterPut.KVConflicts)
	}

	// Step 4: Guardrail prevents double-spend and KV conflicts
	st4 := callStep("/api/step/4-guardrail")
	if st4.DoubleSpendActive || st4.LogForkDetected || len(st4.KVConflicts) != 0 {
		t.Fatalf("Step 4 expected 0 KVConflicts with guardrail active, got double=%v fork=%v conflicts=%d",
			st4.DoubleSpendActive, st4.LogForkDetected, len(st4.KVConflicts))
	}
}

func TestTimeWarpGatewayPresenterAuthAndGoogleGate(t *testing.T) {
	t.Setenv("ALLOWED_GOOGLE_DOMAINS", "google.com,cloud-demos.goog")
	t.Setenv("GOOGLE_CLIENT_ID", "test-client-id.apps.googleusercontent.com")

	backend := NewLocalSimulatorBackend("anything-but-agents", "timewarp")
	gw := NewGatewayServer(backend, "tw-secret-token")
	mux := gw.Mux()

	unauthRec := httptest.NewRecorder()
	mux.ServeHTTP(unauthRec, httptest.NewRequest(http.MethodGet, "/dashboard", nil))
	if unauthRec.Code != http.StatusUnauthorized || !strings.Contains(unauthRec.Body.String(), "Presenter Sign-In Required") {
		t.Fatalf("expected 401 Google Sign-In Gate on unauthenticated /dashboard, got %d", unauthRec.Code)
	}
	if !strings.Contains(unauthRec.Body.String(), "@cloud-demos.goog") {
		t.Fatalf("expected @cloud-demos.goog chip in gate HTML")
	}

	unauthAPIRec := httptest.NewRecorder()
	mux.ServeHTTP(unauthAPIRec, httptest.NewRequest(http.MethodGet, "/api/state", nil))
	if unauthAPIRec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 on unauthenticated /api/state, got %d", unauthAPIRec.Code)
	}

	unlockRec := httptest.NewRecorder()
	mux.ServeHTTP(unlockRec, httptest.NewRequest(http.MethodGet, "/dashboard?key=tw-secret-token", nil))
	if unlockRec.Code != http.StatusSeeOther || unlockRec.Header().Get("Location") != "/dashboard" {
		t.Fatalf("expected 303 redirect to /dashboard, got %d (%q)", unlockRec.Code, unlockRec.Header().Get("Location"))
	}
	cookies := unlockRec.Result().Cookies()
	if len(cookies) == 0 || cookies[0].Name != PresenterCookieName {
		t.Fatalf("expected %s cookie at cookies[0], got %+v", PresenterCookieName, cookies)
	}

	dashReq := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	for _, c := range cookies {
		dashReq.AddCookie(c)
	}
	dashRec := httptest.NewRecorder()
	mux.ServeHTTP(dashRec, dashReq)
	if dashRec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK on /dashboard with cookie, got %d", dashRec.Code)
	}
}



