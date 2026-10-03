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

// Command timewarp is an intentional ANTI-PATTERN demo ("Don't Try This at
// Home") for the "Running Anything But Agents on Substrate" talk.
//
// It demonstrates what goes wrong when you run a wall-clock-naive workload or
// a distributed consensus node (like a Raft leader with a 5s lease and a 100ms
// heartbeat loop) inside a checkpoint/restore runtime:
//
//  1. While suspended in GCS, the process executes 0 instructions, so its heap
//     still believes `raft_role == "LEADER (Term 1)"`.
//  2. When resumed 10+ seconds later, a single 100ms tick experiences a
//     multi-second wall-clock jump, expiring its leader lease retroactively
//     (Split-Brain!) and overflowing its naive rate-limiter token bucket.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"sync"
	"time"
)

type TimeJumpAnomaly struct {
	DetectedAt      time.Time `json:"detected_at"`
	ExpectedTickMS  int64     `json:"expected_tick_ms"`
	ObservedGapMS   int64     `json:"observed_gap_ms"`
	SplitBrainAlert string    `json:"split_brain_alert"`
	TokenOverflow   float64   `json:"naive_rate_limiter_tokens_minted"`
	PodIP           string    `json:"pod_ip"`
}

type NaiveClusterNode struct {
	mu               sync.Mutex
	ExpectedTick     time.Duration
	LeaseTTL         time.Duration
	AnomalyThreshold time.Duration
	LastHeartbeatAt  time.Time
	InMemoryRaftRole string
	TotalTicks       int64
	NaiveTokenBucket float64
	Anomalies        []TimeJumpAnomaly
}

func newNaiveClusterNode(now time.Time) *NaiveClusterNode {
	return &NaiveClusterNode{
		ExpectedTick:     100 * time.Millisecond,
		LeaseTTL:         3 * time.Second,
		AnomalyThreshold: 1500 * time.Millisecond,
		LastHeartbeatAt:  now,
		InMemoryRaftRole: "LEADER (Term 1) — stored in local RAM variable",
		NaiveTokenBucket: 10.0,
	}
}

func (n *NaiveClusterNode) ObserveClock(now time.Time, podIP string) {
	n.mu.Lock()
	defer n.mu.Unlock()

	if n.LastHeartbeatAt.IsZero() {
		return
	}

	n.TotalTicks++
	gap := now.Sub(n.LastHeartbeatAt)
	n.LastHeartbeatAt = now

	// Naive token bucket: tokens += elapsed_seconds * 100 req/sec (without clamping before side effects)
	minted := gap.Seconds() * 100.0
	n.NaiveTokenBucket += minted

	if gap >= n.AnomalyThreshold {
		splitBrain := "LEASE_STILL_VALID"
		if gap >= n.LeaseTTL {
			splitBrain = fmt.Sprintf(
				"CRITICAL SPLIT-BRAIN: Heap variable still says %q, but 3s leader lease expired %.1fs ago while frozen in snapshot storage! Cluster already elected Term 2 leader!",
				n.InMemoryRaftRole,
				(gap - n.LeaseTTL).Seconds(),
			)
		}
		n.Anomalies = append(n.Anomalies, TimeJumpAnomaly{
			DetectedAt:      now.UTC(),
			ExpectedTickMS:  n.ExpectedTick.Milliseconds(),
			ObservedGapMS:   gap.Milliseconds(),
			SplitBrainAlert: splitBrain,
			TokenOverflow:   minted,
			PodIP:           podIP,
		})
	}
}

func (n *NaiveClusterNode) Report(now time.Time, podIP string) map[string]any {
	n.mu.Lock()
	if n.LastHeartbeatAt.IsZero() {
		n.LastHeartbeatAt = now
	}
	n.mu.Unlock()

	n.ObserveClock(now, podIP)

	n.mu.Lock()
	defer n.mu.Unlock()

	health := "HEALTHY (wall clock advancing normally)"
	if len(n.Anomalies) > 0 {
		health = "PANIC: TIME-TRAVEL SHOCK & RAFT SPLIT-BRAIN DETECTED AFTER RESUME!"
	}

	return map[string]any{
		"demo":                  "Anti-Pattern: Wall-Clock Naive & Distributed Consensus Node on Substrate",
		"status":                health,
		"in_memory_raft_role":   n.InMemoryRaftRole,
		"leader_lease_ttl":      n.LeaseTTL.String(),
		"total_heartbeat_ticks": n.TotalTicks,
		"naive_token_bucket":    fmt.Sprintf("%.1f tokens", n.NaiveTokenBucket),
		"time_jump_anomalies":   n.Anomalies,
		"current_worker_pod_ip": podIP,
		"why_this_happened":     "Full snapshots freeze process memory, not the universe's wall clock. Never run Raft/Paxos voters or unguarded wall-clock delta loops as suspendable actors!",
	}
}

func newMux(node *NaiveClusterNode) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		resp := node.Report(time.Now(), currentPodIP())
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
	flag.Parse()

	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	// Pass zero time so the leader lease clock starts on the actor's first HTTP
	// request rather than during ActorTemplate Golden Snapshot creation.
	node := newNaiveClusterNode(time.Time{})

	go func() {
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for now := range ticker.C {
			node.ObserveClock(now, currentPodIP())
		}
	}()

	slog.Info("Starting Time-Warp Anti-Pattern server", slog.String("addr", *addr))
	if err := http.ListenAndServe(*addr, newMux(node)); err != nil {
		slog.Error("Server exited", slog.Any("err", err))
		os.Exit(1)
	}
}
