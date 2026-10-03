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

