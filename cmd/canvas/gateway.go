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
	"io"
	"net/http"
	"strings"

	"github.com/moficodes/anything-but-agents/internal/presenterauth"
)

//go:embed web/index.html
var webFS embed.FS

const (
	presenterCookieName = "canvas_presenter"
)

// GatewayServer is the HTTP presenter gateway and OpenFGA perimeter router for Demo 8.
type GatewayServer struct {
	backend  CanvasBackend
	authGate *presenterauth.Gate
}

// NewGatewayServer creates a new GatewayServer instance.
func NewGatewayServer(backend CanvasBackend, presenterToken string) *GatewayServer {
	return &GatewayServer{
		backend: backend,
		authGate: presenterauth.NewGate(presenterauth.GateConfig{
			Title:            "Demo 8: Canvas Collaborative Document Rooms",
			Subtitle:         "Multiplayer Architecture Canvas & Document Rooms • OpenFGA AccessPolicy Relationship Authorization",
			SecretName:       "canvas-gateway",
			LegacyCookieName: presenterCookieName,
			PresenterToken:   strings.TrimSpace(presenterToken),
		}),
	}
}

// PresenterKey returns the presenter key from the auth gate.
func (g *GatewayServer) PresenterKey() string {
	return g.authGate.PresenterKey()
}

// Mux constructs the HTTP handler mux for the gateway.
func (g *GatewayServer) Mux() *http.ServeMux {
	mux := http.NewServeMux()
	g.authGate.RegisterRoutes(mux)

	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})

	// Serve the interactive Web UI
	serveWeb := func(w http.ResponseWriter, r *http.Request) {
		if g.authGate.HandleUnlockOrGate(w, r, "/dashboard") {
			return
		}
		content, err := webFS.ReadFile("web/index.html")
		if err != nil {
			http.Error(w, "web dashboard asset not found", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "SAMEORIGIN")
		w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(content)
	}

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" && r.URL.Path != "/dashboard" {
			http.NotFound(w, r)
			return
		}
		serveWeb(w, r)
	})
	mux.HandleFunc("/dashboard", serveWeb)

	// API: Get canvas state with OpenFGA perimeter check
	mux.HandleFunc("/api/state", g.authGate.RequireAPI(func(w http.ResponseWriter, r *http.Request) {
		principal := r.URL.Query().Get("principal")
		if principal == "" {
			principal = PrincipalAlice
		}
		docID := r.URL.Query().Get("doc")
		if docID == "" {
			docID = DocMergerAcquisition
		}

		ctx := r.Context()
		if r.URL.Query().Get("wake") == "false" {
			ctx = withNoWake(ctx)
		}

		resp, err := g.backend.GetCanvasState(ctx, principal, docID)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}

		if resp.IsDeniedBeforeWakeup {
			// Return HTTP 403 with the perimeter denial details
			writeJSON(w, http.StatusForbidden, resp)
			return
		}

		writeJSON(w, http.StatusOK, resp)
	}))

	// API: Mutate canvas
	mux.HandleFunc("/api/mutate", g.authGate.RequireAPI(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var req struct {
			DocID     string           `json:"doc_id"`
			Principal string           `json:"principal"`
			Op        string           `json:"op"`
			Node      *CanvasNode      `json:"node,omitempty"`
			Move      *MoveNodePayload `json:"move,omitempty"`
			Note      *CanvasNote      `json:"note,omitempty"`
			Edge      *CanvasEdge      `json:"edge,omitempty"`
			Delete    *DeletePayload   `json:"delete,omitempty"`
		}

		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("invalid json: %v", err)})
			return
		}

		if req.DocID == "" {
			req.DocID = DocMergerAcquisition
		}
		if req.Principal == "" {
			req.Principal = PrincipalAlice
		}

		op := CanvasOp{
			Op:        req.Op,
			Principal: req.Principal,
			Node:      req.Node,
			Move:      req.Move,
			Note:      req.Note,
			Edge:      req.Edge,
			Delete:    req.Delete,
		}

		resp, err := g.backend.MutateCanvas(r.Context(), req.Principal, req.DocID, op)
		if err != nil {
			if strings.Contains(strings.ToLower(err.Error()), "permission denied") {
				if resp != nil {
					writeJSON(w, http.StatusForbidden, resp)
				} else {
					writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
				}
				return
			}
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}

		writeJSON(w, http.StatusOK, resp)
	}))

	// API: Grant OpenFGA relationship role
	mux.HandleFunc("/api/policy/grant", g.authGate.RequireAPI(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var req struct {
			DocID      string `json:"doc_id"`
			Principal  string `json:"principal"`
			TargetUser string `json:"target_user"`
			Role       string `json:"role"`
		}

		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}

		entry, err := g.backend.GrantRole(r.Context(), req.Principal, req.TargetUser, req.Role, req.DocID)
		if err != nil {
			if strings.Contains(strings.ToLower(err.Error()), "permission denied") {
				writeJSON(w, http.StatusForbidden, entry)
				return
			}
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}

		writeJSON(w, http.StatusOK, entry)
	}))

	// API: Revoke OpenFGA relationship role
	mux.HandleFunc("/api/policy/revoke", g.authGate.RequireAPI(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var req struct {
			DocID      string `json:"doc_id"`
			Principal  string `json:"principal"`
			TargetUser string `json:"target_user"`
		}

		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}

		entry, err := g.backend.RevokeRole(r.Context(), req.Principal, req.TargetUser, req.DocID)
		if err != nil {
			if strings.Contains(strings.ToLower(err.Error()), "permission denied") {
				writeJSON(w, http.StatusForbidden, entry)
				return
			}
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}

		writeJSON(w, http.StatusOK, entry)
	}))

	// API: Suspend document actor to GCS
	mux.HandleFunc("/api/actor/suspend", g.authGate.RequireAPI(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var req struct {
			DocID     string `json:"doc_id"`
			Principal string `json:"principal"`
		}

		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.DocID == "" {
			req.DocID = DocMergerAcquisition
		}
		if req.Principal == "" {
			req.Principal = PrincipalAlice
		}

		entry, err := g.backend.SuspendDocument(r.Context(), req.Principal, req.DocID)
		if err != nil {
			if strings.Contains(strings.ToLower(err.Error()), "permission denied") {
				writeJSON(w, http.StatusForbidden, entry)
				return
			}
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}

		writeJSON(w, http.StatusOK, entry)
	}))

	// API: Wake document actor
	mux.HandleFunc("/api/actor/wake", g.authGate.RequireAPI(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var req struct {
			DocID     string `json:"doc_id"`
			Principal string `json:"principal"`
		}

		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.DocID == "" {
			req.DocID = DocMergerAcquisition
		}
		if req.Principal == "" {
			req.Principal = PrincipalAlice
		}

		resp, err := g.backend.WakeDocument(r.Context(), req.Principal, req.DocID)
		if err != nil {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
			return
		}

		writeJSON(w, http.StatusOK, resp)
	}))

	// API: Reset demo
	mux.HandleFunc("/api/reset", g.authGate.RequireAPI(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		if err := g.backend.ResetDemo(r.Context()); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}

		writeJSON(w, http.StatusOK, map[string]string{
			"status":  "reset_complete",
			"message": "Demo reset: default catalog loaded, OpenFGA AccessPolicy restored, metrics cleared",
		})
	}))

	// API: Get live OpenFGA AccessPolicy YAML
	mux.HandleFunc("/api/policy/yaml", g.authGate.RequireAPI(func(w http.ResponseWriter, r *http.Request) {
		docID := r.URL.Query().Get("doc")
		if docID == "" {
			docID = DocMergerAcquisition
		}

		yamlStr, err := g.backend.GetPolicyYAML(r.Context(), docID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "text/yaml; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, yamlStr)
	}))

	return mux
}
