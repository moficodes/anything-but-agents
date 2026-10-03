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
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
)

func TestActorSessionThreeFloorProgressionAndVictory(t *testing.T) {
	session := NewActorSession()
	mux := NewActorMux(session)

	// 1. Check /readyz
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 from /readyz, got %d", rec.Code)
	}

	mutate := func(m MutateRequest, podIP string) ActorResponse {
		t.Helper()
		return session.Mutate(m, podIP)
	}

	// 2. Initialize hero on Worker Pod 1
	resp := mutate(MutateRequest{Action: "init", HeroName: "mofi", CharacterClass: "paladin"}, "10.120.0.11")
	if resp.Session.HeroName != "mofi" || resp.Session.Floor != 1 || resp.Session.HP != 100 {
		t.Fatalf("unexpected initial state: %+v", resp.Session)
	}

	// 3. Cannot descend Floor 1 stairs before collecting crypt-key
	resp = mutate(MutateRequest{Action: "stairs", Target: "f1:stairs", X: 13, Y: 9}, "10.120.0.11")
	if resp.Session.Floor != 1 {
		t.Fatalf("expected to remain on floor 1 without crypt-key, got floor %d", resp.Session.Floor)
	}

	// 4. Floor 1 milestones: pick up potion, defeat skeleton, pick up crypt-key, descend stairs
	resp = mutate(MutateRequest{Action: "pickup", Target: "healing-potion", EntityID: "f1:potion", X: 3, Y: 7}, "10.120.1.22")
	if !slices.Contains(resp.Session.Inventory, "healing-potion") {
		t.Fatalf("expected healing-potion in inventory, got %v", resp.Session.Inventory)
	}
	resp = mutate(MutateRequest{Action: "combat", Target: "Restless Skeleton", EntityID: "f1:skeleton", X: 7, Y: 5}, "10.120.1.22")
	if !resp.Session.Cleared["f1:skeleton"] {
		t.Fatalf("expected f1:skeleton to be cleared after combat, state: %+v", resp.Session)
	}
	resp = mutate(MutateRequest{Action: "pickup", Target: "crypt-key", EntityID: "f1:key", X: 11, Y: 3}, "10.120.1.23")
	if !slices.Contains(resp.Session.Inventory, "crypt-key") {
		t.Fatalf("expected crypt-key in inventory, got %v", resp.Session.Inventory)
	}
	resp = mutate(MutateRequest{Action: "stairs", Target: "f1:stairs", X: 13, Y: 9}, "10.120.1.23")
	if resp.Session.Floor != 2 {
		t.Fatalf("expected Floor 2 after unlocking stairs, got %d", resp.Session.Floor)
	}

	// 5. Floor 2 milestones: pick up dragon-slayer-sword, defeat shadow-guard, pick up fire-shield, quicksave, descend to Floor 3
	resp = mutate(MutateRequest{Action: "pickup", Target: "dragon-slayer-sword", EntityID: "f2:sword", X: 6, Y: 5}, "10.120.0.11")
	resp = mutate(MutateRequest{Action: "combat", Target: "Shadow Guard", EntityID: "f2:guard", X: 8, Y: 2}, "10.120.0.11")
	resp = mutate(MutateRequest{Action: "pickup", Target: "fire-shield", EntityID: "f2:shield", X: 10, Y: 2}, "10.120.1.22")
	resp = mutate(MutateRequest{Action: "quicksave", Target: "f2:shrine", X: 10, Y: 5}, "10.120.1.22")
	resp = mutate(MutateRequest{Action: "stairs", Target: "f2:stairs", X: 13, Y: 5}, "10.120.1.23")
	if resp.Session.Floor != 3 {
		t.Fatalf("expected Floor 3, got %d", resp.Session.Floor)
	}

	// 6. Floor 3 Boss Fight with Sword + Fire Shield -> 2 strikes defeat the 100 HP Heap Dragon
	resp = mutate(MutateRequest{Action: "combat", Target: "Ancient Heap Dragon", EntityID: "f3:dragon", X: 7, Y: 4}, "10.120.0.11")
	if !resp.Session.Alive || resp.Session.BossHP != 45 {
		t.Fatalf("expected dragon at 45 HP and player alive, got BossHP=%d Alive=%v", resp.Session.BossHP, resp.Session.Alive)
	}
	resp = mutate(MutateRequest{Action: "combat", Target: "Ancient Heap Dragon", EntityID: "f3:dragon", X: 7, Y: 4}, "10.120.1.22")
	if resp.Session.BossHP != 0 || !resp.Session.Cleared["f3:dragon"] {
		t.Fatalf("expected dragon defeated (0 HP), got BossHP=%d Cleared=%v", resp.Session.BossHP, resp.Session.Cleared)
	}

	// 7. Claim golden-chalice and escape through Ascension Portal
	resp = mutate(MutateRequest{Action: "pickup", Target: "golden-chalice", EntityID: "f3:chalice", X: 7, Y: 3}, "10.120.1.22")
	resp = mutate(MutateRequest{Action: "escape", Target: "f3:portal", X: 7, Y: 1}, "10.120.1.23")
	if !resp.Session.Victory {
		t.Fatalf("expected Victory=true after escaping with golden-chalice, got %+v", resp.Session)
	}
	if len(resp.Session.PodIPHistory) < 3 {
		t.Fatalf("expected multiple pod IP transitions recorded, got %v", resp.Session.PodIPHistory)
	}
}

func TestActorSessionDragonFatalTrapWithoutFireShield(t *testing.T) {
	session := NewActorSession()
	mux := NewActorMux(session)

	// Rush to Floor 3 with sword but WITHOUT fire-shield
	session.Mutate(MutateRequest{Action: "init", HeroName: "rush-hero"}, "10.120.0.11")
	session.Mutate(MutateRequest{Action: "pickup", Target: "crypt-key", EntityID: "f1:key"}, "10.120.0.11")
	session.Mutate(MutateRequest{Action: "stairs", Target: "f1:stairs"}, "10.120.0.11")
	session.Mutate(MutateRequest{Action: "pickup", Target: "dragon-slayer-sword", EntityID: "f2:sword"}, "10.120.0.11")
	session.Mutate(MutateRequest{Action: "stairs", Target: "f2:stairs"}, "10.120.0.11")

	body, _ := json.Marshal(MutateRequest{
		Action:   "combat",
		Target:   "Ancient Heap Dragon",
		EntityID: "f3:dragon",
		X:        7,
		Y:        4,
	})
	req := httptest.NewRequest(http.MethodPost, "/mutate", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 from /mutate, got %d", rec.Code)
	}
	var out ActorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}
	if out.Session.Alive || out.Session.HP != 0 {
		t.Fatalf("expected player to die to Heap Dragon without fire-shield, got Alive=%v HP=%d", out.Session.Alive, out.Session.HP)
	}
}

func TestSubstrateBackendLifecycleRevertAndFork(t *testing.T) {
	b := NewLocalSimulatorBackend("anything-but-agents", "dungeon2d")

	// 1. Start session -> creates actor, wakes on worker, mutates, and suspends to GCS
	rec, err := b.StartSession(t.Context(), "mofi", "paladin")
	if err != nil {
		t.Fatalf("StartSession failed: %v", err)
	}
	if rec.ActorName != "d2d-mofi" || rec.State != "ACTOR_STATE_SUSPENDED" || rec.SaveCount != 1 {
		t.Fatalf("unexpected initial session record: %+v", rec)
	}

	// 2. Floor 1 -> Floor 2 + Quicksave
	_, _ = b.ExecuteMilestone(t.Context(), "d2d-mofi", MutateRequest{Action: "pickup", Target: "crypt-key", EntityID: "f1:key"})
	_, _ = b.ExecuteMilestone(t.Context(), "d2d-mofi", MutateRequest{Action: "stairs", Target: "f1:stairs"})
	_, _ = b.ExecuteMilestone(t.Context(), "d2d-mofi", MutateRequest{Action: "pickup", Target: "dragon-slayer-sword", EntityID: "f2:sword"})
	savedRec, err := b.ExecuteMilestone(t.Context(), "d2d-mofi", MutateRequest{Action: "quicksave", Target: "f2:shrine"})
	if err != nil {
		t.Fatalf("quicksave failed: %v", err)
	}
	if savedRec.LastTag == "" {
		t.Fatalf("expected LastTag to be populated after quicksave, got %+v", savedRec)
	}

	// 3. Fork a parallel timeline from the quicksaved actor before entering Floor 3!
	forkedRec, err := b.ForkSession(t.Context(), "d2d-mofi", "d2d-mofi-branch")
	if err != nil {
		t.Fatalf("ForkSession failed: %v", err)
	}
	if forkedRec.ActorName != "d2d-mofi-branch" || forkedRec.Session.Floor != 2 || !slices.Contains(forkedRec.Session.Inventory, "dragon-slayer-sword") {
		t.Fatalf("unexpected forked actor state: %+v", forkedRec)
	}

	// 4. Descend to Floor 3 without fire-shield and attack Dragon -> DIE!
	// Note: For Revert to rewind to before the fatal attack, attacking the Dragon
	// leaves the dead actor in RUNNING (uncommitted) state so Revert rewinds to the
	// last committed GCS snapshot!
	_, _ = b.ExecuteMilestone(t.Context(), "d2d-mofi", MutateRequest{Action: "stairs", Target: "f2:stairs"})
	deadRec, err := b.ExecuteMilestone(t.Context(), "d2d-mofi", MutateRequest{Action: "combat", Target: "Ancient Heap Dragon", EntityID: "f3:dragon"})
	if err != nil {
		t.Fatalf("dragon combat failed: %v", err)
	}
	if deadRec.Session.Alive {
		t.Fatalf("expected hero to die without fire-shield")
	}

	// 5. SAVE-SCUM! Revert actor -> restores to last committed GCS snapshot (Alive == true!)
	revertedRec, err := b.RevertSession(t.Context(), "d2d-mofi")
	if err != nil {
		t.Fatalf("RevertSession failed: %v", err)
	}
	if !revertedRec.Session.Alive || revertedRec.Session.HP == 0 || revertedRec.RevertCount != 1 {
		t.Fatalf("expected RevertSession to bring hero back to life, got Alive=%v HP=%d Reverts=%d",
			revertedRec.Session.Alive, revertedRec.Session.HP, revertedRec.RevertCount)
	}
}

