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
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/moficodes/anything-but-agents/internal/presenterauth"
)

//go:embed web/*.html
var webFS embed.FS

const (
	// PresenterCookieName is the HttpOnly cookie set when the presenter unlocks /dashboard?key=<PRESENTER_TOKEN>.
	PresenterCookieName = "d2d_presenter"
	// PresenterHeaderName carries the presenter token for CLI scripts and tests.
	PresenterHeaderName = presenterauth.PresenterHeaderName
)

type GatewayServer struct {
	backend   SubstrateBackend
	publicURL string
	authGate  *presenterauth.Gate
}

func NewGatewayServer(backend SubstrateBackend, publicURL string, presenterKeys ...string) *GatewayServer {
	var key string
	if len(presenterKeys) > 0 {
		key = strings.TrimSpace(presenterKeys[0])
	}
	if key == "" {
		key = strings.TrimSpace(os.Getenv("PRESENTER_TOKEN"))
	}
	if key == "" {
		key = presenterauth.RandomToken(24)
	}
	return &GatewayServer{
		backend:   backend,
		publicURL: strings.TrimRight(publicURL, "/"),
		authGate: presenterauth.NewGate(presenterauth.GateConfig{
			Title:            "Demo 1b: Substrate Dungeon Mission Control",
			Subtitle:         "Sign in with an authorized Google Workspace or event account to open the Live Mission-Control Dashboard, QR code projector modal, and bot runner.",
			SecretName:       "dungeon2d-gateway",
			LegacyCookieName: PresenterCookieName,
			PresenterToken:   key,
		}),
	}
}

func (g *GatewayServer) PresenterKey() string {
	return g.authGate.PresenterKey()
}

// IsPresenter reports whether the request carries a valid Google session cookie,
// X-Presenter-Key header, or d2d_presenter cookie.
func (g *GatewayServer) IsPresenter(r *http.Request) bool {
	return g.authGate.IsAuthorized(r)
}

func (g *GatewayServer) requirePresenter(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !g.IsPresenter(r) {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "presenter sign-in or presenter key required"})
			return
		}
		next(w, r)
	}
}

func (g *GatewayServer) Mux() *http.ServeMux {
	mux := http.NewServeMux()
	g.authGate.RegisterRoutes(mux)

	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})

	mux.HandleFunc("/play", func(w http.ResponseWriter, _ *http.Request) {
		raw, _ := webFS.ReadFile("web/play.html")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate, max-age=0")
		w.Header().Set("Pragma", "no-cache")
		w.Header().Set("Expires", "0")
		_, _ = w.Write(raw)
	})

	mux.HandleFunc("/dashboard", func(w http.ResponseWriter, r *http.Request) {
		if g.authGate.HandleUnlockOrGate(w, r, "/dashboard") {
			return
		}
		raw, _ := webFS.ReadFile("web/dashboard.html")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate, max-age=0")
		w.Header().Set("Pragma", "no-cache")
		w.Header().Set("Expires", "0")
		_, _ = w.Write(raw)
	})

	mux.HandleFunc("/api/qr.svg", func(w http.ResponseWriter, r *http.Request) {
		targetURL := r.URL.Query().Get("url")
		if targetURL == "" {
			if g.publicURL != "" {
				targetURL = g.publicURL + "/play"
			} else {
				scheme := "http"
				if r.TLS != nil {
					scheme = "https"
				}
				host := r.Host
				if host == "" {
					host = "localhost:8090"
				}
				targetURL = fmt.Sprintf("%s://%s/play", scheme, host)
			}
		}
		svg := RenderQRSVG(targetURL)
		w.Header().Set("Content-Type", "image/svg+xml")
		w.Header().Set("Cache-Control", "private, max-age=300")
		_, _ = w.Write([]byte(svg))
	})

	mux.HandleFunc("/api/session/start", func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			HeroName       string `json:"hero_name"`
			CharacterClass string `json:"character_class"`
		}
		if r.Body != nil {
			body, _ := io.ReadAll(io.LimitReader(r.Body, 8192))
			_ = json.Unmarshal(body, &payload)
		}
		if payload.HeroName == "" {
			payload.HeroName = r.URL.Query().Get("hero")
		}
		if payload.HeroName == "" {
			payload.HeroName = fmt.Sprintf("hero-%d", time.Now().Unix()%1000)
		}
		rec, err := g.backend.StartSession(r.Context(), payload.HeroName, payload.CharacterClass)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, rec)
	})

	mux.HandleFunc("/api/session/state", func(w http.ResponseWriter, r *http.Request) {
		actorName := r.URL.Query().Get("actor")
		if actorName == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing actor parameter"})
			return
		}
		rec, err := g.backend.GetSession(r.Context(), actorName)
		if err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, rec)
	})

	mux.HandleFunc("/api/session/action", func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			ActorName      string      `json:"actor_name"`
			Action         string      `json:"action"`
			Label          string      `json:"label"`
			Target         string      `json:"target"`
			EntityID       string      `json:"entity_id"`
			CharacterClass string      `json:"character_class"`
			X              int         `json:"x"`
			Y              int         `json:"y"`
			PrevX          int         `json:"prev_x"`
			PrevY          int         `json:"prev_y"`
			World          *WorldState `json:"world,omitempty"`
		}
		if r.Body != nil {
			body, _ := io.ReadAll(io.LimitReader(r.Body, 262144))
			_ = json.Unmarshal(body, &payload)
		}
		if payload.ActorName == "" {
			payload.ActorName = r.URL.Query().Get("actor")
		}
		if payload.Action == "" {
			payload.Action = r.URL.Query().Get("action")
		}
		rec, err := g.backend.ExecuteMilestone(r.Context(), payload.ActorName, MutateRequest{
			Action:         payload.Action,
			Label:          payload.Label,
			Target:         payload.Target,
			EntityID:       payload.EntityID,
			CharacterClass: payload.CharacterClass,
			X:              payload.X,
			Y:              payload.Y,
			PrevX:          payload.PrevX,
			PrevY:          payload.PrevY,
			World:          payload.World,
		})
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, rec)
	})

	mux.HandleFunc("/api/session/revert", func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			ActorName     string `json:"actor_name"`
			TargetVersion int    `json:"target_version"`
		}
		if r.Body != nil {
			body, _ := io.ReadAll(io.LimitReader(r.Body, 4096))
			_ = json.Unmarshal(body, &payload)
		}
		if payload.ActorName == "" {
			payload.ActorName = r.URL.Query().Get("actor")
		}
		rec, err := g.backend.RevertSession(r.Context(), payload.ActorName, payload.TargetVersion)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, rec)
	})

	mux.HandleFunc("/api/session/fork", func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			SourceActor string `json:"source_actor"`
			NewActor    string `json:"new_actor"`
		}
		if r.Body != nil {
			body, _ := io.ReadAll(io.LimitReader(r.Body, 4096))
			_ = json.Unmarshal(body, &payload)
		}
		if payload.SourceActor == "" {
			payload.SourceActor = r.URL.Query().Get("actor")
		}
		rec, err := g.backend.ForkSession(r.Context(), payload.SourceActor, payload.NewActor)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, rec)
	})

	mux.HandleFunc("/api/session/delete", g.requirePresenter(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			ActorName string `json:"actor_name"`
		}
		if r.Body != nil {
			body, _ := io.ReadAll(io.LimitReader(r.Body, 4096))
			_ = json.Unmarshal(body, &payload)
		}
		if payload.ActorName == "" {
			payload.ActorName = r.URL.Query().Get("actor")
		}
		if err := g.backend.DeleteSession(r.Context(), payload.ActorName); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"deleted": payload.ActorName})
	}))

	mux.HandleFunc("/api/session/bot", g.requirePresenter(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			HeroName string `json:"hero_name"`
			Mode     string `json:"mode"`
			DelayMs  int    `json:"delay_ms"`
		}
		if r.Body != nil {
			body, _ := io.ReadAll(io.LimitReader(r.Body, 4096))
			_ = json.Unmarshal(body, &payload)
		}
		if payload.HeroName == "" {
			payload.HeroName = fmt.Sprintf("bot-%d", time.Now().Unix()%1000)
		}
		rec, err := g.runDemoBot(r.Context(), payload.HeroName, time.Duration(payload.DelayMs)*time.Millisecond)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, rec)
	}))

	mux.HandleFunc("/api/dashboard/reset", g.requirePresenter(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "POST required"})
			return
		}
		deleted, err := g.backend.ResetAllSessions(r.Context())
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"deleted":   deleted,
			"remaining": 0,
			"status":    "ok",
		})
	}))

	mux.HandleFunc("/api/dashboard/state", g.requirePresenter(func(w http.ResponseWriter, r *http.Request) {
		snap, err := g.backend.DashboardState(r.Context())
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		if g.publicURL != "" {
			snap.PublicURL = g.publicURL
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(snap)
	}))

	mux.HandleFunc("/api/dashboard/events", g.requirePresenter(func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")

		ch, unsub := g.backend.SubscribeEvents()
		defer unsub()

		_, _ = fmt.Fprintf(w, "data: {\"ready\":true}\n\n")
		flusher.Flush()

		for {
			select {
			case <-r.Context().Done():
				return
			case ev := <-ch:
				raw, _ := json.Marshal(ev)
				_, _ = fmt.Fprintf(w, "data: %s\n\n", raw)
				flusher.Flush()
			}
		}
	}))

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			raw, _ := webFS.ReadFile("web/play.html")
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate, max-age=0")
			w.Header().Set("Pragma", "no-cache")
			w.Header().Set("Expires", "0")
			_, _ = w.Write(raw)
			return
		}
		http.NotFound(w, r)
	})

	return mux
}

func (g *GatewayServer) runDemoBot(ctx context.Context, heroName string, stepDelay time.Duration) (SessionRecord, error) {
	rec, err := g.backend.StartSession(ctx, heroName, "paladin")
	if err != nil {
		return SessionRecord{}, err
	}
	actor := rec.ActorName
	pause := func() {
		if stepDelay > 0 {
			time.Sleep(stepDelay)
		}
	}

	steps := []struct {
		isRevert bool
		req      MutateRequest
	}{
		{
			req: MutateRequest{
				Action: "save_world",
				Label:  "Floor 2 Crossroad Catacombs Cleared",
				World: &WorldState{
					Floor:     2,
					MaxFloors: 8,
					Hero: HeroWorldState{
						X:              18,
						Y:              8,
						HP:             128,
						MaxHP:          140,
						Armor:          25,
						CharacterClass: "paladin",
						EquippedWeapon: "sunblade-greatsword",
						Potions:        2,
						Gold:           140,
						Score:          320,
						Inventory:      []string{"crypt-key", "sunblade-greatsword", "healing-potion", "healing-potion"},
					},
					Floors: map[string]FloorWorldState{
						"2": {
							Enemies: []EnemyState{
								{ID: "f2:archer-1", Archetype: "bone_archer", Name: "Bone Archer", X: 14, Y: 7, HP: 0, MaxHP: 28, Alive: false, AIState: "dead"},
							},
						},
					},
				},
			},
		},
		{
			req: MutateRequest{
				Action: "save_world",
				Label:  "Floor 4 Clockwork Foundry Gate Opened",
				World: &WorldState{
					Floor:     4,
					MaxFloors: 8,
					Hero: HeroWorldState{
						X:              16,
						Y:              8,
						HP:             112,
						MaxHP:          140,
						Armor:          25,
						CharacterClass: "paladin",
						EquippedWeapon: "sunblade-greatsword",
						EquippedRelic:  "chrono-sigil",
						Potions:        2,
						Gold:           290,
						Score:          740,
						Inventory:      []string{"crypt-key", "sunblade-greatsword", "chrono-sigil", "healing-potion"},
					},
					Floors: map[string]FloorWorldState{
						"4": {
							Interactables: []InteractableState{
								{ID: "f4:foundry-gate", Kind: "gate", X: 14, Y: 8, State: "open"},
							},
						},
					},
				},
			},
		},
		{
			req: MutateRequest{
				Action: "save_world",
				Label:  "Floor 6 Sunforged Vault Unlocked",
				World: &WorldState{
					Floor:     6,
					MaxFloors: 8,
					Hero: HeroWorldState{
						X:              19,
						Y:              8,
						HP:             134,
						MaxHP:          140,
						Armor:          35,
						CharacterClass: "paladin",
						EquippedWeapon: "dragon-slayer-sword",
						EquippedArmor:  "sunforged-aegis",
						EquippedRelic:  "chrono-sigil",
						Potions:        2,
						Gold:           480,
						Score:          1280,
						Inventory:      []string{"crypt-key", "dragon-slayer-sword", "sunforged-aegis", "chrono-sigil"},
					},
					Floors: map[string]FloorWorldState{
						"6": {
							Interactables: []InteractableState{
								{ID: "f6:vault-sunforged", Kind: "vault", X: 6, Y: 3, State: "open"},
								{ID: "f6:vault-voidbane", Kind: "vault", X: 16, Y: 12, State: "sealed"},
							},
						},
					},
				},
			},
		},
		{
			req: MutateRequest{
				Action:   "quicksave",
				Target:   "f7:shrine",
				EntityID: "f7:shrine",
				Label:    "Floor 7 Obsidian Gauntlet Pre-Boss Shrine",
				World: &WorldState{
					Floor:     7,
					MaxFloors: 8,
					Hero: HeroWorldState{
						X:              18,
						Y:              8,
						HP:             140,
						MaxHP:          140,
						Armor:          35,
						CharacterClass: "paladin",
						EquippedWeapon: "dragon-slayer-sword",
						EquippedArmor:  "sunforged-aegis",
						EquippedRelic:  "chrono-sigil",
						Potions:        3,
						Gold:           620,
						Score:          1680,
						Inventory:      []string{"crypt-key", "dragon-slayer-sword", "sunforged-aegis", "chrono-sigil"},
					},
				},
			},
		},
		// Recklessly charge Ancient Heap Dragon on Floor 8 without fire-shield equipped -> DIE in uncommitted RAM!
		{
			req: MutateRequest{
				Action:   "combat",
				Target:   "Ancient Heap Dragon",
				EntityID: "f8:dragon",
				X:        15,
				Y:        8,
			},
		},
		// SAVE-SCUM! Revert actor back to Floor 7 Pre-Boss Shrine snapshot
		{isRevert: true},
		// Equip Fire Shield + Sunforged Aegis and slay the Ancient Heap Dragon on Floor 8!
		{
			req: MutateRequest{
				Action: "victory",
				Label:  "Floor 8 Ancient Heap Dragon Slain & Sanctum Escaped",
				World: &WorldState{
					Floor:     8,
					MaxFloors: 8,
					Hero: HeroWorldState{
						X:              20,
						Y:              8,
						HP:             96,
						MaxHP:          140,
						Armor:          45,
						CharacterClass: "paladin",
						EquippedWeapon: "dragon-slayer-sword",
						EquippedArmor:  "sunforged-aegis",
						EquippedRelic:  "chrono-sigil",
						Potions:        1,
						Gold:           950,
						Score:          2650,
						Inventory:      []string{"crypt-key", "dragon-slayer-sword", "fire-shield", "sunforged-aegis", "chrono-sigil", "golden-chalice"},
					},
					Floors: map[string]FloorWorldState{
						"8": {
							Enemies: []EnemyState{
								{ID: "f8:dragon", Archetype: "heap_dragon", Name: "Ancient Heap Dragon", X: 15, Y: 8, HP: 0, MaxHP: 240, Alive: false, AIState: "dead"},
							},
						},
					},
				},
			},
		},
	}

	for _, st := range steps {
		pause()
		if st.isRevert {
			rec, err = g.backend.RevertSession(ctx, actor)
		} else {
			rec, err = g.backend.ExecuteMilestone(ctx, actor, st.req)
		}
		if err != nil {
			return rec, err
		}
	}
	return rec, nil
}
