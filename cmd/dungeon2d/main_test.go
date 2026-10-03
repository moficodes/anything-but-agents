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

	// 6. Test live-session 1-action-at-a-time Revert:
	// First fight Shadow Guard (takes -25 HP counter damage), then click Revert Actor -> rewinds HP back to 100!
	damagedRec, err := b.ExecuteMilestone(t.Context(), "d2d-mofi", MutateRequest{Action: "combat", Target: "Shadow Guard", EntityID: "f2:guard"})
	if err != nil {
		t.Fatalf("guard combat failed: %v", err)
	}
	if damagedRec.Session.LastDamageTaken != 25 || damagedRec.Session.HP >= 100 {
		t.Fatalf("expected enemy counter-attack damage (25), got LastDamageTaken=%d HP=%d", damagedRec.Session.LastDamageTaken, damagedRec.Session.HP)
	}
	liveRevertedRec, err := b.RevertSession(t.Context(), "d2d-mofi")
	if err != nil {
		t.Fatalf("live RevertSession failed: %v", err)
	}
	if liveRevertedRec.Session.HP != 100 || liveRevertedRec.RevertCount != 2 {
		t.Fatalf("expected live RevertSession to restore checkpoint HP=100, got HP=%d Reverts=%d", liveRevertedRec.Session.HP, liveRevertedRec.RevertCount)
	}

	// 7. Test sequential item pickup revert (pick up chest -> pick up potion -> revert once keeps chest and undoes only potion):
	_, err = b.StartSession(t.Context(), "stepper", "ranger")
	if err != nil {
		t.Fatalf("StartSession stepper failed: %v", err)
	}
	_, err = b.ExecuteMilestone(t.Context(), "d2d-stepper", MutateRequest{
		Action: "pickup", Target: "gold-chest", EntityID: "f1:chest", X: 5, Y: 2, PrevX: 4, PrevY: 2,
	})
	if err != nil {
		t.Fatalf("pickup chest failed: %v", err)
	}
	afterPotion, err := b.ExecuteMilestone(t.Context(), "d2d-stepper", MutateRequest{
		Action: "pickup", Target: "healing-potion", EntityID: "f1:potion", X: 3, Y: 7, PrevX: 3, PrevY: 6,
	})
	if err != nil {
		t.Fatalf("pickup potion failed: %v", err)
	}
	if !afterPotion.Session.Cleared["f1:chest"] || !afterPotion.Session.Cleared["f1:potion"] {
		t.Fatalf("expected both chest and potion cleared, got %v", afterPotion.Session.Cleared)
	}

	// Revert #1: undoes ONLY the potion pickup; chest stays looted and player steps back to (3, 6)
	rev1, err := b.RevertSession(t.Context(), "d2d-stepper")
	if err != nil {
		t.Fatalf("RevertSession #1 failed: %v", err)
	}
	if !rev1.Session.Cleared["f1:chest"] || rev1.Session.Cleared["f1:potion"] {
		t.Fatalf("expected chest to stay cleared and potion to be restored after 1st revert, got Cleared=%v", rev1.Session.Cleared)
	}
	if rev1.Session.X != 3 || rev1.Session.Y != 6 {
		t.Fatalf("expected player to step back to (3,6) before potion pickup, got (%d,%d)", rev1.Session.X, rev1.Session.Y)
	}

	// Revert #2: undoes the chest pickup; player steps back to (4, 2)
	rev2, err := b.RevertSession(t.Context(), "d2d-stepper")
	if err != nil {
		t.Fatalf("RevertSession #2 failed: %v", err)
	}
	if rev2.Session.Cleared["f1:chest"] {
		t.Fatalf("expected chest to be restored after 2nd revert, got Cleared=%v", rev2.Session.Cleared)
	}
	if rev2.Session.X != 4 || rev2.Session.Y != 2 {
		t.Fatalf("expected player to step back to (4,2) before chest pickup, got (%d,%d)", rev2.Session.X, rev2.Session.Y)
	}
}

func TestGatewayAndDashboardIntegration(t *testing.T) {
	b := NewLocalSimulatorBackend("anything-but-agents", "dungeon2d")
	gw := NewGatewayServer(b, "http://localhost:8090")
	srv := httptest.NewServer(gw.Mux())
	defer srv.Close()

	// 1. Verify /play and /dashboard serve embedded HTML with inline Lucide SVGs and zero emojis
	for _, path := range []string{"/play", "/dashboard"} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatalf("GET %s failed: %v", path, err)
		}
		raw := new(bytes.Buffer)
		_, _ = raw.ReadFrom(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s returned %d", path, resp.StatusCode)
		}
		html := raw.String()
		if !bytes.Contains(raw.Bytes(), []byte("<svg")) {
			t.Fatalf("expected inline Lucide <svg icons in %s", path)
		}
		for _, r := range html {
			if r >= 0x1F300 && r <= 0x1FAFF {
				t.Fatalf("found prohibited emoji character %U in %s", r, path)
			}
		}
		if path == "/play" {
			if !bytes.Contains(raw.Bytes(), []byte("helpModal")) || !bytes.Contains(raw.Bytes(), []byte("preview-paladin")) {
				t.Fatalf("expected How-to-Play modal and pixel-art class previews in /play")
			}
		}
	}

	// 2. Verify /api/qr.svg returns valid SVG QR code
	qrResp, err := http.Get(srv.URL + "/api/qr.svg?url=http://localhost:8090/play")
	if err != nil {
		t.Fatalf("GET /api/qr.svg failed: %v", err)
	}
	qrBody := new(bytes.Buffer)
	_, _ = qrBody.ReadFrom(qrResp.Body)
	qrResp.Body.Close()
	if qrResp.StatusCode != http.StatusOK || !bytes.Contains(qrBody.Bytes(), []byte("<svg")) {
		t.Fatalf("unexpected /api/qr.svg response: status=%d", qrResp.StatusCode)
	}

	// 3. Start session via POST /api/session/start
	postJSON := func(path string, payload any) *http.Response {
		t.Helper()
		b, _ := json.Marshal(payload)
		r, err := http.Post(srv.URL+path, "application/json", bytes.NewReader(b))
		if err != nil {
			t.Fatalf("POST %s failed: %v", path, err)
		}
		return r
	}

	r := postJSON("/api/session/start", map[string]string{"hero_name": "aria", "character_class": "ranger"})
	if r.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 from /api/session/start, got %d", r.StatusCode)
	}
	r.Body.Close()

	// 4. Execute milestone via POST /api/session/action
	r = postJSON("/api/session/action", map[string]any{
		"actor_name": "d2d-aria",
		"action":     "pickup",
		"target":     "crypt-key",
		"entity_id":  "f1:key",
		"x":          11,
		"y":          3,
	})
	if r.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 from /api/session/action, got %d", r.StatusCode)
	}
	r.Body.Close()

	// 5. Run demo bot step via POST /api/session/bot
	r = postJSON("/api/session/bot", map[string]any{
		"hero_name": "bot-knight",
		"mode":      "full-demo",
	})
	if r.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 from /api/session/bot, got %d", r.StatusCode)
	}
	r.Body.Close()

	// 6. Verify /api/dashboard/state includes both sessions and stats
	stateResp, err := http.Get(srv.URL + "/api/dashboard/state")
	if err != nil {
		t.Fatalf("GET /api/dashboard/state failed: %v", err)
	}
	var dash DashboardSnapshot
	if err := json.NewDecoder(stateResp.Body).Decode(&dash); err != nil {
		t.Fatalf("failed to decode dashboard state: %v", err)
	}
	stateResp.Body.Close()
	if dash.Stats.TotalSessions < 2 || dash.Stats.Victories < 1 || dash.Stats.TotalReverts < 1 {
		t.Fatalf("expected bot-knight victory & revert in dashboard stats, got %+v", dash.Stats)
	}
}


