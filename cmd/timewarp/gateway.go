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
	"net/http"
	"strings"

	"github.com/moficodes/anything-but-agents/internal/presenterauth"
)

//go:embed web/*.html
var webFS embed.FS

const (
	// PresenterCookieName is the HttpOnly cookie set when unlocking the Time-Warp Lab via ?key=<PRESENTER_TOKEN>.
	PresenterCookieName = "tw_presenter"
	// PresenterHeaderName carries the presenter token for CLI scripts and tests.
	PresenterHeaderName = presenterauth.PresenterHeaderName
)

type GatewayServer struct {
	backend  TimewarpBackend
	authGate *presenterauth.Gate
}

func NewGatewayServer(backend TimewarpBackend, presenterTokens ...string) *GatewayServer {
	var token string
	if len(presenterTokens) > 0 {
		token = strings.TrimSpace(presenterTokens[0])
	}
	return &GatewayServer{
		backend: backend,
		authGate: presenterauth.NewGate(presenterauth.GateConfig{
			Title:            "Demo 5: Chronos Raft Split-Brain & Dual-Clock Lab",
			Subtitle:         "Sign in with an authorized Google Workspace or event account to control the 3-node consensus actors and Wakeup Clock-Jump Guardrail.",
			SecretName:       "timewarp-gateway",
			LegacyCookieName: PresenterCookieName,
			PresenterToken:   token,
		}),
	}
}

func (g *GatewayServer) PresenterKey() string {
	return g.authGate.PresenterKey()
}

func (g *GatewayServer) Mux() *http.ServeMux {
	mux := http.NewServeMux()
	g.authGate.RegisterRoutes(mux)

	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})

	serveDashboard := func(w http.ResponseWriter, r *http.Request) {
		if g.authGate.HandleUnlockOrGate(w, r, "/dashboard") {
			return
		}
		data, err := webFS.ReadFile("web/index.html")
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data)
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		serveDashboard(w, r)
	})
	mux.HandleFunc("/dashboard", serveDashboard)

	mux.HandleFunc("/api/state", g.authGate.RequireAPI(func(w http.ResponseWriter, r *http.Request) {
		st, err := g.backend.GetState(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writePrettyJSON(w, http.StatusOK, st)
	}))

	mux.HandleFunc("/api/step/1-quorum", g.authGate.RequireAPI(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		st, err := g.backend.Step1Quorum(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writePrettyJSON(w, http.StatusOK, st)
	}))

	mux.HandleFunc("/api/step/2-freeze-elect", g.authGate.RequireAPI(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var (
			st  *ClusterLabState
			err error
		)
		if q := r.URL.Query().Get("async"); q == "1" || strings.EqualFold(q, "true") {
			st, err = g.backend.Step2FreezeLeaderAsync(r.Context())
		} else {
			st, err = g.backend.Step2FreezeAndElect(r.Context())
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writePrettyJSON(w, http.StatusOK, st)
	}))

	mux.HandleFunc("/api/step/3-wake-zombie", g.authGate.RequireAPI(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		st, err := g.backend.Step3WakeZombie(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writePrettyJSON(w, http.StatusOK, st)
	}))

	mux.HandleFunc("/api/step/4-guardrail", g.authGate.RequireAPI(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		st, err := g.backend.Step4ReplayGuardrail(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writePrettyJSON(w, http.StatusOK, st)
	}))

	mux.HandleFunc("/api/guardrail", g.authGate.RequireAPI(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			Guard *bool `json:"guard"`
		}
		_ = decodeOptionalJSON(r, &req)
		guard := false
		if req.Guard != nil {
			guard = *req.Guard
		} else if q := r.URL.Query().Get("guard"); q != "" {
			guard = q == "1" || strings.EqualFold(q, "true")
		}
		st, err := g.backend.SetGuardrail(r.Context(), guard)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writePrettyJSON(w, http.StatusOK, st)
	}))

	mux.HandleFunc("/api/kv/put", g.authGate.RequireAPI(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			TargetNode string `json:"target_node"`
			Key        string `json:"key"`
			Value      string `json:"value"`
		}
		_ = decodeOptionalJSON(r, &req)
		if q := r.URL.Query().Get("target_node"); q != "" {
			req.TargetNode = q
		}
		if q := r.URL.Query().Get("key"); q != "" {
			req.Key = q
		}
		if q := r.URL.Query().Get("value"); q != "" {
			req.Value = q
		}
		st, code, err := g.backend.PutKV(r.Context(), req.TargetNode, req.Key, req.Value)
		if err != nil && code != http.StatusConflict {
			http.Error(w, err.Error(), code)
			return
		}
		writePrettyJSON(w, code, st)
	}))

	mux.HandleFunc("/api/reset", g.authGate.RequireAPI(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		st, err := g.backend.Reset(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writePrettyJSON(w, http.StatusOK, st)
	}))

	return mux
}
