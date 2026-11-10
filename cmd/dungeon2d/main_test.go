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
	"os"
	"path/filepath"
	"slices"
	"strings"
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

	// 2. Initialize hero on Worker Pod 1 (Paladin starts with 140 HP)
	resp := mutate(MutateRequest{Action: "init", HeroName: "mofi", CharacterClass: "paladin"}, "10.120.0.11")
	if resp.Session.HeroName != "mofi" || resp.Session.Floor != 1 || resp.Session.HP != 140 || resp.Session.MaxHP != 140 {
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

	// 7. Verify /api/dashboard/reset requires presenter auth + POST, and resets actors & stats back to 0
	unauthReset := postJSON("/api/dashboard/reset", map[string]any{}, false)
	unauthReset.Body.Close()
	if unauthReset.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 from unauthenticated POST /api/dashboard/reset, got %d", unauthReset.StatusCode)
	}

	getResetReq, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/dashboard/reset", nil)
	getResetReq.Header.Set(PresenterHeaderName, gw.PresenterKey())
	getResetResp, err := http.DefaultClient.Do(getResetReq)
	if err != nil {
		t.Fatalf("GET /api/dashboard/reset failed: %v", err)
	}
	getResetResp.Body.Close()
	if getResetResp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405 from GET /api/dashboard/reset, got %d", getResetResp.StatusCode)
	}

	okReset := postJSON("/api/dashboard/reset", map[string]any{}, true)
	var resetRes struct {
		Deleted   int `json:"deleted"`
		Remaining int `json:"remaining"`
	}
	if err := json.NewDecoder(okReset.Body).Decode(&resetRes); err != nil {
		t.Fatalf("decode reset response: %v", err)
	}
	okReset.Body.Close()
	if okReset.StatusCode != http.StatusOK || resetRes.Deleted < 2 || resetRes.Remaining != 0 {
		t.Fatalf("expected >=2 deleted and 0 remaining on reset, got status=%d res=%+v", okReset.StatusCode, resetRes)
	}

	afterReq, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/dashboard/state", nil)
	afterReq.Header.Set(PresenterHeaderName, gw.PresenterKey())
	afterResp, err := http.DefaultClient.Do(afterReq)
	if err != nil {
		t.Fatalf("GET /api/dashboard/state after reset failed: %v", err)
	}
	var afterDash DashboardSnapshot
	if err := json.NewDecoder(afterResp.Body).Decode(&afterDash); err != nil {
		t.Fatalf("decode dashboard state after reset: %v", err)
	}
	afterResp.Body.Close()
	if afterDash.Stats.TotalSessions != 0 || afterDash.Stats.TotalSaves != 0 || afterDash.Stats.TotalReverts != 0 || afterDash.Stats.Victories != 0 {
		t.Fatalf("expected all session & event counters reset to 0, got %+v", afterDash.Stats)
	}
}

func TestWorldStateSaveRevertForkAndAsymmetricClasses(t *testing.T) {
	b := NewLocalSimulatorBackend("anything-but-agents", "dungeon2d")
	gw := NewGatewayServer(b, "http://localhost:8090", "test-presenter-secret")
	srv := httptest.NewServer(gw.Mux())
	defer srv.Close()

	postJSON := func(path string, payload any) SessionRecord {
		t.Helper()
		raw, _ := json.Marshal(payload)
		resp, err := http.Post(srv.URL+path, "application/json", bytes.NewReader(raw))
		if err != nil {
			t.Fatalf("POST %s failed: %v", path, err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("POST %s returned %d", path, resp.StatusCode)
		}
		var out SessionRecord
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatalf("decode SessionRecord from %s failed: %v", path, err)
		}
		return out
	}

	// 1. Verify asymmetric Hero Class base stats across Paladin, Ranger, and Wizard
	pal := postJSON("/api/session/start", map[string]string{"hero_name": "pal-hero", "character_class": "paladin"})
	rng := postJSON("/api/session/start", map[string]string{"hero_name": "rng-hero", "character_class": "ranger"})
	wiz := postJSON("/api/session/start", map[string]string{"hero_name": "wiz-hero", "character_class": "wizard"})

	if pal.Session.MaxHP != 140 || pal.Session.World == nil || pal.Session.World.Hero.Armor != 25 || pal.Session.MaxFloors != 8 {
		t.Fatalf("expected Paladin 140 HP, 25 armor, 8 floors; got %+v", pal.Session)
	}
	if rng.Session.MaxHP != 100 || rng.Session.World == nil || rng.Session.World.Hero.AttackRange < 4.0 {
		t.Fatalf("expected Ranger 100 HP and ranged bow attack; got %+v", rng.Session)
	}
	if wiz.Session.MaxHP != 75 || wiz.Session.World == nil || wiz.Session.World.Hero.AttackDamage < 35 {
		t.Fatalf("expected Wizard 75 HP glass cannon and high spell damage; got %+v", wiz.Session)
	}

	// 2. Client plays locally to Floor 4, wounds a Shadow Knight at (14.5, 8.2), opens a lever gate, and triggers Save (v2)
	saveV2 := postJSON("/api/session/action", map[string]any{
		"actor_name": "d2d-wiz-hero",
		"action":     "save_world",
		"label":      "Floor 4 Foundry Lever & Wounded Knight",
		"world": map[string]any{
			"floor":      4,
			"max_floors": 8,
			"hero": map[string]any{
				"x":                12.5,
				"y":                8.0,
				"hp":               62,
				"max_hp":           75,
				"armor":            0,
				"character_class":  "wizard",
				"equipped_weapon":  "runic-firestaff",
				"equipped_relic":   "chrono-sigil",
				"potions":          2,
				"gold":             240,
				"score":            850,
				"inventory":        []string{"crypt-key", "runic-firestaff", "healing-potion", "healing-potion"},
			},
			"floors": map[string]any{
				"4": map[string]any{
					"enemies": []map[string]any{
						{
							"id":        "f4:knight-1",
							"archetype": "shadow_knight",
							"name":      "Shadow Knight",
							"x":         14.5,
							"y":         8.2,
							"hp":        18,
							"max_hp":    60,
							"alive":     true,
							"ai_state":  "chasing",
						},
					},
					"items": []map[string]any{
						{
							"id":        "f4:relic",
							"kind":      "chrono-sigil",
							"x":         6,
							"y":         3,
							"collected": true,
						},
					},
					"interactables": []map[string]any{
						{
							"id":    "f4:foundry-gate",
							"kind":  "gate",
							"x":     10,
							"y":     8,
							"state": "open",
						},
					},
				},
			},
		},
	})

	if saveV2.Session.Floor != 4 || saveV2.Session.HP != 62 || saveV2.SaveCount != 2 {
		t.Fatalf("expected Floor 4, HP 62, SaveCount 2 after save_world; got Floor=%d HP=%d SaveCount=%d",
			saveV2.Session.Floor, saveV2.Session.HP, saveV2.SaveCount)
	}
	f4 := saveV2.Session.World.Floors["4"]
	if len(f4.Enemies) != 1 || f4.Enemies[0].HP != 18 || f4.Enemies[0].X != 14.5 {
		t.Fatalf("expected wounded knight at (14.5, 8.2) with 18 HP in saved WorldState, got %+v", f4.Enemies)
	}

	// 3. Player advances to Floor 6, unlocks Sunforged Vault (sealing Voidbane Vault), takes heavy damage (11 HP), and saves v3
	saveV3 := postJSON("/api/session/action", map[string]any{
		"actor_name": "d2d-wiz-hero",
		"action":     "save_world",
		"label":      "Floor 6 Sunforged Vault Chosen",
		"world": map[string]any{
			"floor":      6,
			"max_floors": 8,
			"hero": map[string]any{
				"x":               9.0,
				"y":               11.0,
				"hp":              11,
				"max_hp":          75,
				"character_class": "wizard",
				"equipped_armor":  "sunforged-aegis",
				"gold":            410,
				"score":           1420,
				"inventory":       []string{"crypt-key", "runic-firestaff", "sunforged-aegis"},
			},
			"floors": map[string]any{
				"6": map[string]any{
					"interactables": []map[string]any{
						{"id": "f6:vault-sunforged", "kind": "vault", "x": 5, "y": 4, "state": "open"},
						{"id": "f6:vault-voidbane", "kind": "vault", "x": 19, "y": 4, "state": "sealed"},
					},
				},
			},
		},
	})
	if saveV3.Session.Floor != 6 || saveV3.SaveCount != 3 {
		t.Fatalf("expected Floor 6 and SaveCount 3, got Floor=%d SaveCount=%d", saveV3.Session.Floor, saveV3.SaveCount)
	}

	// 4. Revert back to Snapshot v2 (Floor 4) -> restores exact Floor 4 WorldState (HP=62, Knight at 14.5,8.2 with 18 HP)!
	revV2 := postJSON("/api/session/revert", map[string]any{
		"actor_name":     "d2d-wiz-hero",
		"target_version": 2,
	})
	if revV2.Session.Floor != 4 || revV2.Session.HP != 62 || revV2.Session.World == nil {
		t.Fatalf("expected revert to v2 to restore Floor 4 and 62 HP, got Floor=%d HP=%d", revV2.Session.Floor, revV2.Session.HP)
	}
	revF4 := revV2.Session.World.Floors["4"]
	if len(revF4.Enemies) != 1 || revF4.Enemies[0].HP != 18 || revF4.Enemies[0].X != 14.5 {
		t.Fatalf("expected revert to v2 to restore wounded knight at (14.5,8.2) with 18 HP, got %+v", revF4.Enemies)
	}

	// 5. Fork timeline from v2 so player can explore the alternate Floor 6 vault branch!
	forked := postJSON("/api/session/fork", map[string]any{
		"source_actor": "d2d-wiz-hero",
		"new_actor":    "d2d-wiz-hero-alt",
	})
	if forked.ActorName != "d2d-wiz-hero-alt" || forked.Session.Floor != 4 || forked.Session.World == nil {
		t.Fatalf("expected forked actor on Floor 4 with cloned WorldState, got %+v", forked.Session)
	}
}

func TestPlayHTMLPixiEngineClassKitsAndMobileControls(t *testing.T) {
	b := NewLocalSimulatorBackend("anything-but-agents", "dungeon2d")
	gw := NewGatewayServer(b, "http://localhost:8090", "test-presenter-secret")
	srv := httptest.NewServer(gw.Mux())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/play")
	if err != nil {
		t.Fatalf("GET /play failed: %v", err)
	}
	defer resp.Body.Close()
	raw := new(bytes.Buffer)
	_, _ = raw.ReadFrom(resp.Body)
	html := raw.Bytes()

	requiredTokens := []string{
		"PIXI",
		"CLASS_KITS",
		"Sunblade Cleave",
		"Aegis Shield",
		"Longbow Shot",
		"Tumble & Snare",
		"Arcane Fireball",
		"Frost Blink",
		"btnAttack",
		"btnSkill",
		"joystickZone",
		"joystickKnob",
		"syncCanvasViewportSize",
		"drawCenteredCanvasLabel",
		"unsavedBadge",
		"serializeWorldState",
		"hydrateWorldState",
		"save_world",
		"toggleLightMode",
	}
	for _, tok := range requiredTokens {
		if !bytes.Contains(html, []byte(tok)) {
			t.Fatalf("expected /play to contain %q for PixiJS v8 engine, class kits, and mobile controls", tok)
		}
	}
}

func TestPlayHTMLEightFloorCampaignAndEnemyPathfindingAI(t *testing.T) {
	b := NewLocalSimulatorBackend("anything-but-agents", "dungeon2d")
	gw := NewGatewayServer(b, "http://localhost:8090", "test-presenter-secret")
	srv := httptest.NewServer(gw.Mux())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/play")
	if err != nil {
		t.Fatalf("GET /play failed: %v", err)
	}
	defer resp.Body.Close()
	raw := new(bytes.Buffer)
	_, _ = raw.ReadFrom(resp.Body)
	html := raw.Bytes()

	requiredCampaignTokens := []string{
		"Floor 1: The Forgotten Crypt",
		"Floor 2: The Crossroad Catacombs",
		"Floor 3: The Sunken Cistern",
		"Floor 4: The Clockwork Foundry",
		"Floor 5: The Shadow Bastion",
		"Floor 6: The Vault of Forking Paths",
		"Floor 7: The Obsidian Gauntlet",
		"Floor 8: Sanctum of the Ancient Heap Dragon",
		"findPathAStar",
		"hasLineOfSight",
		"bone_archer",
		"crypt_ghoul",
		"shadow_knight",
		"dark_acolyte",
		"abyssal_warden",
		"heap_dragon",
		"sunforged-aegis",
		"voidbane-relic",
	}
	for _, tok := range requiredCampaignTokens {
		if !bytes.Contains(html, []byte(tok)) {
			t.Fatalf("expected /play to contain %q for 8-floor campaign and enemy pathfinding AI", tok)
		}
	}
}

func TestDashboardEightFloorProgressionAndDemoBot(t *testing.T) {
	b := NewLocalSimulatorBackend("anything-but-agents", "dungeon2d")
	gw := NewGatewayServer(b, "http://localhost:8090", "test-presenter-secret")
	srv := httptest.NewServer(gw.Mux())
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/dashboard", nil)
	req.Header.Set(PresenterHeaderName, gw.PresenterKey())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /dashboard failed: %v", err)
	}
	defer resp.Body.Close()
	raw := new(bytes.Buffer)
	_, _ = raw.ReadFrom(resp.Body)
	dashHTML := raw.Bytes()

	requiredDashTokens := []string{
		"toggleLightMode",
		"light-mode",
		"sunforged-aegis",
		"voidbane-relic",
		"/8",
		"qrModal",
		"openQrModal",
		"closeQrModal",
		"qrModalImage",
	}
	for _, tok := range requiredDashTokens {
		if !bytes.Contains(dashHTML, []byte(tok)) {
			t.Fatalf("expected /dashboard to contain %q for 8-floor progression, relic badges, and Projector Light Mode", tok)
		}
	}

	// Trigger the 8-Floor Demo Bot and verify it reaches Floor 8 Victory after a Save-Scum Revert
	botPayload, _ := json.Marshal(map[string]any{
		"hero_name": "bot-eight",
		"mode":      "full-demo",
		"delay_ms":  0,
	})
	botReq, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/session/bot", bytes.NewReader(botPayload))
	botReq.Header.Set("Content-Type", "application/json")
	botReq.Header.Set(PresenterHeaderName, gw.PresenterKey())
	botResp, err := http.DefaultClient.Do(botReq)
	if err != nil {
		t.Fatalf("POST /api/session/bot failed: %v", err)
	}
	defer botResp.Body.Close()
	if botResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 from /api/session/bot, got %d", botResp.StatusCode)
	}
	var botRec SessionRecord
	if err := json.NewDecoder(botResp.Body).Decode(&botRec); err != nil {
		t.Fatalf("decode bot SessionRecord failed: %v", err)
	}
	if botRec.Session.Floor != 8 || !botRec.Session.Victory || botRec.RevertCount < 1 || botRec.Session.World == nil {
		t.Fatalf("expected demo bot to reach Floor 8 Victory with RevertCount>=1 and World!=nil, got Floor=%d Victory=%v Reverts=%d World=%v",
			botRec.Session.Floor, botRec.Session.Victory, botRec.RevertCount, botRec.Session.World != nil)
	}
}

func TestAllEightFloorsFullyExplorableAndElevatedVisuals(t *testing.T) {
	raw, err := webFS.ReadFile("web/play.html")
	if err != nil {
		t.Fatalf("failed to read web/play.html: %v", err)
	}
	html := string(raw)

	// 1. Verify elevated 2.5D graphics, multi-part animated character/monster renderer, and particle engine
	visualTokens := []string{
		"drawBiomeTileAndWall",
		"drawDetailedHero",
		"drawDetailedEnemy",
		"drawDetailedInteractable",
		"spawnParticles",
		"updateAndRenderParticles",
		"bossHudBanner",
		"FLOOR_BLUEPRINTS_JSON_START",
		"FLOOR_BLUEPRINTS_JSON_END",
	}
	for _, tok := range visualTokens {
		if !bytes.Contains(raw, []byte(tok)) {
			t.Fatalf("expected web/play.html to contain elevated visual engine token %q", tok)
		}
	}

	// 2. Extract FLOOR_BLUEPRINTS JSON and run BFS reachability across all 8 floors
	const startMarker = "/* FLOOR_BLUEPRINTS_JSON_START */"
	const endMarker = "/* FLOOR_BLUEPRINTS_JSON_END */"
	sIdx := bytes.Index(raw, []byte(startMarker))
	eIdx := bytes.Index(raw, []byte(endMarker))
	if sIdx < 0 || eIdx <= sIdx {
		t.Fatalf("missing FLOOR_BLUEPRINTS_JSON markers in web/play.html")
	}
	jsonBytes := bytes.TrimSpace(raw[sIdx+len(startMarker) : eIdx])

	type coord struct {
		X int `json:"x"`
		Y int `json:"y"`
	}
	type bpEnemy struct {
		ID        string `json:"id"`
		Archetype string `json:"archetype"`
		X         int    `json:"x"`
		Y         int    `json:"y"`
	}
	type bpItem struct {
		ID      string `json:"id"`
		Kind    string `json:"kind"`
		X       int    `json:"x"`
		Y       int    `json:"y"`
		VaultID string `json:"vaultId"`
	}
	type bpInteractable struct {
		ID         string `json:"id"`
		Kind       string `json:"kind"`
		X          int    `json:"x"`
		Y          int    `json:"y"`
		State      string `json:"state"`
		TargetGate string `json:"targetGate"`
	}
	type floorBP struct {
		Name          string           `json:"name"`
		Spawn         coord            `json:"spawn"`
		Grid          []string         `json:"grid"`
		Enemies       []bpEnemy        `json:"enemies"`
		Items         []bpItem         `json:"items"`
		Interactables []bpInteractable `json:"interactables"`
	}

	var floors map[string]floorBP
	if err := json.Unmarshal(jsonBytes, &floors); err != nil {
		t.Fatalf("failed to unmarshal FLOOR_BLUEPRINTS JSON: %v", err)
	}
	if len(floors) != 8 {
		t.Fatalf("expected 8 floors in FLOOR_BLUEPRINTS, got %d", len(floors))
	}

	bfs := func(grid []string, start coord, blocked map[coord]bool) map[coord]bool {
		visited := map[coord]bool{start: true}
		q := []coord{start}
		dirs := []coord{{1, 0}, {-1, 0}, {0, 1}, {0, -1}}
		for len(q) > 0 {
			cur := q[0]
			q = q[1:]
			for _, d := range dirs {
				nx, ny := cur.X+d.X, cur.Y+d.Y
				next := coord{nx, ny}
				if ny < 0 || ny >= len(grid) || nx < 0 || nx >= len(grid[0]) {
					continue
				}
				if grid[ny][nx] == '#' || blocked[next] || visited[next] {
					continue
				}
				visited[next] = true
				q = append(q, next)
			}
		}
		return visited
	}

	isAdjacentVisited := func(visited map[coord]bool, target coord) bool {
		if visited[target] {
			return true
		}
		for _, d := range []coord{{1, 0}, {-1, 0}, {0, 1}, {0, -1}} {
			if visited[coord{target.X + d.X, target.Y + d.Y}] {
				return true
			}
		}
		return false
	}

	_ = html
	for _, fKey := range []string{"1", "2", "3", "4", "5", "6", "7", "8"} {
		bp, ok := floors[fKey]
		if !ok {
			t.Fatalf("missing floor %s in FLOOR_BLUEPRINTS", fKey)
		}
		if len(bp.Grid) != 16 {
			t.Fatalf("floor %s expected 16 rows, got %d", fKey, len(bp.Grid))
		}
		for r, row := range bp.Grid {
			if len(row) != 22 {
				t.Fatalf("floor %s row %d expected 22 cols, got %d", fKey, r, len(row))
			}
		}
		if bp.Grid[bp.Spawn.Y][bp.Spawn.X] == '#' {
			t.Fatalf("floor %s spawn (%d,%d) is on a wall '#'", fKey, bp.Spawn.X, bp.Spawn.Y)
		}

		// Verify all enemies, items, and interactables are on walkable '.' tiles
		for _, en := range bp.Enemies {
			if bp.Grid[en.Y][en.X] == '#' {
				t.Fatalf("floor %s enemy %s at (%d,%d) is on a wall '#'", fKey, en.ID, en.X, en.Y)
			}
		}
		for _, it := range bp.Items {
			if bp.Grid[it.Y][it.X] == '#' {
				t.Fatalf("floor %s item %s at (%d,%d) is on a wall '#'", fKey, it.ID, it.X, it.Y)
			}
		}

		closedGates := map[coord]bool{}
		for _, ia := range bp.Interactables {
			if bp.Grid[ia.Y][ia.X] == '#' {
				t.Fatalf("floor %s interactable %s at (%d,%d) is on a wall '#'", fKey, ia.ID, ia.X, ia.Y)
			}
			if (ia.Kind == "gate" || ia.Kind == "vault") && ia.State != "open" {
				closedGates[coord{ia.X, ia.Y}] = true
			}
		}

		// Pre-gate BFS: verify levers, keys, shrines, and gate/vault approaches are reachable BEFORE opening gates/vaults
		preReach := bfs(bp.Grid, bp.Spawn, closedGates)
		for _, ia := range bp.Interactables {
			pt := coord{ia.X, ia.Y}
			if ia.Kind == "lever" || ia.Kind == "shrine" || ia.Kind == "stairs_up" {
				if !preReach[pt] {
					t.Fatalf("floor %s %s (%s) at (%d,%d) is unreachable before opening gates!", fKey, ia.Kind, ia.ID, ia.X, ia.Y)
				}
			}
			if ia.Kind == "gate" || ia.Kind == "vault" {
				if !isAdjacentVisited(preReach, pt) {
					t.Fatalf("floor %s %s (%s) at (%d,%d) cannot even be approached from spawn!", fKey, ia.Kind, ia.ID, ia.X, ia.Y)
				}
			}
		}
		for _, it := range bp.Items {
			if it.VaultID == "" && (it.Kind == "crypt-key" || it.Kind == "dragon-sigil") {
				if !preReach[coord{it.X, it.Y}] {
					t.Fatalf("floor %s progression key %s at (%d,%d) is unreachable before opening gates!", fKey, it.ID, it.X, it.Y)
				}
			}
		}

		// Full floor BFS (with gates/vaults unlocked): every single '.' tile on the floor must be reachable!
		fullReach := bfs(bp.Grid, bp.Spawn, nil)
		for r, row := range bp.Grid {
			for c, ch := range row {
				if ch == '.' && !fullReach[coord{c, r}] {
					t.Fatalf("floor %s has unreachable '.' tile at (%d,%d) — floor is not 100%% explorable!", fKey, c, r)
				}
			}
		}
	}
}

func TestClusterAteBackendWorkerPoolIsolation(t *testing.T) {
	tmpDir := t.TempDir()
	logPath := filepath.Join(tmpDir, "suspended.log")
	fakeAtePath := filepath.Join(tmpDir, "fake-kubectl-ate.sh")
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = \"get\" ] && [ \"$2\" = \"workers\" ]; then\n" +
		"  cat <<'EOF'\n" +
		"{\"workers\":[\n" +
		"  {\"labels\":{\"workload\":\"slides\"},\"workerPool\":\"wp-slides\",\"workerPod\":\"wp-slides-1\",\"nodeName\":\"node-a\",\"status\":{\"state\":\"WORKER_STATE_ACTIVE\",\"ips\":[\"10.120.0.10\"]}},\n" +
		"  {\"labels\":{\"workload\":\"dungeon\"},\"workerPool\":\"wp-dungeon\",\"workerPod\":\"wp-dungeon-1\",\"nodeName\":\"node-a\",\"status\":{\"state\":\"WORKER_STATE_ACTIVE\",\"ips\":[\"10.120.1.21\"]}},\n" +
		"  {\"labels\":{\"workload\":\"dungeon\"},\"workerPool\":\"wp-dungeon\",\"workerPod\":\"wp-dungeon-2\",\"nodeName\":\"node-b\",\"status\":{\"state\":\"WORKER_STATE_ACTIVE\",\"ips\":[\"10.120.2.22\"]}}\n" +
		"]}\n" +
		"EOF\n" +
		"  exit 0\n" +
		"fi\n" +
		"if [ \"$1\" = \"get\" ] && [ \"$2\" = \"actors\" ] && [ \"$5\" = \"-o\" ]; then\n" +
		"  echo '{\"actors\":[]}'\n" +
		"  exit 0\n" +
		"fi\n" +
		"if [ \"$1\" = \"get\" ] && [ \"$2\" = \"actors\" ]; then\n" +
		"  echo 'ATESPACE NAME TEMPLATE STATE WORKER_POD WORKER_IPS'\n" +
		"  echo 'anything-but-agents slide-01 anything-but-agents/slides ACTOR_STATE_RUNNING anything-but-agents/wp-slides-1 10.120.0.10'\n" +
		"  echo 'anything-but-agents coldstart-1 anything-but-agents/coldstart ACTOR_STATE_RUNNING anything-but-agents/wp-coldstart-1 10.120.0.30'\n" +
		"  echo 'anything-but-agents d2d-other anything-but-agents/dungeon2d ACTOR_STATE_RUNNING anything-but-agents/wp-dungeon-1 10.120.1.21'\n" +
		"  echo 'anything-but-agents d2d-keep anything-but-agents/dungeon2d ACTOR_STATE_RUNNING anything-but-agents/wp-dungeon-2 10.120.2.22'\n" +
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

	b := NewClusterAteBackend("anything-but-agents", "dungeon2d", "http://127.0.0.1:8000", "", fakeAtePath)
	b.reclaimWorkerSlots(context.Background(), "d2d-keep")

	rawLog, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read suspended log: %v", err)
	}
	suspended := strings.Fields(string(rawLog))
	if !slices.Equal(suspended, []string{"d2d-other"}) {
		t.Fatalf("expected only dungeon actor d2d-other to be suspended, got %v", suspended)
	}

	snap, err := b.DashboardState(context.Background())
	if err != nil {
		t.Fatalf("DashboardState: %v", err)
	}
	if len(snap.Workers) != 2 || snap.Workers[0].IP != "10.120.1.21" || snap.Workers[1].IP != "10.120.2.22" {
		t.Fatalf("expected only wp-dungeon workers with status.ips hydrated, got %+v", snap.Workers)
	}
}

func TestClusterAteBackendResetAllSessionsOnlyDeletesDungeon2D(t *testing.T) {
	tmpDir := t.TempDir()
	delLogPath := filepath.Join(tmpDir, "deleted.log")
	fakeAtePath := filepath.Join(tmpDir, "fake-kubectl-ate.sh")
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = \"get\" ] && [ \"$2\" = \"actors\" ] && [ \"$5\" = \"-o\" ]; then\n" +
		"  cat <<'EOF'\n" +
		"{\"actors\":[\n" +
		"  {\"actorTemplate\":{\"name\":\"slides\"},\"metadata\":{\"name\":\"slide-01\"}},\n" +
		"  {\"actorTemplate\":{\"name\":\"coldstart\"},\"metadata\":{\"name\":\"coldstart-1\"}},\n" +
		"  {\"actorTemplate\":{\"name\":\"dungeon2d\"},\"metadata\":{\"name\":\"d2d-alpha\"}},\n" +
		"  {\"actorTemplate\":{\"name\":\"dungeon2d\"},\"metadata\":{\"name\":\"d2d-bravo\"}}\n" +
		"]}\n" +
		"EOF\n" +
		"  exit 0\n" +
		"fi\n" +
		"if [ \"$1\" = \"get\" ] && [ \"$2\" = \"tags\" ]; then\n" +
		"  echo 'ATESPACE NAME AGE'\n" +
		"  echo 'anything-but-agents tag-d2d-alpha-f4-1234 5m'\n" +
		"  echo 'anything-but-agents tag-honeypot-forensic 5m'\n" +
		"  exit 0\n" +
		"fi\n" +
		"if [ \"$1\" = \"delete\" ]; then\n" +
		"  echo \"$2:$3\" >> \"" + delLogPath + "\"\n" +
		"  exit 0\n" +
		"fi\n" +
		"exit 0\n"
	if err := os.WriteFile(fakeAtePath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake kubectl-ate: %v", err)
	}

	b := NewClusterAteBackend("anything-but-agents", "dungeon2d", "http://127.0.0.1:8000", "", fakeAtePath)
	b.records["d2d-charlie"] = &SessionRecord{ActorName: "d2d-charlie"}

	deleted, err := b.ResetAllSessions(context.Background())
	if err != nil {
		t.Fatalf("ResetAllSessions: %v", err)
	}
	if deleted != 3 {
		t.Fatalf("expected 3 d2d-* actors deleted, got %d", deleted)
	}

	raw, err := os.ReadFile(delLogPath)
	if err != nil {
		t.Fatalf("read deleted log: %v", err)
	}
	items := strings.Fields(string(raw))
	slices.Sort(items)
	want := []string{"actor:d2d-alpha", "actor:d2d-bravo", "actor:d2d-charlie", "tag:tag-d2d-alpha-f4-1234"}
	if !slices.Equal(items, want) {
		t.Fatalf("expected exact deletion set %v, got %v", want, items)
	}
}








