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
)

//go:embed web/*.html
var webFS embed.FS

type GatewayServer struct {
	backend TimewarpBackend
}

func NewGatewayServer(backend TimewarpBackend) *GatewayServer {
	return &GatewayServer{backend: backend}
}

func (g *GatewayServer) Mux() *http.ServeMux {
	mux := http.NewServeMux()

	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})

	serveDashboard := func(w http.ResponseWriter, _ *http.Request) {
		data, err := webFS.ReadFile("web/index.html")
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data)
	}
	mux.HandleFunc("/", serveDashboard)
	mux.HandleFunc("/dashboard", serveDashboard)

	mux.HandleFunc("/api/state", func(w http.ResponseWriter, r *http.Request) {
		st, err := g.backend.GetState(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writePrettyJSON(w, http.StatusOK, st)
	})

	mux.HandleFunc("/api/step/1-quorum", func(w http.ResponseWriter, r *http.Request) {
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
	})

	mux.HandleFunc("/api/step/2-freeze-elect", func(w http.ResponseWriter, r *http.Request) {
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
	})

	mux.HandleFunc("/api/step/3-wake-zombie", func(w http.ResponseWriter, r *http.Request) {
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
	})

	mux.HandleFunc("/api/step/4-guardrail", func(w http.ResponseWriter, r *http.Request) {
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
	})

	mux.HandleFunc("/api/guardrail", func(w http.ResponseWriter, r *http.Request) {
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
	})

	mux.HandleFunc("/api/kv/put", func(w http.ResponseWriter, r *http.Request) {
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
	})

	mux.HandleFunc("/api/reset", func(w http.ResponseWriter, r *http.Request) {
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
	})

	return mux
}
