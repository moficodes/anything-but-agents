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
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTenantDBPersistenceAcrossDataScopeRestart(t *testing.T) {
	dir := t.TempDir()

	db1, err := openTenantDB(dir)
	if err != nil {
		t.Fatalf("openTenantDB: %v", err)
	}
	if err := db1.Put("plan", "enterprise"); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := db1.Put("seats", "250"); err != nil {
		t.Fatalf("Put: %v", err)
	}

	// Simulate a DATA-scope Resume: process restarts from scratch with the same DurableDir (/data).
	db2, err := openTenantDB(dir)
	if err != nil {
		t.Fatalf("reopen TenantDB: %v", err)
	}
	snap := db2.Snapshot("10.0.0.9")
	if snap["rows_in_durable_dir"] != 2 {
		t.Fatalf("expected 2 rows restored from DurableDir WAL, got %v", snap["rows_in_durable_dir"])
	}
	if snap["wal_replayed_on_boot"] != 2 {
		t.Fatalf("expected 2 WAL entries replayed on boot, got %v", snap["wal_replayed_on_boot"])
	}
	if snap["recovery_mode"] != "COLD_DATA_REPLAY" {
		t.Fatalf("expected recovery_mode COLD_DATA_REPLAY after DATA-scope restart, got %v", snap["recovery_mode"])
	}
	if snap["ram_buffer_pool_bytes"] != 16*1024*1024 {
		t.Fatalf("expected 16MB ram_buffer_pool_bytes, got %v", snap["ram_buffer_pool_bytes"])
	}
}

func TestTenantDBGoldenSnapshotLazyWALReplayAndPodIPHistory(t *testing.T) {
	dir := t.TempDir()

	// 1. ActorTemplate takes Golden Snapshot before any WAL exists on /data.
	goldenBootDB, err := openTenantDB(dir)
	if err != nil {
		t.Fatalf("openTenantDB golden: %v", err)
	}
	if len(goldenBootDB.RAMBufferPoolSlab) != 0 {
		t.Fatalf("expected 0 dirty buffer pool bytes at Golden Snapshot boot, got %d", len(goldenBootDB.RAMBufferPoolSlab))
	}

	// 2. Write two rows on Worker Pod 10.120.1.10 and 10.120.2.20
	if err := goldenBootDB.PutRow("invoices", "inv-1001", "Acme Annual Enterprise License", 120000, "10.120.1.10"); err != nil {
		t.Fatalf("PutRow 1: %v", err)
	}
	if err := goldenBootDB.PutRow("subscriptions", "tier", "enterprise-plus", 22000, "10.120.2.20"); err != nil {
		t.Fatalf("PutRow 2: %v", err)
	}

	// 3. Simulate Pause (FULL scope): heap remains intact.
	hotSnap := goldenBootDB.BuildSnapshot("10.120.2.20")
	if hotSnap.RecoveryMode != "HOT_RAM_RESUME" {
		t.Fatalf("expected HOT_RAM_RESUME on warm heap, got %s", hotSnap.RecoveryMode)
	}
	if hotSnap.RAMQueriesSinceBoot != 3 {
		t.Fatalf("expected 3 ram_queries_since_boot after 2 writes + 1 read, got %d", hotSnap.RAMQueriesSinceBoot)
	}
	if hotSnap.MaterializedStats.TotalRevenueUSD != 142000 {
		t.Fatalf("expected TotalRevenueUSD=142000, got %d", hotSnap.MaterializedStats.TotalRevenueUSD)
	}

	// 4. Simulate Suspend (DATA scope): container restores from Golden Snapshot (empty heap)
	// while /data/wal.jsonl already has the 2 rows on disk.
	restoredFromGolden := &TenantDB{
		DataDir:           dir,
		WALPath:           goldenBootDB.WALPath,
		ProcessBootedAt:   goldenBootDB.ProcessBootedAt,
		BufferPoolTargetB: 16 * 1024 * 1024,
		RAMBufferPool:     make(map[string]Row),
	}
	coldSnap := restoredFromGolden.BuildSnapshot("10.120.3.30")
	if coldSnap.RecoveryMode != "COLD_DATA_REPLAY" {
		t.Fatalf("expected COLD_DATA_REPLAY when waking from Golden Snapshot with existing WAL, got %s", coldSnap.RecoveryMode)
	}
	if coldSnap.WALReplayedOnBoot != 2 {
		t.Fatalf("expected 2 WAL entries replayed, got %d", coldSnap.WALReplayedOnBoot)
	}
	if coldSnap.RAMQueriesSinceBoot != 1 {
		t.Fatalf("expected ram_queries_since_boot reset to 1 after DATA restore, got %d", coldSnap.RAMQueriesSinceBoot)
	}
	if len(coldSnap.PodIPHistory) != 3 {
		t.Fatalf("expected 3 pod IPs in history across WAL + current pod, got %v", coldSnap.PodIPHistory)
	}
	if coldSnap.GCSSnapshotSavingsPct < 99.9 {
		t.Fatalf("expected >99.9%% GCS snapshot savings, got %f", coldSnap.GCSSnapshotSavingsPct)
	}
}

func TestLocalSimulatorBackendLifecycleMultiplexingAndGDPR(t *testing.T) {
	ctx := context.Background()
	sim := NewLocalSimulatorBackend("anything-but-agents", "tenantdb")

	// 1. Seed all 4 customer databases across 3 worker slots.
	st, err := sim.SeedFleet(ctx, "all")
	if err != nil {
		t.Fatalf("SeedFleet: %v", err)
	}
	if len(st.Tenants) != 4 {
		t.Fatalf("expected 4 tenants, got %d", len(st.Tenants))
	}
	if st.Summary.RunningOnWorkers != 3 || st.Summary.PausedFullCount != 1 {
		t.Fatalf("expected 3 running + 1 multiplexed PAUSED_FULL across 3 workers, got running=%d paused=%d",
			st.Summary.RunningOnWorkers, st.Summary.PausedFullCount)
	}

	// 2. Pause db-acme (FULL scope) -> Query db-acme: hot RAM buffer pool stays intact.
	if _, err := sim.PauseTenant(ctx, "db-acme"); err != nil {
		t.Fatalf("PauseTenant: %v", err)
	}
	hotSnap, err := sim.QueryTenant(ctx, "db-acme", "", "", "", 0)
	if err != nil {
		t.Fatalf("QueryTenant after Pause: %v", err)
	}
	if hotSnap.RecoveryMode != "HOT_RAM_RESUME" {
		t.Fatalf("expected HOT_RAM_RESUME after FULL pause, got %s", hotSnap.RecoveryMode)
	}
	if hotSnap.RAMQueriesSinceBoot <= 1 {
		t.Fatalf("expected ram_queries_since_boot > 1 after FULL pause, got %d", hotSnap.RAMQueriesSinceBoot)
	}

	// 3. Suspend db-acme (DATA scope) -> Query db-acme: RAM discarded, WAL replayed in <1ms, pod IP rotates.
	if _, err := sim.SuspendTenant(ctx, "db-acme"); err != nil {
		t.Fatalf("SuspendTenant: %v", err)
	}
	coldSnap, err := sim.QueryTenant(ctx, "db-acme", "", "", "", 0)
	if err != nil {
		t.Fatalf("QueryTenant after Suspend: %v", err)
	}
	if coldSnap.RecoveryMode != "COLD_DATA_REPLAY" {
		t.Fatalf("expected COLD_DATA_REPLAY after DATA suspend, got %s", coldSnap.RecoveryMode)
	}
	if coldSnap.WALReplayedOnBoot != 3 {
		t.Fatalf("expected 3 WAL entries replayed on boot, got %d", coldSnap.WALReplayedOnBoot)
	}
	if coldSnap.RAMQueriesSinceBoot != 1 {
		t.Fatalf("expected ram_queries_since_boot reset to 1, got %d", coldSnap.RAMQueriesSinceBoot)
	}
	if len(coldSnap.PodIPHistory) < 2 {
		t.Fatalf("expected pod_ip_history to show migration across worker pods, got %v", coldSnap.PodIPHistory)
	}

	// 3b. Suspend db-acme again and wake it directly via a write (+ Append Invoice):
	// verify both the new WAL row (#4) is persisted AND recovery_mode remains COLD_DATA_REPLAY.
	if _, err := sim.SuspendTenant(ctx, "db-acme"); err != nil {
		t.Fatalf("SuspendTenant (second): %v", err)
	}
	writeWakeSnap, err := sim.QueryTenant(ctx, "db-acme", "invoices", "inv-2026-04", "42000", 42000)
	if err != nil {
		t.Fatalf("QueryTenant write after Suspend: %v", err)
	}
	if writeWakeSnap.RecoveryMode != "COLD_DATA_REPLAY" {
		t.Fatalf("expected COLD_DATA_REPLAY when waking suspended actor via write, got %s", writeWakeSnap.RecoveryMode)
	}
	if writeWakeSnap.RAMBufferPoolStatus != "REHYDRATED_FROM_WAL" {
		t.Fatalf("expected REHYDRATED_FROM_WAL when waking suspended actor via write, got %s", writeWakeSnap.RAMBufferPoolStatus)
	}
	if writeWakeSnap.RowsInDurableDir != 4 {
		t.Fatalf("expected 4 rows in durable dir after write-on-wake, got %d", writeWakeSnap.RowsInDurableDir)
	}
	if writeWakeSnap.WALReplayedOnBoot != 3 {
		t.Fatalf("expected 3 prior WAL entries replayed before write, got %d", writeWakeSnap.WALReplayedOnBoot)
	}

	// 4. GDPR delete db-acme -> verify db-acme returns 404 TENANT_SHREDDED while db-globex remains 100% intact.
	receipt, err := sim.GDPRDeleteTenant(ctx, "db-acme")
	if err != nil {
		t.Fatalf("GDPRDeleteTenant: %v", err)
	}
	if !receipt.ComputeShredded || !receipt.DurableDirShredded {
		t.Fatalf("expected full GDPR shred receipt, got %+v", receipt)
	}
	if _, err := sim.QueryTenant(ctx, "db-acme", "", "", "", 0); err == nil {
		t.Fatalf("expected querying shredded tenant db-acme to fail with 404 TENANT_SHREDDED")
	}
	globexSnap, err := sim.QueryTenant(ctx, "db-globex", "", "", "", 0)
	if err != nil {
		t.Fatalf("expected sibling tenant db-globex to remain online, got err: %v", err)
	}
	if globexSnap.RowsInDurableDir != 3 {
		t.Fatalf("expected db-globex to have 3 rows intact, got %d", globexSnap.RowsInDurableDir)
	}
}

func TestGatewayServerEndpointsAndPresenterAuth(t *testing.T) {
	sim := NewLocalSimulatorBackend("anything-but-agents", "tenantdb")
	gw := NewGatewayServer(sim, "secret-stage-key")
	srv := httptest.NewServer(gw.Mux())
	defer srv.Close()

	// 1. Unauthenticated /api/state should return 401
	res, err := http.Get(srv.URL + "/api/state")
	if err != nil {
		t.Fatalf("GET /api/state: %v", err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 without presenter token, got %d", res.StatusCode)
	}

	// 2. Unlock via /dashboard?key=secret-stage-key (verify 303 redirect + cookie)
	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	unlockRes, err := client.Get(srv.URL + "/dashboard?key=secret-stage-key")
	if err != nil {
		t.Fatalf("GET /dashboard?key=...: %v", err)
	}
	unlockRes.Body.Close()
	if unlockRes.StatusCode != http.StatusSeeOther {
		t.Fatalf("expected 303 redirect on /dashboard?key=..., got %d", unlockRes.StatusCode)
	}
	cookies := unlockRes.Cookies()
	if len(cookies) == 0 || cookies[0].Name != presenterCookieName {
		t.Fatalf("expected %s cookie to be set, got %v", presenterCookieName, cookies)
	}

	// 3. Seed fleet using X-Presenter-Key header
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/tenant/seed?actor=all", nil)
	req.Header.Set("X-Presenter-Key", "secret-stage-key")
	seedRes, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /api/tenant/seed: %v", err)
	}
	defer seedRes.Body.Close()
	if seedRes.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 on seed, got %d", seedRes.StatusCode)
	}
	var st FleetStateResponse
	if err := json.NewDecoder(seedRes.Body).Decode(&st); err != nil {
		t.Fatalf("decode FleetStateResponse: %v", err)
	}
	if st.Summary.GCSSavingsPct < 99.9 {
		t.Fatalf("expected >99.9%% GCS savings in summary, got %f", st.Summary.GCSSavingsPct)
	}

	// 4. Verify UI HTML includes Projector Light Mode toggle buttons
	uiReq, _ := http.NewRequest(http.MethodGet, srv.URL+"/dashboard", nil)
	uiReq.AddCookie(cookies[0])
	uiRes, err := http.DefaultClient.Do(uiReq)
	if err != nil {
		t.Fatalf("GET /dashboard with cookie: %v", err)
	}
	defer uiRes.Body.Close()
	if uiRes.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 on /dashboard with cookie, got %d", uiRes.StatusCode)
	}
	if !strings.Contains(string(indexHTML), "theme-toggle-light") {
		t.Fatalf("expected index.html to include Projector Light Mode toggle")
	}
}

func TestParseClusterActorsOutput(t *testing.T) {
	raw := `ATESPACE              NAME                    TEMPLATE                        STATE                   WORKER POD                                                 WORKER IP     VERSION   AGE
anything-but-agents   db-acme                 anything-but-agents/tenantdb    ACTOR_STATE_RUNNING     anything-but-agents/anything-but-agents-5699564879-gf8q9   10.120.0.11   1         3m10s
anything-but-agents   db-globex               anything-but-agents/tenantdb    ACTOR_STATE_PAUSED      anything-but-agents/anything-but-agents-5699564879-fn4f8   10.120.1.22   2         2m00s
anything-but-agents   db-initech              anything-but-agents/tenantdb    ACTOR_STATE_SUSPENDED   <none>                                                                   3         1m00s`

	parsed := parseClusterActorsOutput(raw)
	if len(parsed) != 3 {
		t.Fatalf("expected 3 parsed actors, got %d", len(parsed))
	}
	if parsed[0].Name != "db-acme" || parsed[0].State != "ACTOR_STATE_RUNNING" || parsed[0].WorkerIP != "10.120.0.11" {
		t.Fatalf("unexpected parsed[0]: %+v", parsed[0])
	}
	if parsed[1].Name != "db-globex" || parsed[1].State != "ACTOR_STATE_PAUSED" || parsed[1].WorkerIP != "10.120.1.22" {
		t.Fatalf("unexpected parsed[1]: %+v", parsed[1])
	}
	if parsed[2].Name != "db-initech" || parsed[2].State != "ACTOR_STATE_SUSPENDED" || parsed[2].WorkerIP != "" {
		t.Fatalf("unexpected parsed[2]: %+v", parsed[2])
	}
}

func TestNativeSnapshotFidelityVolumesAndDualTemplates(t *testing.T) {
	ctx := context.Background()
	sim := NewLocalSimulatorBackend("anything-but-agents", "tenantdb")

	st, err := sim.SeedFleet(ctx, "all")
	if err != nil {
		t.Fatalf("SeedFleet: %v", err)
	}

	byActor := make(map[string]TenantTracker, len(st.Tenants))
	for _, tr := range st.Tenants {
		byActor[tr.ActorName] = tr
	}

	if byActor["db-acme"].Template != "tenantdb" || byActor["db-acme"].PreferredFidelity != "SNAPSHOT_FIDELITY_MEMORY" {
		t.Fatalf("expected db-acme on tenantdb / SNAPSHOT_FIDELITY_MEMORY, got %+v", byActor["db-acme"])
	}
	if byActor["db-initech"].Template != "tenantdb-volumes" || byActor["db-initech"].PreferredFidelity != "SNAPSHOT_FIDELITY_VOLUMES" {
		t.Fatalf("expected db-initech on tenantdb-volumes / SNAPSHOT_FIDELITY_VOLUMES, got %+v", byActor["db-initech"])
	}

	// Suspend db-initech -> verify LastSnapshot.SnapshotFidelity is SNAPSHOT_FIDELITY_VOLUMES
	suspendedTr, err := sim.SuspendTenant(ctx, "db-initech")
	if err != nil {
		t.Fatalf("SuspendTenant db-initech: %v", err)
	}
	if suspendedTr.LastSnapshot == nil || suspendedTr.LastSnapshot.SnapshotFidelity != "SNAPSHOT_FIDELITY_VOLUMES" {
		t.Fatalf("expected suspended db-initech snapshot_fidelity=SNAPSHOT_FIDELITY_VOLUMES, got %+v", suspendedTr.LastSnapshot)
	}

	// Wake db-initech -> verify COLD_DATA_REPLAY and SNAPSHOT_FIDELITY_VOLUMES
	wakeSnap, err := sim.QueryTenant(ctx, "db-initech", "", "", "", 0)
	if err != nil {
		t.Fatalf("QueryTenant db-initech: %v", err)
	}
	if wakeSnap.RecoveryMode != "COLD_DATA_REPLAY" || wakeSnap.SnapshotFidelity != "SNAPSHOT_FIDELITY_VOLUMES" {
		t.Fatalf("expected COLD_DATA_REPLAY + SNAPSHOT_FIDELITY_VOLUMES on wake, got mode=%s fidelity=%s",
			wakeSnap.RecoveryMode, wakeSnap.SnapshotFidelity)
	}
}


