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
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestColdStartReadyzTransitionAndQuery(t *testing.T) {
	engine := newWarmHeapEngine(100)
	mux := newMux(engine)

	// Before initialization finishes, /readyz must return 503 so Substrate does not snapshot early.
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 before init finishes, got %d", rec.Code)
	}

	// /statusz should always return 200 OK even during cold boot.
	statusReq := httptest.NewRequest(http.MethodGet, "/statusz", nil)
	statusRec := httptest.NewRecorder()
	mux.ServeHTTP(statusRec, statusReq)
	if statusRec.Code != http.StatusOK {
		t.Fatalf("expected 200 from /statusz during init, got %d", statusRec.Code)
	}

	engine.RunExpensiveInitialization(0)

	// After initialization, /readyz returns 200 OK and primes up to 100 (25 primes, largest 97) + search index are in RAM.
	rec2 := httptest.NewRecorder()
	mux.ServeHTTP(rec2, req)
	if rec2.Code != http.StatusOK {
		t.Fatalf("expected 200 after init finishes, got %d", rec2.Code)
	}

	resp := engine.HandleSearchQuery("kubernetes autoscaler", "fn_main", "10.0.0.5")
	if resp["precomputed_prime_count"] != 25 || resp["precomputed_largest_prime"] != 97 {
		t.Fatalf("unexpected prime sieve results: %v", resp)
	}
	if resp["golden_heap_fingerprint"] == "" {
		t.Fatalf("expected non-empty golden_heap_fingerprint")
	}
	if resp["docs_indexed"].(int) <= 0 || resp["trigrams_indexed"].(int) <= 0 {
		t.Fatalf("expected populated search index in RAM, got docs=%v trigrams=%v", resp["docs_indexed"], resp["trigrams_indexed"])
	}
	hits, ok := resp["search_hits"].([]SearchHit)
	if !ok || len(hits) == 0 {
		t.Fatalf("expected non-empty ranked search_hits, got %T %v", resp["search_hits"], resp["search_hits"])
	}

	// Simulate cross-pod actor migration and verify pod_ip_history grows while preserving heap state.
	resp2 := engine.HandleSearchQuery("gvisor snapshot", "fn_second", "10.0.0.9")
	history, ok := resp2["pod_ip_history"].([]string)
	if !ok || len(history) != 2 || history[0] != "10.0.0.5" || history[1] != "10.0.0.9" {
		t.Fatalf("expected pod_ip_history [10.0.0.5, 10.0.0.9], got %v", resp2["pod_ip_history"])
	}
}

func TestColdStartConcurrencyLoadShedding(t *testing.T) {
	engine := NewWarmHeapEngineWithConfig(WorkerConfig{
		SieveLimit:       100,
		CorpusDocs:       200,
		MaxConcurrent:    2,
		MinQueryDuration: 80 * time.Millisecond,
	})
	engine.RunExpensiveInitialization(0)
	mux := newMux(engine)

	var okCount, shedCount atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodGet, "/search?q=kubernetes+autoscaler", nil)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)
			switch rec.Code {
			case http.StatusOK:
				okCount.Add(1)
			case http.StatusTooManyRequests:
				shedCount.Add(1)
			default:
				t.Errorf("unexpected status code: %d", rec.Code)
			}
		}()
	}
	wg.Wait()

	if okCount.Load() != 2 {
		t.Fatalf("expected exactly 2 concurrent requests to succeed, got %d", okCount.Load())
	}
	if shedCount.Load() != 3 {
		t.Fatalf("expected 3 overflow requests to shed with 429, got %d", shedCount.Load())
	}
}

func TestExternalMetricsAPIAndTLS(t *testing.T) {
	cert, err := generateSelfSignedCert()
	if err != nil || len(cert.Certificate) == 0 {
		t.Fatalf("failed to generate self-signed TLS cert: %v", err)
	}

	gw := NewGatewayServer(GatewayConfig{
		Mode:       "local",
		CorpusDocs: 200,
		SieveLimit: 100,
	})
	gw.SetTargetQPS(60, "SPIKE")

	extMux := gw.NewExternalMetricsMux()
	req := httptest.NewRequest(http.MethodGet, "/apis/external.metrics.k8s.io/v1beta1/namespaces/anything-but-agents/coldstart_lb_qps?labelSelector=backend%3Dsubstrate", nil)
	rec := httptest.NewRecorder()
	extMux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 from external metrics endpoint, got %d", rec.Code)
	}
	var list ExternalMetricValueList
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("failed to decode ExternalMetricValueList: %v", err)
	}
	if len(list.Items) != 1 || list.Items[0].Value != "60" || list.Items[0].MetricLabels["backend"] != "substrate" {
		t.Fatalf("unexpected external metric items: %+v", list.Items)
	}
}

func TestHPAColdStartQPSAdvantage(t *testing.T) {
	gw := NewGatewayServer(GatewayConfig{
		Mode:                   "local",
		K8sColdBootDelay:       500 * time.Millisecond,
		SubstrateWakeDelay:     10 * time.Millisecond,
		WorkerMinQueryDuration: 100 * time.Millisecond,
		WorkerMaxConcurrent:    2,
		CorpusDocs:             300,
		SieveLimit:             100,
		TickInterval:           25 * time.Millisecond,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	gw.StartBackgroundLoops(ctx)

	// 1. Unauthenticated /dashboard and /api/probe must be rejected with 401 Unauthorized
	unauthReq := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	unauthRec := httptest.NewRecorder()
	gw.Mux().ServeHTTP(unauthRec, unauthReq)
	if unauthRec.Code != http.StatusUnauthorized || !strings.Contains(unauthRec.Body.String(), "Presenter Key Required") {
		t.Fatalf("expected 401 Presenter Key Required for unauthenticated /dashboard, got %d", unauthRec.Code)
	}
	unauthProbeReq := httptest.NewRequest(http.MethodPost, "/api/probe?q=gvisor+snapshot", nil)
	unauthProbeRec := httptest.NewRecorder()
	gw.Mux().ServeHTTP(unauthProbeRec, unauthProbeReq)
	if unauthProbeRec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for unauthenticated /api/probe, got %d", unauthProbeRec.Code)
	}

	// 2. Unlocking /dashboard?key=<PRESENTER_TOKEN> sets HttpOnly cookie and redirects (303) to /dashboard
	unlockReq := httptest.NewRequest(http.MethodGet, "/dashboard?key="+gw.PresenterKey(), nil)
	unlockRec := httptest.NewRecorder()
	gw.Mux().ServeHTTP(unlockRec, unlockReq)
	if unlockRec.Code != http.StatusSeeOther {
		t.Fatalf("expected 303 redirect on /dashboard?key=..., got %d", unlockRec.Code)
	}
	cookies := unlockRec.Result().Cookies()
	if len(cookies) == 0 || cookies[0].Name != PresenterCookieName {
		t.Fatalf("expected %s cookie on unlock, got %+v", PresenterCookieName, cookies)
	}

	// 3. Verify dashboard HTML serves cleanly with Hardware Telemetry Rack markers & security headers once authenticated
	dashReq := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	dashReq.AddCookie(cookies[0])
	dashRec := httptest.NewRecorder()
	gw.Mux().ServeHTTP(dashRec, dashReq)
	bodyStr := dashRec.Body.String()
	if dashRec.Code != http.StatusOK || !strings.Contains(bodyStr, "Production HPA + Cold-Start QPS Benchmark") {
		t.Fatalf("expected dashboard HTML, got status=%d", dashRec.Code)
	}
	for _, marker := range []string{
		"GOLDEN HEAP FINGERPRINT PROOF",
		"6-STAGE INDEX BUILD",
		"heapProofBadge",
		"k8sChart",
		"subChart",
		"btnThemeToggle",
		`[data-theme="light"]`,
	} {
		if !strings.Contains(bodyStr, marker) {
			t.Fatalf("expected redesigned dashboard HTML to contain %q", marker)
		}
	}
	if dashRec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("expected X-Content-Type-Options=nosniff header on /dashboard")
	}

	// 4. Verify /api/probe dispatches a synchronized dual-backend search probe when authenticated
	probeReq := httptest.NewRequest(http.MethodPost, "/api/probe?q=gvisor+snapshot", nil)
	probeReq.Header.Set(PresenterHeaderName, gw.PresenterKey())
	probeRec := httptest.NewRecorder()
	gw.Mux().ServeHTTP(probeRec, probeReq)
	if probeRec.Code != http.StatusOK {
		t.Fatalf("expected 200 from authenticated /api/probe, got %d", probeRec.Code)
	}

	// Run a 350ms spike at 60 QPS: K8s pods 2 & 3 are still in their 500ms cold-boot window,
	// while Substrate actors 2 & 3 wake from the Golden Snapshot in 10ms.
	gw.RunSpikeScenario(ctx, 15, 60, 50*time.Millisecond, 350*time.Millisecond, 50*time.Millisecond)

	st := gw.StateSnapshot()
	if st.Substrate.TotalGoodput <= st.K8s.TotalGoodput {
		t.Fatalf("expected Substrate total goodput (%d) to exceed K8s cold-start goodput (%d)",
			st.Substrate.TotalGoodput, st.K8s.TotalGoodput)
	}
	if st.K8s.TotalShed == 0 {
		t.Fatalf("expected K8s side to shed overflow traffic while pods 2 & 3 cold-boot, got 0 shed")
	}

	// Verify all 3 Substrate actor replicas share the exact same GoldenHeapFingerprint
	fp0 := st.Substrate.Replicas[0].GoldenHeapFingerprint
	for _, rep := range st.Substrate.Replicas {
		if rep.GoldenHeapFingerprint != fp0 {
			t.Fatalf("expected all Substrate clones to share fingerprint %q, got %q on %s", fp0, rep.GoldenHeapFingerprint, rep.Name)
		}
	}
}

func TestK8sRouterL7RoundRobinAndServiceProxy(t *testing.T) {
	// Spin up two mock K8s worker pods returning distinct current_pod_ip values.
	eng1 := NewWarmHeapEngineWithConfig(WorkerConfig{SieveLimit: 100, CorpusDocs: 100, MaxConcurrent: 2, MinQueryDuration: 10 * time.Millisecond})
	eng1.RunExpensiveInitialization(0)
	srv1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(eng1.HandleSearchQuery(r.URL.Query().Get("q"), "fn_test", "10.244.0.11"))
	}))
	defer srv1.Close()

	eng2 := NewWarmHeapEngineWithConfig(WorkerConfig{SieveLimit: 100, CorpusDocs: 100, MaxConcurrent: 2, MinQueryDuration: 10 * time.Millisecond})
	eng2.RunExpensiveInitialization(0)
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(eng2.HandleSearchQuery(r.URL.Query().Get("q"), "fn_test", "10.244.0.12"))
	}))
	defer srv2.Close()

	router := NewK8sRouterServer("anything-but-agents", "coldstart-k8s", "")
	router.mu.Lock()
	router.endpoints = []string{srv1.URL, srv2.URL}
	router.mu.Unlock()

	routerSrv := httptest.NewServer(router.Mux())
	defer routerSrv.Close()

	gw := NewGatewayServer(GatewayConfig{
		Mode:          "gateway",
		K8sServiceURL: routerSrv.URL,
		CorpusDocs:    100,
		SieveLimit:    100,
	})
	gw.mu.Lock()
	gw.localK8sSlots[0].podIP = "10.244.0.11"
	gw.localK8sSlots[0].state = "READY"
	gw.localK8sSlots[1].podIP = "10.244.0.12"
	gw.localK8sSlots[1].state = "READY"
	gw.mu.Unlock()

	for i := 0; i < 4; i++ {
		gw.sendOneK8sRequest("kubernetes autoscaler")
	}

	st := gw.StateSnapshot()
	if st.K8s.Replicas[0].ServedCount != 2 || st.K8s.Replicas[1].ServedCount != 2 {
		t.Fatalf("expected 2 requests attributed to each K8s pod via coldstart-k8s-router, got pod1=%d pod2=%d",
			st.K8s.Replicas[0].ServedCount, st.K8s.Replicas[1].ServedCount)
	}
}

func TestSubstrateWakeOnHTTPAndConcurrencyLoadBalancing(t *testing.T) {
	// Spin up 3 real WarmHeapEngine worker muxes behind a mock atenet-router that models
	// ActorResumer singleflight wake-on-HTTP: coldstart-1 starts RUNNING, while coldstart-2
	// and coldstart-3 start SUSPENDED and hold concurrent requests for 140ms while restoring
	// before releasing all held callers simultaneously to the worker mux.
	type mockActor struct {
		mux       *http.ServeMux
		mu        sync.Mutex
		running   bool
		waking    chan struct{}
		wakeDelay time.Duration
	}
	actors := make(map[string]*mockActor, 3)
	for i := 1; i <= 3; i++ {
		eng := NewWarmHeapEngineWithConfig(WorkerConfig{
			SieveLimit:       100,
			CorpusDocs:       100,
			MaxConcurrent:    4,
			MinQueryDuration: 80 * time.Millisecond,
		})
		eng.RunExpensiveInitialization(0)
		name := "anything-but-agents/coldstart-" + string(rune('0'+i))
		actors[name] = &mockActor{
			mux:       newMux(eng),
			running:   i == 1,
			wakeDelay: 220 * time.Millisecond,
		}
	}

	mockAtenet := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		target := r.Header.Get("ate-target-actor")
		actor := actors[target]
		if actor == nil {
			http.Error(w, "unknown actor", http.StatusNotFound)
			return
		}
		actor.mu.Lock()
		if !actor.running {
			if actor.waking == nil {
				ch := make(chan struct{})
				actor.waking = ch
				delay := actor.wakeDelay
				go func() {
					time.Sleep(delay)
					actor.mu.Lock()
					actor.running = true
					actor.waking = nil
					actor.mu.Unlock()
					close(ch)
				}()
			}
			waitCh := actor.waking
			actor.mu.Unlock()
			select {
			case <-waitCh:
			case <-r.Context().Done():
				return
			}
		} else {
			actor.mu.Unlock()
		}
		actor.mux.ServeHTTP(w, r)
	}))
	defer mockAtenet.Close()

	// Mock K8s router with a single Ready pod so both sides run in gateway mode.
	k8sEng := NewWarmHeapEngineWithConfig(WorkerConfig{
		SieveLimit:       100,
		CorpusDocs:       100,
		MaxConcurrent:    4,
		MinQueryDuration: 80 * time.Millisecond,
	})
	k8sEng.RunExpensiveInitialization(0)
	k8sSrv := httptest.NewServer(newMux(k8sEng))
	defer k8sSrv.Close()

	gw := NewGatewayServer(GatewayConfig{
		Mode:                   "gateway",
		RouterURL:              mockAtenet.URL,
		K8sServiceURL:          k8sSrv.URL,
		KubectlAteBin:          "true",
		K8sColdBootDelay:       1200 * time.Millisecond,
		SubstrateWakeDelay:     140 * time.Millisecond,
		WorkerMinQueryDuration: 80 * time.Millisecond,
		WorkerMaxConcurrent:    4,
		CorpusDocs:             100,
		SieveLimit:             100,
		TickInterval:           50 * time.Millisecond,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	gw.StartBackgroundLoops(ctx)

	// Drive a 60 QPS spike (matching 3 actors * 4 slots / ~80ms -> well within 3-actor capacity,
	// while exceeding 1-actor capacity during the 140ms wake window).
	gw.RunSpikeScenario(ctx, 15, 60, 50*time.Millisecond, 500*time.Millisecond, 0)

	st := gw.StateSnapshot()
	if st.Substrate.TotalGoodput == 0 {
		t.Fatalf("expected non-zero Substrate goodput")
	}
	if st.Substrate.TotalShed != 0 {
		t.Fatalf("expected 0 shed (429/503) on Substrate during 60 QPS scale-out with wake-on-HTTP, got %d shed (replicas: %+v)",
			st.Substrate.TotalShed, st.Substrate.Replicas)
	}
	for _, rep := range st.Substrate.Replicas {
		if rep.ServedCount == 0 {
			t.Fatalf("expected Substrate actor %s to serve traffic during 60 QPS spike, got 0 served", rep.Name)
		}
	}
}

func TestSubstrateScaleDownDrainsInFlightBeforeSuspend(t *testing.T) {
	// Model Envoy/atunnel behavior: if kubectl-ate suspend actor <name> is invoked while
	// an HTTP /search request is still in flight on that actor, the upstream connection
	// terminates mid-flight with 503 Service Unavailable (503 UC).
	var mu sync.Mutex
	suspendedCh := make(map[string]chan struct{})
	suspendedActors := make(map[string]bool)
	for _, name := range []string{"coldstart-1", "coldstart-2", "coldstart-3"} {
		suspendedCh[name] = make(chan struct{})
	}

	eng := NewWarmHeapEngineWithConfig(WorkerConfig{
		SieveLimit:       100,
		CorpusDocs:       100,
		MaxConcurrent:    4,
		MinQueryDuration: 180 * time.Millisecond,
	})
	eng.RunExpensiveInitialization(0)
	workerMux := newMux(eng)

	mockAtenet := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/_ctl/suspend" {
			actor := r.URL.Query().Get("actor")
			mu.Lock()
			if !suspendedActors[actor] {
				suspendedActors[actor] = true
				if ch, ok := suspendedCh[actor]; ok {
					close(ch)
				}
			}
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
			return
		}

		target := strings.TrimPrefix(r.Header.Get("ate-target-actor"), "anything-but-agents/")
		mu.Lock()
		if suspendedActors[target] {
			// Wake-on-HTTP re-opens a fresh channel if ever woken again
			suspendedActors[target] = false
			suspendedCh[target] = make(chan struct{})
		}
		abortCh := suspendedCh[target]
		mu.Unlock()

		done := make(chan *httptest.ResponseRecorder, 1)
		go func() {
			rec := httptest.NewRecorder()
			workerMux.ServeHTTP(rec, r)
			done <- rec
		}()

		select {
		case <-abortCh:
			// Actor was suspended mid-request -> Envoy 503 UC!
			http.Error(w, "upstream_reset_before_response_started{connection_termination}", http.StatusServiceUnavailable)
		case rec := <-done:
			for k, vals := range rec.Header() {
				for _, v := range vals {
					w.Header().Add(k, v)
				}
			}
			w.WriteHeader(rec.Code)
			_, _ = w.Write(rec.Body.Bytes())
		}
	}))
	defer mockAtenet.Close()

	// Fake kubectl-ate script that notifies mockAtenet when `suspend actor <name>` executes.
	fakeAtePath := filepath.Join(t.TempDir(), "fake-kubectl-ate.sh")
	script := fmt.Sprintf("#!/bin/sh\nif [ \"$1\" = \"suspend\" ] && [ \"$2\" = \"actor\" ]; then\n  curl -fsS -X POST %q >/dev/null 2>&1 || true\nfi\nexit 0\n",
		mockAtenet.URL+"/_ctl/suspend?actor=$3")
	// Note: expand $3 in shell, not Go
	script = "#!/bin/sh\nif [ \"$1\" = \"suspend\" ] && [ \"$2\" = \"actor\" ]; then\n  curl -fsS -X POST \"" + mockAtenet.URL + "/_ctl/suspend?actor=$3\" >/dev/null 2>&1 || true\nfi\nexit 0\n"
	if err := os.WriteFile(fakeAtePath, []byte(script), 0o755); err != nil {
		t.Fatalf("failed to write fake kubectl-ate: %v", err)
	}

	gw := NewGatewayServer(GatewayConfig{
		Mode:                   "gateway",
		RouterURL:              mockAtenet.URL,
		KubectlAteBin:          fakeAtePath,
		SubstrateWakeDelay:     5 * time.Millisecond,
		WorkerMinQueryDuration: 180 * time.Millisecond,
		WorkerMaxConcurrent:    4,
		CorpusDocs:             100,
		SieveLimit:             100,
	})
	gw.SetTargetQPS(60, "SPIKE")

	// Dispatch 3 concurrent /search queries (1 per actor, each taking ~160ms)
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			gw.sendOneSubstrateRequest("gvisor snapshot")
		}()
	}

	// Wait 40ms so all 3 queries are mid-execution, then scale down to 15 QPS (1 replica).
	time.Sleep(40 * time.Millisecond)
	gw.SetTargetQPS(15, "COOLDOWN")

	wg.Wait()

	st := gw.StateSnapshot()
	if st.Substrate.TotalShed != 0 {
		t.Fatalf("expected 0 503s when scaling down with in-flight queries, got %d shed", st.Substrate.TotalShed)
	}
	if st.Substrate.TotalGoodput != 3 {
		t.Fatalf("expected all 3 in-flight queries to drain cleanly with 200 OK, got %d", st.Substrate.TotalGoodput)
	}

	// Verify coldstart-2 and coldstart-3 actually suspended once drained.
	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		bothSuspended := suspendedActors["coldstart-2"] && suspendedActors["coldstart-3"]
		mu.Unlock()
		if bothSuspended {
			return
		}
		time.Sleep(15 * time.Millisecond)
	}
	t.Fatalf("expected coldstart-2 and coldstart-3 to be suspended after draining in-flight queries")
}

func TestSubstrateRequestParkingAndProactiveForeignEviction(t *testing.T) {
	// Simulate a 3-replica WorkerPool where foreign demo actor "slide-01" is RUNNING on slot 3.
	// Real atenet-router parks ResumeActor(coldstart-3) (up to its 5s --parked-request-budget)
	// instead of immediately returning 503. Gateway must:
	//   1. Proactively evict foreign actors on scale-out so parked requests unblock in ~100ms, AND
	//   2. Honor atenet-router's 5s park budget (not aborting parked requests at 4.0s).
	var slideEvicted atomic.Bool

	eng := NewWarmHeapEngineWithConfig(WorkerConfig{
		SieveLimit:       100,
		CorpusDocs:       100,
		MaxConcurrent:    4,
		MinQueryDuration: 20 * time.Millisecond,
	})
	eng.RunExpensiveInitialization(0)
	workerMux := newMux(eng)

	mockAtenet := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/_ctl/evict-foreign" {
			slideEvicted.Store(true)
			w.WriteHeader(http.StatusOK)
			return
		}
		target := strings.TrimPrefix(r.Header.Get("ate-target-actor"), "anything-but-agents/")
		if r.URL.Query().Get("q") == "slow-park-4100ms" {
			// Simulate a valid 4.15s park wait inside atenet-router's 5s --parked-request-budget.
			select {
			case <-time.After(4150 * time.Millisecond):
			case <-r.Context().Done():
				return
			}
			workerMux.ServeHTTP(w, r)
			return
		}
		if target == "coldstart-3" {
			// Park up to 5s waiting for foreign actor slide-01 to vacate the 3rd WorkerPool pod.
			parkDeadline := time.Now().Add(5 * time.Second)
			for !slideEvicted.Load() {
				if time.Now().After(parkDeadline) {
					http.Error(w, "actor coldstart-3 unavailable: no worker has room for the actor", http.StatusServiceUnavailable)
					return
				}
				select {
				case <-time.After(25 * time.Millisecond):
				case <-r.Context().Done():
					return
				}
			}
		}
		workerMux.ServeHTTP(w, r)
	}))
	defer mockAtenet.Close()

	fakeAtePath := filepath.Join(t.TempDir(), "fake-kubectl-ate.sh")
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = \"get\" ] && [ \"$2\" = \"actors\" ]; then\n" +
		"  echo \"ATESPACE NAME TEMPLATE STATE AGE WORKER_IPS\"\n" +
		"  echo \"anything-but-agents coldstart-1 coldstart ACTOR_STATE_RUNNING 5m 10.244.0.21\"\n" +
		"  echo \"anything-but-agents coldstart-2 coldstart ACTOR_STATE_RUNNING 1m 10.244.0.22\"\n" +
		"  echo \"anything-but-agents slide-01 slides ACTOR_STATE_RUNNING 2m 10.244.0.23\"\n" +
		"  exit 0\n" +
		"fi\n" +
		"if [ \"$1\" = \"suspend\" ] && [ \"$2\" = \"actor\" ] && [ \"$3\" = \"slide-01\" ]; then\n" +
		"  curl -fsS -X POST \"" + mockAtenet.URL + "/_ctl/evict-foreign\" >/dev/null 2>&1 || true\n" +
		"  exit 0\n" +
		"fi\n" +
		"exit 0\n"
	if err := os.WriteFile(fakeAtePath, []byte(script), 0o755); err != nil {
		t.Fatalf("failed to write fake kubectl-ate: %v", err)
	}

	gw := NewGatewayServer(GatewayConfig{
		Mode:                   "gateway",
		RouterURL:              mockAtenet.URL,
		KubectlAteBin:          fakeAtePath,
		SubstrateWakeDelay:     10 * time.Millisecond,
		WorkerMinQueryDuration: 20 * time.Millisecond,
		WorkerMaxConcurrent:    4,
		CorpusDocs:             100,
		SieveLimit:             100,
	})

	// 1. Scale out to 60 QPS (3 actors); foreign actor slide-01 must be evicted proactively
	//    so parked requests to coldstart-3 complete rapidly without waiting out the 5s park budget.
	scaleStart := time.Now()
	gw.SetTargetQPS(60, "SPIKE")
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			gw.sendOneSubstrateRequest("gvisor snapshot")
		}()
	}
	wg.Wait()
	if elapsed := time.Since(scaleStart); elapsed > 1500*time.Millisecond {
		t.Fatalf("expected proactive foreign worker eviction to unblock parked coldstart-3 in <1.5s, took %v", elapsed)
	}
	if !slideEvicted.Load() {
		t.Fatalf("expected foreign actor slide-01 to be suspended proactively on 60 QPS scale-out")
	}

	// 2. Verify a 4.15s parked request (within atenet-router's 5s --parked-request-budget)
	//    succeeds with 200 OK instead of timing out at 4.0s in GatewayServer.httpClient.
	gw.sendOneSubstrateRequest("slow-park-4100ms")

	st := gw.StateSnapshot()
	if st.Substrate.TotalShed != 0 {
		t.Fatalf("expected 0 shed requests under atenet request parking + foreign eviction, got %d", st.Substrate.TotalShed)
	}
	if st.Substrate.TotalGoodput != 4 {
		t.Fatalf("expected all 4 parked/routed requests to succeed with 200 OK, got %d", st.Substrate.TotalGoodput)
	}
}

func TestSubstrateColdGCSWake60QPSSpikeZeroShed(t *testing.T) {
	// Reproduce the exact live-GKE cold-GCS snapshot restore profile observed in Envoy logs:
	// - WorkerMaxConcurrent = 4, MinQueryDuration = 200ms (~180ms upstream query, ~64 QPS max pool ceiling)
	// - coldstart-1 starts RUNNING
	// - coldstart-2 pulls 76.6 MB snapshot cold from GCS in 420ms (+90ms initial page-fault penalty)
	// - coldstart-3 pulls 76.6 MB snapshot cold from GCS in 660ms (+90ms initial page-fault penalty)
	// Under a 60 QPS spike, LIFO broadcast wakeups + an 800ms slotQueueBudget previously starved
	// a handful of wake-window requests into 429s despite 3-actor capacity sufficing to drain the queue.
	type coldActor struct {
		mux        *http.ServeMux
		mu         sync.Mutex
		running    bool
		waking     chan struct{}
		wakeDelay  time.Duration
		firstWaveN atomic.Int64
	}
	actors := make(map[string]*coldActor, 3)
	wakeDelays := []time.Duration{0, 420 * time.Millisecond, 660 * time.Millisecond}
	for i := 1; i <= 3; i++ {
		eng := NewWarmHeapEngineWithConfig(WorkerConfig{
			SieveLimit:       100,
			CorpusDocs:       100,
			MaxConcurrent:    4,
			MinQueryDuration: 200 * time.Millisecond,
		})
		eng.RunExpensiveInitialization(0)
		name := fmt.Sprintf("anything-but-agents/coldstart-%d", i)
		actors[name] = &coldActor{
			mux:       newMux(eng),
			running:   i == 1,
			wakeDelay: wakeDelays[i-1],
		}
	}

	mockAtenet := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		target := r.Header.Get("ate-target-actor")
		actor := actors[target]
		if actor == nil {
			http.Error(w, "unknown actor", http.StatusNotFound)
			return
		}
		wasCold := false
		actor.mu.Lock()
		if !actor.running {
			wasCold = true
			if actor.waking == nil {
				ch := make(chan struct{})
				actor.waking = ch
				delay := actor.wakeDelay
				go func() {
					time.Sleep(delay)
					actor.mu.Lock()
					actor.running = true
					actor.waking = nil
					actor.mu.Unlock()
					close(ch)
				}()
			}
			waitCh := actor.waking
			actor.mu.Unlock()
			select {
			case <-waitCh:
			case <-r.Context().Done():
				return
			}
		} else {
			actor.mu.Unlock()
		}
		if wasCold && r.URL.Path == "/search" && actor.firstWaveN.Add(1) <= 4 {
			// Model initial post-restore gVisor memory page faults observed on GKE (~90ms extra on Wave 1).
			time.Sleep(90 * time.Millisecond)
		}
		actor.mux.ServeHTTP(w, r)
	}))
	defer mockAtenet.Close()

	k8sEng := NewWarmHeapEngineWithConfig(WorkerConfig{
		SieveLimit:       100,
		CorpusDocs:       100,
		MaxConcurrent:    4,
		MinQueryDuration: 200 * time.Millisecond,
	})
	k8sEng.RunExpensiveInitialization(0)
	k8sSrv := httptest.NewServer(newMux(k8sEng))
	defer k8sSrv.Close()

	gw := NewGatewayServer(GatewayConfig{
		Mode:                   "gateway",
		RouterURL:              mockAtenet.URL,
		K8sServiceURL:          k8sSrv.URL,
		KubectlAteBin:          "true",
		K8sColdBootDelay:       12 * time.Second,
		SubstrateWakeDelay:     420 * time.Millisecond,
		WorkerMinQueryDuration: 200 * time.Millisecond,
		WorkerMaxConcurrent:    4,
		CorpusDocs:             100,
		SieveLimit:             100,
		TickInterval:           100 * time.Millisecond,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	gw.StartBackgroundLoops(ctx)

	gw.RunSpikeScenario(ctx, 15, 60, 200*time.Millisecond, 2500*time.Millisecond, 200*time.Millisecond)

	st := gw.StateSnapshot()
	if st.Substrate.TotalGoodput < 120 {
		t.Fatalf("expected >=120 Substrate 200 OK queries across 2.5s @ 60 QPS, got %d", st.Substrate.TotalGoodput)
	}
	if st.Substrate.TotalShed != 0 {
		t.Fatalf("expected strictly 0 shed (429/503) on Substrate during cold-GCS 60 QPS spike, got %d shed (replicas: %+v)",
			st.Substrate.TotalShed, st.Substrate.Replicas)
	}
	if st.K8s.TotalShed == 0 {
		t.Fatalf("expected K8s to shed overflow traffic during 12s cold-boot, got 0 shed")
	}
}

func TestReclaimForeignWorkersSkipsDedicatedPools(t *testing.T) {
	tmpDir := t.TempDir()
	logPath := filepath.Join(tmpDir, "suspended.log")
	fakeAtePath := filepath.Join(tmpDir, "fake-kubectl-ate.sh")
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = \"get\" ] && [ \"$2\" = \"actors\" ]; then\n" +
		"  echo 'ATESPACE NAME TEMPLATE STATE WORKER_POD WORKER_IPS'\n" +
		"  echo 'anything-but-agents slide-15 anything-but-agents/slides ACTOR_STATE_RUNNING anything-but-agents/wp-slides-abc 10.120.0.10'\n" +
		"  echo 'anything-but-agents d2d-player anything-but-agents/dungeon2d ACTOR_STATE_RUNNING anything-but-agents/wp-dungeon-xyz 10.120.0.20'\n" +
		"  echo 'anything-but-agents clone-1 anything-but-agents/coldstart ACTOR_STATE_RUNNING anything-but-agents/wp-coldstart-123 10.120.0.30'\n" +
		"  echo 'anything-but-agents coldstart-1 anything-but-agents/coldstart ACTOR_STATE_RUNNING anything-but-agents/wp-coldstart-456 10.120.0.31'\n" +
		"  exit 0\n" +
		"fi\n" +
		"if [ \"$1\" = \"suspend\" ] && [ \"$2\" = \"actor\" ]; then\n" +
		"  echo \"$3\" >> \"" + logPath + "\"\n" +
		"  exit 0\n" +
		"fi\n" +
		"exit 0\n"
	if err := os.WriteFile(fakeAtePath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake kubectl-ate: %v", err)
	}

	gw := NewGatewayServer(GatewayConfig{
		Mode:          "gateway",
		Atespace:      "anything-but-agents",
		KubectlAteBin: fakeAtePath,
		CorpusDocs:    50,
		SieveLimit:    50,
	})
	gw.reclaimForeignWorkers(context.Background())

	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read suspended log: %v", err)
	}
	got := strings.Fields(string(raw))
	if len(got) != 1 || got[0] != "clone-1" {
		t.Fatalf("expected only wp-coldstart non-shard actor clone-1 to be suspended, got %v", got)
	}
}





