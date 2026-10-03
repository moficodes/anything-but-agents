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
