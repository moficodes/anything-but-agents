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
	"net/http"
	"net/http/httptest"
	"testing"
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

	engine.RunExpensiveInitialization(0)

	// After initialization, /readyz returns 200 OK and primes up to 100 (25 primes, largest 97) are in RAM.
	rec2 := httptest.NewRecorder()
	mux.ServeHTTP(rec2, req)
	if rec2.Code != http.StatusOK {
		t.Fatalf("expected 200 after init finishes, got %d", rec2.Code)
	}

	resp := engine.HandleQuery("fn_main", "10.0.0.5")
	if resp["precomputed_prime_count"] != 25 || resp["precomputed_largest_prime"] != 97 {
		t.Fatalf("unexpected prime sieve results: %v", resp)
	}
	if resp["golden_heap_fingerprint"] == "" {
		t.Fatalf("expected non-empty golden_heap_fingerprint")
	}
}
