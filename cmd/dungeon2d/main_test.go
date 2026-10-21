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
	"context"
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

	// 1. Rush to Floor 2 and fight Shadow Guard WITHOUT dragon-slayer-sword -> fatal!
	session.Mutate(MutateRequest{Action: "init", HeroName: "rush-hero"}, "10.120.0.11")
	session.Mutate(MutateRequest{Action: "pickup", Target: "crypt-key", EntityID: "f1:key"}, "10.120.0.11")
	session.Mutate(MutateRequest{Action: "stairs", Target: "f1:stairs"}, "10.120.0.11")

	guardResp := session.Mutate(MutateRequest{Action: "combat", Target: "Shadow Guard", EntityID: "f2:guard"}, "10.120.0.11")
	if guardResp.Session.Alive || guardResp.Session.HP != 0 {
		t.Fatalf("expected player to die to Shadow Guard without dragon-slayer-sword, got Alive=%v HP=%d", guardResp.Session.Alive, guardResp.Session.HP)
	}
	// Restore Floor 2 checkpoint
	session.Mutate(MutateRequest{Action: "restore_checkpoint"}, "10.120.0.11")

	// 2. Rush to Floor 3 with fire-shield ONLY (no sword) -> Dragon is fatal without sword!
	session.Mutate(MutateRequest{Action: "pickup", Target: "fire-shield", EntityID: "f2:shield"}, "10.120.0.11")
	session.Mutate(MutateRequest{Action: "stairs", Target: "f2:stairs"}, "10.120.0.11")
	noSwordResp := session.Mutate(MutateRequest{Action: "combat", Target: "Ancient Heap Dragon", EntityID: "f3:dragon"}, "10.120.0.11")
	if noSwordResp.Session.Alive || noSwordResp.Session.HP != 0 {
		t.Fatalf("expected player to die to Heap Dragon without dragon-slayer-sword, got Alive=%v HP=%d", noSwordResp.Session.Alive, noSwordResp.Session.HP)
	}

	// 3. Restore Floor 3 checkpoint and take stairs_up back to Floor 2 to prevent soft-lock!
	session.Mutate(MutateRequest{Action: "restore_checkpoint"}, "10.120.0.11")
	upResp := session.Mutate(MutateRequest{Action: "stairs_up", Target: "f3:stairs_up"}, "10.120.0.11")
	if upResp.Session.Floor != 2 {
		t.Fatalf("expected stairs_up to return player from Floor 3 to Floor 2, got Floor=%d", upResp.Session.Floor)
	}

	// 4. Also verify Dragon is fatal with sword but WITHOUT fire-shield
	session2 := NewActorSession()
	mux = NewActorMux(session2)
	session2.Mutate(MutateRequest{Action: "init", HeroName: "rush-hero-2"}, "10.120.0.11")
	session2.Mutate(MutateRequest{Action: "pickup", Target: "crypt-key", EntityID: "f1:key"}, "10.120.0.11")
	session2.Mutate(MutateRequest{Action: "stairs", Target: "f1:stairs"}, "10.120.0.11")
	session2.Mutate(MutateRequest{Action: "pickup", Target: "dragon-slayer-sword", EntityID: "f2:sword"}, "10.120.0.11")
	session2.Mutate(MutateRequest{Action: "stairs", Target: "f2:stairs"}, "10.120.0.11")

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
	rec, err := b.StartSession(context.Background(), "mofi", "paladin")
	if err != nil {
		t.Fatalf("StartSession failed: %v", err)
	}
	if rec.ActorName != "d2d-mofi" || rec.State != "ACTOR_STATE_SUSPENDED" || rec.SaveCount != 1 {
		t.Fatalf("unexpected initial session record: %+v", rec)
	}

	// 2. Floor 1 -> Floor 2 + Quicksave
	_, _ = b.ExecuteMilestone(context.Background(), "d2d-mofi", MutateRequest{Action: "pickup", Target: "crypt-key", EntityID: "f1:key"})
	_, _ = b.ExecuteMilestone(context.Background(), "d2d-mofi", MutateRequest{Action: "stairs", Target: "f1:stairs"})
	_, _ = b.ExecuteMilestone(context.Background(), "d2d-mofi", MutateRequest{Action: "pickup", Target: "dragon-slayer-sword", EntityID: "f2:sword"})
	savedRec, err := b.ExecuteMilestone(context.Background(), "d2d-mofi", MutateRequest{Action: "quicksave", Target: "f2:shrine"})
	if err != nil {
		t.Fatalf("quicksave failed: %v", err)
	}
	if savedRec.LastTag == "" {
		t.Fatalf("expected LastTag to be populated after quicksave, got %+v", savedRec)
	}

	// 3. Fork a parallel timeline from the quicksaved actor before entering Floor 3!
	forkedRec, err := b.ForkSession(context.Background(), "d2d-mofi", "d2d-mofi-branch")
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
	_, _ = b.ExecuteMilestone(context.Background(), "d2d-mofi", MutateRequest{Action: "stairs", Target: "f2:stairs"})
	deadRec, err := b.ExecuteMilestone(context.Background(), "d2d-mofi", MutateRequest{Action: "combat", Target: "Ancient Heap Dragon", EntityID: "f3:dragon"})
	if err != nil {
		t.Fatalf("dragon combat failed: %v", err)
	}
	if deadRec.Session.Alive {
		t.Fatalf("expected hero to die without fire-shield")
	}

	// 5. SAVE-SCUM! Revert actor -> restores to last committed GCS snapshot (Alive == true!)
	revertedRec, err := b.RevertSession(context.Background(), "d2d-mofi")
	if err != nil {
		t.Fatalf("RevertSession failed: %v", err)
	}
	if !revertedRec.Session.Alive || revertedRec.Session.HP == 0 || revertedRec.RevertCount != 1 {
		t.Fatalf("expected RevertSession to bring hero back to life, got Alive=%v HP=%d Reverts=%d",
			revertedRec.Session.Alive, revertedRec.Session.HP, revertedRec.RevertCount)
	}

	// 6. Test post-milestone checkpoint Revert:
	// When a player picks up the Gold Chest at (5, 2), the save happens AFTER the milestone
	// with the new state saved. Moving around and clicking Revert restores (5, 2) right after
	// picking up the chest (with the chest still looted!).
	_, err = b.StartSession(context.Background(), "stepper", "ranger")
	if err != nil {
		t.Fatalf("StartSession stepper failed: %v", err)
	}
	afterChest, err := b.ExecuteMilestone(context.Background(), "d2d-stepper", MutateRequest{
		Action: "pickup", Target: "gold-chest", EntityID: "f1:chest", X: 5, Y: 2,
	})
	if err != nil {
		t.Fatalf("pickup chest failed: %v", err)
	}
	if !afterChest.Session.Cleared["f1:chest"] || afterChest.Session.X != 5 || afterChest.Session.Y != 2 {
		t.Fatalf("expected chest cleared at (5,2), got %+v", afterChest.Session)
	}

	// Revert after picking up chest -> restores state right AFTER picking up chest at (5,2)
	revChest, err := b.RevertSession(context.Background(), "d2d-stepper")
	if err != nil {
		t.Fatalf("RevertSession after chest failed: %v", err)
	}
	if !revChest.Session.Cleared["f1:chest"] || revChest.Session.Gold != 100 || revChest.Session.X != 5 || revChest.Session.Y != 2 {
		t.Fatalf("expected revert after chest to keep chest looted at (5,2), got Cleared=%v Gold=%d Pos=(%d,%d)",
			revChest.Session.Cleared, revChest.Session.Gold, revChest.Session.X, revChest.Session.Y)
	}

	// Pick up Healing Potion at (3, 7), then revert -> restores state right AFTER picking up potion at (3,7)
	_, err = b.ExecuteMilestone(context.Background(), "d2d-stepper", MutateRequest{
		Action: "pickup", Target: "healing-potion", EntityID: "f1:potion", X: 3, Y: 7,
	})
	if err != nil {
		t.Fatalf("pickup potion failed: %v", err)
	}
	revPotion, err := b.RevertSession(context.Background(), "d2d-stepper")
	if err != nil {
		t.Fatalf("RevertSession after potion failed: %v", err)
	}
	if !revPotion.Session.Cleared["f1:chest"] || !revPotion.Session.Cleared["f1:potion"] || revPotion.Session.X != 3 || revPotion.Session.Y != 7 {
		t.Fatalf("expected revert after potion to keep both chest and potion at (3,7), got Cleared=%v Pos=(%d,%d)",
			revPotion.Session.Cleared, revPotion.Session.X, revPotion.Session.Y)
	}

	// 7. Test reverting to a specific earlier snapshot version (v2 = right after gold-chest, before potion):
	revV2, err := b.RevertSession(context.Background(), "d2d-stepper", 2)
	if err != nil {
		t.Fatalf("RevertSession to v2 failed: %v", err)
	}
	if !revV2.Session.Cleared["f1:chest"] || revV2.Session.Cleared["f1:potion"] || revV2.Session.X != 5 || revV2.Session.Y != 2 {
		t.Fatalf("expected revert to v2 to keep chest looted at (5,2) and un-clear potion, got Cleared=%v Pos=(%d,%d)",
			revV2.Session.Cleared, revV2.Session.X, revV2.Session.Y)
	}
}

func TestGatewayAndDashboardIntegration(t *testing.T) {
	b := NewLocalSimulatorBackend("anything-but-agents", "dungeon2d")
	gw := NewGatewayServer(b, "http://localhost:8090", "test-presenter-secret")
	srv := httptest.NewServer(gw.Mux())
	defer srv.Close()

	// 0. Verify /dashboard and /api/dashboard/state require presenter auth (401 Unauthorized without key)
	unauthResp, err := http.Get(srv.URL + "/dashboard")
	if err != nil {
		t.Fatalf("GET /dashboard failed: %v", err)
	}
	unauthResp.Body.Close()
	if unauthResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 from unauthenticated GET /dashboard, got %d", unauthResp.StatusCode)
	}

	unauthAPIResp, err := http.Get(srv.URL + "/api/dashboard/state")
	if err != nil {
		t.Fatalf("GET /api/dashboard/state failed: %v", err)
	}
	unauthAPIResp.Body.Close()
	if unauthAPIResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 from unauthenticated GET /api/dashboard/state, got %d", unauthAPIResp.StatusCode)
	}

	// Verify ?key= sets HttpOnly d2d_presenter cookie and redirects (303 SeeOther) to /dashboard
	noRedirClient := &http.Client{
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse },
	}
	keyResp, err := noRedirClient.Get(srv.URL + "/dashboard?key=test-presenter-secret")
	if err != nil {
		t.Fatalf("GET /dashboard?key= failed: %v", err)
	}
	keyResp.Body.Close()
	if keyResp.StatusCode != http.StatusSeeOther || keyResp.Header.Get("Location") != "/dashboard" {
		t.Fatalf("expected 303 redirect to /dashboard, got status=%d loc=%q", keyResp.StatusCode, keyResp.Header.Get("Location"))
	}
	var presenterCookie *http.Cookie
	for _, c := range keyResp.Cookies() {
		if c.Name == PresenterCookieName {
			presenterCookie = c
		}
	}
	if presenterCookie == nil || !presenterCookie.HttpOnly {
		t.Fatalf("expected HttpOnly %s cookie, got %+v", PresenterCookieName, keyResp.Cookies())
	}

	// 1. Verify /play and authenticated /dashboard serve embedded HTML with inline Lucide SVGs and zero emojis
	for _, path := range []string{"/play", "/dashboard"} {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+path, nil)
		if path == "/dashboard" {
			req.AddCookie(presenterCookie)
		}
		resp, err := http.DefaultClient.Do(req)
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
		if !bytes.Contains(raw.Bytes(), []byte("stone-slab")) || !bytes.Contains(raw.Bytes(), []byte("parchment-scroll")) {
			t.Fatalf("expected 2004 Stone & Parchment Scroll classes (stone-slab, parchment-scroll) in %s", path)
		}
		if path == "/play" {
			if !bytes.Contains(raw.Bytes(), []byte("helpModal")) || !bytes.Contains(raw.Bytes(), []byte("preview-paladin")) {
				t.Fatalf("expected How-to-Play modal and pixel-art class previews in /play")
			}
			if !bytes.Contains(raw.Bytes(), []byte("drawStarburstSplat")) {
				t.Fatalf("expected 2004 starburst combat hit-splat renderer in /play")
			}
			if bytes.Contains(raw.Bytes(), []byte("/dashboard")) {
				t.Fatalf("expected /play to have no link back to /dashboard")
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
	postJSON := func(path string, payload any, withPresenter bool) *http.Response {
		t.Helper()
		b, _ := json.Marshal(payload)
		req, _ := http.NewRequest(http.MethodPost, srv.URL+path, bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		if withPresenter {
			req.Header.Set(PresenterHeaderName, gw.PresenterKey())
		}
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("POST %s failed: %v", path, err)
		}
		return r
	}

	r := postJSON("/api/session/start", map[string]string{"hero_name": "aria", "character_class": "ranger"}, false)
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
	}, false)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 from /api/session/action, got %d", r.StatusCode)
	}
	r.Body.Close()

	// 5. Run demo bot step via POST /api/session/bot (presenter-protected)
	r = postJSON("/api/session/bot", map[string]any{
		"hero_name": "bot-knight",
		"mode":      "full-demo",
	}, true)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 from /api/session/bot, got %d", r.StatusCode)
	}
	r.Body.Close()

	// 6. Verify /api/dashboard/state includes both sessions and stats (presenter-protected)
	stateReq, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/dashboard/state", nil)
	stateReq.Header.Set(PresenterHeaderName, gw.PresenterKey())
	stateResp, err := http.DefaultClient.Do(stateReq)
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



