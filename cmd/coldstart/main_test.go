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

	// Verify dashboard HTML serves cleanly
	dashReq := httptest.NewRequest(http.MethodGet, "/", nil)
	dashRec := httptest.NewRecorder()
	gw.Mux().ServeHTTP(dashRec, dashReq)
	if dashRec.Code != http.StatusOK || !strings.Contains(dashRec.Body.String(), "Production HPA + Cold-Start QPS Benchmark") {
		t.Fatalf("expected dashboard HTML, got status=%d", dashRec.Code)
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

