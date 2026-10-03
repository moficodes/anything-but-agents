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
	"strings"
	"time"
)

//go:embed web/*.html
var webFS embed.FS

type GatewayServer struct {
	backend   SubstrateBackend
	publicURL string
}

func NewGatewayServer(backend SubstrateBackend, publicURL string) *GatewayServer {
	return &GatewayServer{
		backend:   backend,
		publicURL: strings.TrimRight(publicURL, "/"),
	}
}

func (g *GatewayServer) Mux() *http.ServeMux {
	mux := http.NewServeMux()

	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})

	mux.HandleFunc("/play", func(w http.ResponseWriter, _ *http.Request) {
		raw, _ := webFS.ReadFile("web/play.html")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(raw)
	})

	mux.HandleFunc("/dashboard", func(w http.ResponseWriter, _ *http.Request) {
		raw, _ := webFS.ReadFile("web/dashboard.html")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
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
		w.Header().Set("Cache-Control", "no-store")
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
			ActorName      string `json:"actor_name"`
			Action         string `json:"action"`
			Target         string `json:"target"`
			EntityID       string `json:"entity_id"`
			CharacterClass string `json:"character_class"`
			X              int    `json:"x"`
			Y              int    `json:"y"`
			PrevX          int    `json:"prev_x"`
			PrevY          int    `json:"prev_y"`
		}
		if r.Body != nil {
			body, _ := io.ReadAll(io.LimitReader(r.Body, 8192))
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
			Target:         payload.Target,
			EntityID:       payload.EntityID,
			CharacterClass: payload.CharacterClass,
			X:              payload.X,
			Y:              payload.Y,
			PrevX:          payload.PrevX,
			PrevY:          payload.PrevY,
		})
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, rec)
	})

	mux.HandleFunc("/api/session/revert", func(w http.ResponseWriter, r *http.Request) {
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
		rec, err := g.backend.RevertSession(r.Context(), payload.ActorName)
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

	mux.HandleFunc("/api/session/delete", func(w http.ResponseWriter, r *http.Request) {
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
	})

	mux.HandleFunc("/api/session/bot", func(w http.ResponseWriter, r *http.Request) {
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
	})

	mux.HandleFunc("/api/dashboard/state", func(w http.ResponseWriter, r *http.Request) {
		snap, err := g.backend.DashboardState(r.Context())
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		if g.publicURL != "" {
			snap.PublicURL = g.publicURL
		}
		writeJSON(w, http.StatusOK, snap)
	})

	mux.HandleFunc("/api/dashboard/events", func(w http.ResponseWriter, r *http.Request) {
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
	})

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			raw, _ := webFS.ReadFile("web/play.html")
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
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
		{req: MutateRequest{Action: "pickup", Target: "healing-potion", EntityID: "f1:potion", X: 3, Y: 7}},
		{req: MutateRequest{Action: "combat", Target: "Restless Skeleton", EntityID: "f1:skeleton", X: 7, Y: 5}},
		{req: MutateRequest{Action: "pickup", Target: "crypt-key", EntityID: "f1:key", X: 11, Y: 3}},
		{req: MutateRequest{Action: "stairs", Target: "f1:stairs", EntityID: "f1:stairs", X: 13, Y: 9}},
		{req: MutateRequest{Action: "pickup", Target: "dragon-slayer-sword", EntityID: "f2:sword", X: 5, Y: 5}},
		{req: MutateRequest{Action: "quicksave", Target: "f2:shrine", EntityID: "f2:shrine", X: 10, Y: 5}},
		{req: MutateRequest{Action: "stairs", Target: "f2:stairs", EntityID: "f2:stairs", X: 13, Y: 5}},
		// Recklessly attack the Ancient Heap Dragon WITHOUT fire-shield -> DIE!
		{req: MutateRequest{Action: "combat", Target: "Ancient Heap Dragon", EntityID: "f3:dragon", X: 7, Y: 4}},
		// SAVE-SCUM! Revert actor back to pre-attack GCS snapshot
		{isRevert: true},
		// Now grab the Fire Shield and defeat the Dragon!
		{req: MutateRequest{Action: "pickup", Target: "fire-shield", EntityID: "f2:shield", X: 10, Y: 1}},
		{req: MutateRequest{Action: "combat", Target: "Ancient Heap Dragon", EntityID: "f3:dragon", X: 7, Y: 4}},
		{req: MutateRequest{Action: "combat", Target: "Ancient Heap Dragon", EntityID: "f3:dragon", X: 7, Y: 4}},
		{req: MutateRequest{Action: "pickup", Target: "golden-chalice", EntityID: "f3:chalice", X: 7, Y: 2}},
		{req: MutateRequest{Action: "escape", Target: "f3:portal", EntityID: "f3:portal", X: 7, Y: 1}},
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
