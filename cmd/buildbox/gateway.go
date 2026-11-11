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
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/moficodes/anything-but-agents/internal/presenterauth"
)

//go:embed web/*.html
var webFS embed.FS

const (
	PresenterCookieName = "bb_presenter"
	PresenterHeaderName = presenterauth.PresenterHeaderName
)

type PublishedReleaseRecord struct {
	ReleaseID       string               `json:"release_id"`
	JobName         string               `json:"job_name"`
	BinaryDigest    string               `json:"binary_digest"`
	GitCommit       string               `json:"git_commit"`
	ReleaseURI      string               `json:"release_uri"`
	Signature       string               `json:"signature"`
	AirgapVerified  bool                 `json:"airgap_verified"`
	PublishedAt     time.Time            `json:"published_at"`
	SLSALevel       string               `json:"slsa_level"`
	ArtifactDetails *BuildArtifactRecord `json:"artifact_details,omitempty"`
}

type GatewayServer struct {
	backend       BuildboxBackend
	authGate      *presenterauth.Gate
	mu            sync.Mutex
	registryStore []PublishedReleaseRecord
}

func NewGatewayServer(backend BuildboxBackend, presenterTokens ...string) *GatewayServer {
	var token string
	if len(presenterTokens) > 0 {
		token = strings.TrimSpace(presenterTokens[0])
	}
	return &GatewayServer{
		backend: backend,
		authGate: presenterauth.NewGate(presenterauth.GateConfig{
			Title:            "Demo 6: The 3-Stage Hermetic CI/CD Build Pipeline",
			Subtitle:         "Sign in with an authorized Google Workspace or event account to inspect dynamic ActorEgressPolicy hot-swapping and SLSA L3 airgap verification.",
			SecretName:       "buildbox-gateway",
			LegacyCookieName: PresenterCookieName,
			PresenterToken:   token,
		}),
		registryStore: make([]PublishedReleaseRecord, 0),
	}
}

func (g *GatewayServer) PresenterKey() string {
	return g.authGate.PresenterKey()
}

func writePrettyJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func decodeOptionalJSON(r *http.Request, target any) error {
	if r.Body == nil || r.ContentLength == 0 {
		return nil
	}
	defer r.Body.Close()
	return json.NewDecoder(r.Body).Decode(target)
}

func (g *GatewayServer) Mux() *http.ServeMux {
	mux := http.NewServeMux()
	g.authGate.RegisterRoutes(mux)

	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})

	// --- Hosted Upstream Mock Endpoints (Package Mirror & Artifact Registry) ---

	// GET /api/mirror/pkgs/{pkg}
	mux.HandleFunc("/api/mirror/pkgs/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		pkgName := strings.TrimPrefix(r.URL.Path, "/api/mirror/pkgs/")
		if pkgName == "" {
			http.Error(w, "package name required", http.StatusBadRequest)
			return
		}

		// Mock package repository metadata
		var pkgInfo map[string]any
		switch pkgName {
		case "core":
			pkgInfo = map[string]any{
				"package":    "pkg/core",
				"version":    "v1.4.2",
				"size_bytes": 14889728,
				"checksum":   "sha256:7a81c4e9b9c0d1e2f3a4b5c6d7e8f9a0b1c2d3e4f5a6b7c8d9e0f1a2b3c4d5e6",
				"license":    "Apache-2.0",
				"files":      []string{"core.a", "runtime.o", "symbols.tbl"},
			}
		case "crypto":
			pkgInfo = map[string]any{
				"package":    "pkg/crypto",
				"version":    "v2.1.0",
				"size_bytes": 8388608,
				"checksum":   "sha256:3b94a8f1e2d3c4b5a6f7e8d9c0b1a2f3e4d5c6b7a8f9e0d1c2b3a4f5e6d7c8b9",
				"license":    "BSD-3-Clause",
				"files":      []string{"crypto.a", "ciphers.o"},
			}
		case "transport":
			pkgInfo = map[string]any{
				"package":    "pkg/transport",
				"version":    "v0.9.1",
				"size_bytes": 11534336,
				"checksum":   "sha256:9f8e7d6c5b4a3f2e1d0c9b8a7f6e5d4c3b2a1f0e9d8c7b6a5f4e3d2c1b0a9f8e",
				"license":    "MIT",
				"files":      []string{"transport.a", "h2c.o"},
			}
		case "backdoor-payload":
			// Unauthorized rogue download attempted during hermetic compile
			w.Header().Set("X-Egress-Status", "ROGUE_PROBE_CAUGHT")
			http.Error(w, "Forbidden: unauthorized package download attempted", http.StatusForbidden)
			return
		default:
			pkgInfo = map[string]any{
				"package":    "pkg/" + pkgName,
				"version":    "v1.0.0",
				"size_bytes": 4194304,
				"checksum":   "sha256:11223344556677889900aabbccddeeff11223344556677889900aabbccddeeff",
				"license":    "Apache-2.0",
			}
		}

		writePrettyJSON(w, http.StatusOK, pkgInfo)
	})

	// POST /api/registry/releases
	mux.HandleFunc("/api/registry/releases", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			g.mu.Lock()
			releases := slices.Clone(g.registryStore)
			g.mu.Unlock()
			writePrettyJSON(w, http.StatusOK, releases)
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var artifact BuildArtifactRecord
		if err := json.NewDecoder(r.Body).Decode(&artifact); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		if artifact.JobName == "" || artifact.BinaryDigest == "" {
			http.Error(w, "job_name and binary_digest required", http.StatusBadRequest)
			return
		}

		releaseID := fmt.Sprintf("rel-%s-%d", artifact.JobName, time.Now().Unix())
		releaseURI := fmt.Sprintf("us-docker.pkg.dev/internal-repo/releases/%s@%s", artifact.JobName, artifact.BinaryDigest)

		record := PublishedReleaseRecord{
			ReleaseID:       releaseID,
			JobName:         artifact.JobName,
			BinaryDigest:    artifact.BinaryDigest,
			GitCommit:       artifact.GitCommit,
			ReleaseURI:      releaseURI,
			Signature:       artifact.Signature,
			AirgapVerified:  artifact.AirgapVerified,
			PublishedAt:     time.Now().UTC(),
			SLSALevel:       "SLSA_BUILD_LEVEL_3",
			ArtifactDetails: &artifact,
		}

		g.mu.Lock()
		g.registryStore = append(g.registryStore, record)
		g.mu.Unlock()

		writePrettyJSON(w, http.StatusCreated, map[string]any{
			"status":       "PUBLISHED",
			"release_id":   releaseID,
			"release_uri":  releaseURI,
			"published_at": record.PublishedAt,
			"slsa_level":   "SLSA_BUILD_LEVEL_3",
			"airgap_proof": "Verified by 0-rule ActorEgressPolicy and 3 rejected exfiltration attempts",
		})
	})

	// --- Presenter-Gated Buildbox Control Plane APIs ---

	mux.HandleFunc("/api/state", g.authGate.RequireAPI(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		jobName := r.URL.Query().Get("job")
		if jobName == "" {
			jobName = "build-pr-104"
		}
		wake := r.URL.Query().Get("wake") == "1"
		state, err := g.backend.GetJobState(r.Context(), jobName, wake)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writePrettyJSON(w, http.StatusOK, state)
	}))

	mux.HandleFunc("/api/jobs", g.authGate.RequireAPI(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		jobs, err := g.backend.ListJobs(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writePrettyJSON(w, http.StatusOK, jobs)
	}))

	mux.HandleFunc("/api/build/fetch-deps", g.authGate.RequireAPI(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			JobName string `json:"job_name"`
		}
		_ = decodeOptionalJSON(r, &req)
		if req.JobName == "" {
			req.JobName = "build-pr-104"
		}
		state, err := g.backend.FetchDeps(r.Context(), req.JobName)
		if err != nil {
			writePrettyJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error(), "state": state})
			return
		}
		writePrettyJSON(w, http.StatusOK, state)
	}))

	mux.HandleFunc("/api/build/compile-test", g.authGate.RequireAPI(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			JobName string `json:"job_name"`
		}
		_ = decodeOptionalJSON(r, &req)
		if req.JobName == "" {
			req.JobName = "build-pr-104"
		}
		state, err := g.backend.CompileAndTest(r.Context(), req.JobName)
		if err != nil {
			writePrettyJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error(), "state": state})
			return
		}
		writePrettyJSON(w, http.StatusOK, state)
	}))

	mux.HandleFunc("/api/build/test-rogue", g.authGate.RequireAPI(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			JobName string `json:"job_name"`
		}
		_ = decodeOptionalJSON(r, &req)
		if req.JobName == "" {
			req.JobName = "build-pr-104"
		}
		state, err := g.backend.TestRogueEgress(r.Context(), req.JobName)
		if err != nil {
			writePrettyJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error(), "state": state})
			return
		}
		writePrettyJSON(w, http.StatusOK, state)
	}))

	mux.HandleFunc("/api/build/publish", g.authGate.RequireAPI(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			JobName string `json:"job_name"`
		}
		_ = decodeOptionalJSON(r, &req)
		if req.JobName == "" {
			req.JobName = "build-pr-104"
		}
		state, err := g.backend.PublishArtifact(r.Context(), req.JobName)
		if err != nil {
			writePrettyJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error(), "state": state})
			return
		}
		writePrettyJSON(w, http.StatusOK, state)
	}))

	mux.HandleFunc("/api/build/autorun", g.authGate.RequireAPI(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			JobName string `json:"job_name"`
		}
		_ = decodeOptionalJSON(r, &req)
		if req.JobName == "" {
			req.JobName = "build-pr-104"
		}
		state, err := g.backend.AutoRunPipeline(r.Context(), req.JobName)
		if err != nil {
			writePrettyJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error(), "state": state})
			return
		}
		writePrettyJSON(w, http.StatusOK, state)
	}))

	mux.HandleFunc("/api/build/suspend", g.authGate.RequireAPI(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			JobName string `json:"job_name"`
		}
		_ = decodeOptionalJSON(r, &req)
		if req.JobName == "" {
			req.JobName = "build-pr-104"
		}
		if err := g.backend.SuspendActor(r.Context(), req.JobName); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		state, _ := g.backend.GetJobState(r.Context(), req.JobName, false)
		writePrettyJSON(w, http.StatusOK, state)
	}))

	mux.HandleFunc("/api/build/reset", g.authGate.RequireAPI(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			JobName string `json:"job_name"`
		}
		_ = decodeOptionalJSON(r, &req)
		if req.JobName == "" {
			req.JobName = "build-pr-104"
		}
		state, err := g.backend.ResetJob(r.Context(), req.JobName)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writePrettyJSON(w, http.StatusOK, state)
	}))

	mux.HandleFunc("/api/build/reset-stage", g.authGate.RequireAPI(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		state, err := g.backend.ResetStage(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writePrettyJSON(w, http.StatusOK, state)
	}))

	mux.HandleFunc("/api/build/egress-policy", g.authGate.RequireAPI(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		jobName := r.URL.Query().Get("job")
		if jobName == "" {
			jobName = "build-pr-104"
		}
		yamlStr, err := g.backend.GetEgressPolicyYAML(r.Context(), jobName)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writePrettyJSON(w, http.StatusOK, map[string]any{
			"job_name": jobName,
			"yaml":     yamlStr,
		})
	}))

	serveUI := func(w http.ResponseWriter, r *http.Request) {
		if g.authGate.HandleUnlockOrGate(w, r, "/") {
			return
		}
		data, err := webFS.ReadFile("web/index.html")
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "SAMEORIGIN")
		w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		_, _ = w.Write(data)
	}

	mux.HandleFunc("/dashboard", serveUI)
	mux.HandleFunc("/", serveUI)

	return mux
}
