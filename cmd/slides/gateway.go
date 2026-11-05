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
	"embed"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strings"

	"github.com/moficodes/anything-but-agents/internal/presenterauth"
)

//go:embed web/index.html
var indexHTML []byte

//go:embed web/assets/*.png
var embeddedAssets embed.FS

var allowedAssetNames = map[string]bool{
	"bg-hero.png":    true,
	"bg-section.png": true,
	"bg-content.png": true,
	"mascot.png":     true,
}

const (
	// PresenterCookieName is the HttpOnly cookie set when the presenter unlocks /deck?key=<PRESENTER_TOKEN>.
	PresenterCookieName = "slides_presenter"
	// PresenterHeaderName carries the presenter token for CLI scripts and tests.
	PresenterHeaderName = presenterauth.PresenterHeaderName
	// defaultAudienceQRURL is the public Dungeon 2D /play endpoint encoded into the stage QR modal.
	defaultAudienceQRURL = "https://dungeon.substrate.learngke.dev/play"
)

// GatewayServer serves the presentation deck web UI and proxies API calls to slide actors.
type GatewayServer struct {
	backend  SlidesBackend
	authGate *presenterauth.Gate
}

// NewGatewayServer creates a new GatewayServer.
func NewGatewayServer(backend SlidesBackend, presenterToken string) *GatewayServer {
	token := strings.TrimSpace(presenterToken)
	if token == "" {
		token = presenterauth.RandomToken(16)
	}
	return &GatewayServer{
		backend: backend,
		authGate: presenterauth.NewGate(presenterauth.GateConfig{
			Title:            "Demo 0: Slides on Substrate",
			Subtitle:         "Sign in with an authorized Google Workspace or event account to present the live 16-slide Agent Substrate Day deck and control sleeping slide actors.",
			SecretName:       "slides-gateway",
			LegacyCookieName: PresenterCookieName,
			PresenterToken:   token,
		}),
	}
}

// PresenterKey returns the configured presenter token.
func (g *GatewayServer) PresenterKey() string {
	return g.authGate.PresenterKey()
}

func (g *GatewayServer) isAuthorized(r *http.Request) bool {
	return g.authGate.IsAuthorized(r)
}

func setSecurityHeaders(w http.ResponseWriter) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "SAMEORIGIN")
	w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
	w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
}

func isSameOriginMutation(r *http.Request) bool {
	if r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions {
		return true
	}
	// Non-browser CLI callers using explicit header auth do not carry cookie ambient authority.
	if strings.TrimSpace(r.Header.Get(PresenterHeaderName)) != "" {
		return true
	}
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" {
		return true
	}
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Host == "" {
		return false
	}
	return strings.EqualFold(parsed.Host, r.Host)
}

func (g *GatewayServer) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		setSecurityHeaders(w)
		if !g.isAuthorized(r) {
			writeJSONError(w, http.StatusUnauthorized, "unauthorized: sign in with Google or provide presenter token")
			return
		}
		if !isSameOriginMutation(r) {
			writeJSONError(w, http.StatusForbidden, "forbidden: cross-origin state-changing request rejected")
			return
		}
		next(w, r)
	}
}

func (g *GatewayServer) handleDeckPage(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)
	if g.authGate.HandleUnlockOrGate(w, r, "") {
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(indexHTML)
}

func (g *GatewayServer) handleStaticAsset(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	name := path.Base(strings.TrimPrefix(r.URL.Path, "/assets/"))
	if !allowedAssetNames[name] {
		http.NotFound(w, r)
		return
	}
	raw, err := embeddedAssets.ReadFile("web/assets/" + name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(raw)
	}
}

func (g *GatewayServer) handleQRSVG(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	target := strings.TrimSpace(r.URL.Query().Get("url"))
	if target == "" {
		target = defaultAudienceQRURL
	}
	if len(target) > 104 {
		writeJSONError(w, http.StatusBadRequest, "qr url exceeds 104-byte encoder limit")
		return
	}
	parsed, err := url.Parse(target)
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" {
		writeJSONError(w, http.StatusBadRequest, "qr url must be an absolute http(s) URL")
		return
	}
	svg := RenderQRSVG(target)
	w.Header().Set("Content-Type", "image/svg+xml; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write([]byte(svg))
	}
}

// Mux builds the HTTP router for the slides gateway.
func (g *GatewayServer) Mux() *http.ServeMux {
	mux := http.NewServeMux()
	g.authGate.RegisterRoutes(mux)

	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		setSecurityHeaders(w)
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		setSecurityHeaders(w)
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})

	mux.HandleFunc("/assets/", g.handleStaticAsset)

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" && r.URL.Path != "/deck" {
			http.NotFound(w, r)
			return
		}
		g.handleDeckPage(w, r)
	})

	mux.HandleFunc("/deck", g.handleDeckPage)
	mux.HandleFunc("/api/qr.svg", g.requireAuth(g.handleQRSVG))

	mux.HandleFunc("/api/slides", g.requireAuth(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		slides, err := g.backend.ListSlides(r.Context())
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"slides":       slides,
			"is_presenter": g.isAuthorized(r),
			"total":        len(slides),
		})
	}))

	mux.HandleFunc("/api/slide/", g.requireAuth(func(w http.ResponseWriter, r *http.Request) {
		trimmed := strings.TrimPrefix(r.URL.Path, "/api/slide/")
		parts := strings.Split(trimmed, "/")
		if len(parts) == 0 || parts[0] == "" {
			writeJSONError(w, http.StatusBadRequest, "missing slide id")
			return
		}
		slideID := parts[0]

		if len(parts) == 1 {
			// GET /api/slide/{id}
			if r.Method != http.MethodGet {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			prevID := r.URL.Query().Get("prev")
			resp, err := g.backend.GetSlide(r.Context(), slideID, prevID)
			if err != nil {
				writeJSONError(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSON(w, http.StatusOK, resp)
			return
		}

		action := parts[1]
		switch action {
		case "note":
			if r.Method != http.MethodPost {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			var req struct {
				Notes string `json:"notes"`
				Note  string `json:"note"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("invalid json: %v", err))
				return
			}
			note := req.Notes
			if note == "" {
				note = req.Note
			}
			resp, err := g.backend.UpdateSlideNote(r.Context(), slideID, note)
			if err != nil {
				writeJSONError(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSON(w, http.StatusOK, resp)

		case "revert":
			if r.Method != http.MethodPost {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			resp, err := g.backend.RevertSlide(r.Context(), slideID)
			if err != nil {
				writeJSONError(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSON(w, http.StatusOK, resp)

		case "suspend":
			if r.Method != http.MethodPost {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			if err := g.backend.SuspendSlide(r.Context(), slideID); err != nil {
				writeJSONError(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{
				"actor_name": slideID,
				"status":     "suspended",
			})

		default:
			http.NotFound(w, r)
		}
	}))

	return mux
}

func writeJSONError(w http.ResponseWriter, status int, msg string) {
	setSecurityHeaders(w)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(map[string]any{
		"error":  msg,
		"status": status,
	})
}
