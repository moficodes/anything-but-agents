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
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
)

//go:embed web/index.html
var indexHTML []byte

const presenterCookieName = "tdb_presenter"

type GatewayServer struct {
	backend        TenantDBBackend
	presenterToken string
}

func NewGatewayServer(backend TenantDBBackend, presenterToken string) *GatewayServer {
	return &GatewayServer{
		backend:        backend,
		presenterToken: strings.TrimSpace(presenterToken),
	}
}

func (g *GatewayServer) isAuthorized(r *http.Request) bool {
	if g.presenterToken == "" {
		return true
	}
	if key := strings.TrimSpace(r.Header.Get("X-Presenter-Key")); key != "" {
		if subtle.ConstantTimeCompare([]byte(key), []byte(g.presenterToken)) == 1 {
			return true
		}
	}
	if key := strings.TrimSpace(r.URL.Query().Get("key")); key != "" {
		if subtle.ConstantTimeCompare([]byte(key), []byte(g.presenterToken)) == 1 {
			return true
		}
	}
	if c, err := r.Cookie(presenterCookieName); err == nil {
		if subtle.ConstantTimeCompare([]byte(c.Value), []byte(g.presenterToken)) == 1 {
			return true
		}
	}
	return false
}

func (g *GatewayServer) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !g.isAuthorized(r) {
			writeJSONError(w, http.StatusUnauthorized, "unauthorized: provide ?key=<PRESENTER_TOKEN> or X-Presenter-Key header")
			return
		}
		next(w, r)
	}
}

func (g *GatewayServer) Mux() *http.ServeMux {
	mux := http.NewServeMux()

	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})

	serveUI := func(w http.ResponseWriter, r *http.Request) {
		if g.presenterToken != "" {
			if qKey := strings.TrimSpace(r.URL.Query().Get("key")); qKey != "" &&
				subtle.ConstantTimeCompare([]byte(qKey), []byte(g.presenterToken)) == 1 {
				http.SetCookie(w, &http.Cookie{
					Name:     presenterCookieName,
					Value:    g.presenterToken,
					Path:     "/",
					HttpOnly: true,
					SameSite: http.SameSiteStrictMode,
				})
				http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
				return
			}
			if !g.isAuthorized(r) {
				writeJSONError(w, http.StatusUnauthorized, "presenter dashboard locked: open /dashboard?key=<PRESENTER_TOKEN>")
				return
			}
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(indexHTML)
	}

	mux.HandleFunc("/dashboard", serveUI)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		serveUI(w, r)
	})

	mux.HandleFunc("/api/state", g.requireAuth(func(w http.ResponseWriter, r *http.Request) {
		st, err := g.backend.GetFleetState(r.Context())
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, st)
	}))

	actorParam := func(r *http.Request, fallback string) string {
		actor := strings.TrimSpace(r.URL.Query().Get("actor"))
		if actor == "" {
			actor = strings.TrimSpace(r.URL.Query().Get("tenant"))
		}
		if actor == "" {
			return fallback
		}
		return actor
	}

	mux.HandleFunc("/api/tenant/seed", g.requireAuth(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		actor := actorParam(r, "")
		st, err := g.backend.SeedFleet(r.Context(), actor)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, st)
	}))

	mux.HandleFunc("/api/tenant/query", g.requireAuth(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost && r.Method != http.MethodGet {
			writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		actor := actorParam(r, "db-acme")
		table := strings.TrimSpace(r.URL.Query().Get("table"))
		key := strings.TrimSpace(r.URL.Query().Get("key"))
		val := r.URL.Query().Get("val")
		amountUSD, _ := strconv.Atoi(r.URL.Query().Get("amount"))

		snap, err := g.backend.QueryTenant(r.Context(), actor, table, key, val, amountUSD)
		if err != nil {
			status := http.StatusInternalServerError
			if strings.Contains(err.Error(), "404 TENANT_SHREDDED") {
				status = http.StatusNotFound
			}
			writeJSONError(w, status, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, snap)
	}))

	mux.HandleFunc("/api/tenant/pause", g.requireAuth(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		actor := actorParam(r, "db-acme")
		tr, err := g.backend.PauseTenant(r.Context(), actor)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, tr)
	}))

	mux.HandleFunc("/api/tenant/suspend", g.requireAuth(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		actor := actorParam(r, "db-acme")
		tr, err := g.backend.SuspendTenant(r.Context(), actor)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, tr)
	}))

	mux.HandleFunc("/api/tenant/gdpr-delete", g.requireAuth(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		actor := actorParam(r, "db-acme")
		receipt, err := g.backend.GDPRDeleteTenant(r.Context(), actor)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, receipt)
	}))

	mux.HandleFunc("/api/reset", g.requireAuth(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		st, err := g.backend.ResetFleet(r.Context())
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, st)
	}))

	return mux
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(payload)
}

func writeJSONError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
