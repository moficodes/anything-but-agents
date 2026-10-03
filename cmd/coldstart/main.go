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

// Command coldstart simulates a "Cold-Start Monster" (like a legacy JVM
// monolith, a Rust LSP indexing a workspace, or a scientific model loader)
// that takes 12+ seconds to initialize its in-memory data structures before
// /readyz reports 200 OK.
//
// Because Agent Substrate waits for wakeupProbe (/readyz) to succeed before
// capturing the ActorTemplate's Golden Snapshot, the agonizing cold start runs
// ONCE at template creation time. Every Actor instantiated from the template
// restores directly into the post-init heap in ~100ms, sharing the same
// golden_heap_fingerprint while maintaining isolated per-actor mutations.
package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

type WarmHeapEngine struct {
	ready                 atomic.Bool
	ColdBootStartedAt     time.Time
	ColdBootCompletedAt   time.Time
	ColdBootDuration      time.Duration
	GoldenHeapFingerprint string
	PrimesUpToN           int
	PrimeCount            int
	LargestPrime          int

	mu            sync.Mutex
	QueryCount    int      `json:"query_count"`
	CustomOverlay []string `json:"custom_overlay"`
}

func newWarmHeapEngine(sieveLimit int) *WarmHeapEngine {
	return &WarmHeapEngine{
		ColdBootStartedAt: time.Now().UTC(),
		PrimesUpToN:       sieveLimit,
	}
}

func (e *WarmHeapEngine) RunExpensiveInitialization(delay time.Duration) {
	steps := 6
	stepSleep := delay / time.Duration(steps)
	for i := 1; i <= steps; i++ {
		if stepSleep > 0 {
			time.Sleep(stepSleep)
		}
		slog.Info("Cold-start monster warming heap...", slog.Int("step", i), slog.Int("total_steps", steps))
	}

	// Compute primes up to PrimesUpToN in RAM so there is real computed state in the heap.
	count, largest := sieveOfEratosthenes(e.PrimesUpToN)
	e.PrimeCount = count
	e.LargestPrime = largest

	var raw [8]byte
	_, _ = rand.Read(raw[:])
	e.GoldenHeapFingerprint = hex.EncodeToString(raw[:])
	e.ColdBootCompletedAt = time.Now().UTC()
	e.ColdBootDuration = e.ColdBootCompletedAt.Sub(e.ColdBootStartedAt)
	e.ready.Store(true)

	slog.Info("Golden heap initialized; /readyz now returning 200 OK",
		slog.String("golden_heap_fingerprint", e.GoldenHeapFingerprint),
		slog.Duration("cold_boot_duration", e.ColdBootDuration),
		slog.Int("prime_count", e.PrimeCount),
	)
}

func sieveOfEratosthenes(n int) (count int, largest int) {
	if n < 2 {
		return 0, 0
	}
	composite := make([]bool, n+1)
	for p := 2; p*p <= n; p++ {
		if !composite[p] {
			for i := p * p; i <= n; i += p {
				composite[i] = true
			}
		}
	}
	for p := 2; p <= n; p++ {
		if !composite[p] {
			count++
			largest = p
		}
	}
	return count, largest
}

func (e *WarmHeapEngine) HandleQuery(overlaySymbol string, podIP string) map[string]any {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.QueryCount++
	if overlaySymbol != "" {
		e.CustomOverlay = append(e.CustomOverlay, overlaySymbol)
	}

	return map[string]any{
		"status":                    "INSTANT_CLONE_FROM_GOLDEN_SNAPSHOT",
		"golden_heap_fingerprint":   e.GoldenHeapFingerprint,
		"cold_boot_duration_ms":     e.ColdBootDuration.Milliseconds(),
		"cold_boot_completed_at":    e.ColdBootCompletedAt.Format(time.RFC3339Nano),
		"served_at":                 time.Now().UTC().Format(time.RFC3339Nano),
		"time_since_golden_boot_s":  fmt.Sprintf("%.1fs", time.Since(e.ColdBootCompletedAt).Seconds()),
		"precomputed_primes_limit":  e.PrimesUpToN,
		"precomputed_prime_count":   e.PrimeCount,
		"precomputed_largest_prime": e.LargestPrime,
		"actor_query_count":         e.QueryCount,
		"actor_custom_overlay":      e.CustomOverlay,
		"current_pod_ip":            podIP,
	}
}

func newMux(engine *WarmHeapEngine) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if !engine.ready.Load() {
			http.Error(w, "still running expensive cold-start initialization...\n", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if !engine.ready.Load() {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		symbol := r.URL.Query().Get("add_symbol")
		resp := engine.HandleQuery(symbol, currentPodIP())
		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(resp)
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

func main() {
	addr := flag.String("addr", ":80", "HTTP listen address")
	initDelay := flag.Duration("init-delay", 12*time.Second, "Simulated heavy cold-start initialization time before /readyz reports 200 OK")
	sieveLimit := flag.Int("sieve-limit", 1000000, "Upper bound for prime sieve precomputed in Golden Snapshot RAM")
	flag.Parse()

	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	engine := newWarmHeapEngine(*sieveLimit)

	go func() {
		slog.Info("Starting HTTP listener (returning 503 on /readyz until heavy init finishes)", slog.String("addr", *addr))
		if err := http.ListenAndServe(*addr, newMux(engine)); err != nil {
			slog.Error("Server exited", slog.Any("err", err))
			os.Exit(1)
		}
	}()

	engine.RunExpensiveInitialization(*initDelay)
	select {}
}
