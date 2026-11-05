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
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/moficodes/anything-but-agents/internal/presenterauth"
)

func decodeOptionalJSON(r *http.Request, dst any) error {
	if r.Body == nil {
		return nil
	}
	err := json.NewDecoder(r.Body).Decode(dst)
	if errors.Is(err, io.EOF) {
		return nil
	}
	return err
}

//go:embed web/*.html
var webFS embed.FS

const (
	// PresenterCookieName is the HttpOnly cookie set when unlocking the Honeypot UI via ?key=<PRESENTER_TOKEN>.
	PresenterCookieName = "hp_presenter"
	// PresenterHeaderName carries the presenter token for CLI scripts and tests.
	PresenterHeaderName = presenterauth.PresenterHeaderName
)

type GatewayServer struct {
	backend  HoneypotBackend
	authGate *presenterauth.Gate
}

type GatewayActionResponse struct {
	ActorName        string                 `json:"actor_name"`
	Status           string                 `json:"status"`
	SnapshotVersion  int                    `json:"snapshot_version"`
	AutoCheckpointed bool                   `json:"auto_checkpointed"`
	CheckpointReason string                 `json:"checkpoint_reason,omitempty"`
	Shell            *AttackerShellResponse `json:"shell,omitempty"`
	Forensics        *ForensicsResponse     `json:"forensics,omitempty"`
	Actors           []ActorTrackerState    `json:"actors"`
}

func NewGatewayServer(backend HoneypotBackend, presenterTokens ...string) *GatewayServer {
	var token string
	if len(presenterTokens) > 0 {
		token = strings.TrimSpace(presenterTokens[0])
	}
	return &GatewayServer{
		backend: backend,
		authGate: presenterauth.NewGate(presenterauth.GateConfig{
			Title:            "Demo 4: Truman Show Honeypot & SOC Booth",
			Subtitle:         "Sign in with an authorized Google Workspace or event account to access the per-attacker gVisor sandbox shell and SOC forensics controls.",
			SecretName:       "honeypot-gateway",
			LegacyCookieName: PresenterCookieName,
			PresenterToken:   token,
		}),
	}
}

func (g *GatewayServer) PresenterKey() string {
	return g.authGate.PresenterKey()
}

func shouldAutoCheckpoint(cmd string, prev, forensics *ForensicsResponse) (bool, string) {
	if forensics == nil {
		return false, ""
	}
	trimmed := strings.TrimSpace(cmd)
	// Never overwrite pre-wipe or pre-crash snapshot when attacker deletes files or crashes PID 1!
	if strings.Contains(trimmed, "rm ") || strings.HasPrefix(trimmed, "rm\t") || strings.Contains(trimmed, "rmdir ") || isCrashCommand(trimmed) {
		return false, ""
	}
	for _, d := range forensics.RootfsDelta {
		if strings.HasSuffix(d.Name, "/") {
			continue
		}
		if d.Status == "DELETED" {
			return false, ""
		}
	}

	prevFiles := make(map[string]FileDeltaEntry)
	prevAlerts := 0
	if prev != nil {
		for _, d := range prev.RootfsDelta {
			prevFiles[d.Name] = d
		}
		prevAlerts = len(prev.CanaryAlerts)
	}

	for _, d := range forensics.RootfsDelta {
		if strings.HasSuffix(d.Name, "/") {
			continue
		}
		old, existed := prevFiles[d.Name]
		if d.Status == "ADDED" {
			if !existed || old.Status == "DELETED" {
				return true, fmt.Sprintf("Auto-Checkpoint: %s planted in rootfs", d.Name)
			}
			if old.SHA256 != d.SHA256 {
				return true, fmt.Sprintf("Auto-Checkpoint: %s modified in rootfs", d.Name)
			}
		}
		if d.Status == "MODIFIED" && (!existed || old.SHA256 != d.SHA256) {
			return true, fmt.Sprintf("Auto-Checkpoint: %s modified in rootfs", d.Name)
		}
	}

	if len(forensics.CanaryAlerts) > prevAlerts {
		for _, alert := range forensics.CanaryAlerts[prevAlerts:] {
			if alert.Rule == "DECOY_CREDENTIAL_EXFILTRATION" {
				return true, "Auto-Checkpoint: Decoy credentials accessed"
			}
		}
	}
	return false, ""
}

func (g *GatewayServer) cachedForensics(r *http.Request, actorName string) *ForensicsResponse {
	actors, _ := g.backend.ListActors(r.Context())
	for _, a := range actors {
		if a.ActorName == actorName {
			return a.LastForensics
		}
	}
	return nil
}

func (g *GatewayServer) buildActionResponse(r *http.Request, actorName string, autoSaved bool, reason string, shell *AttackerShellResponse, forensics *ForensicsResponse) GatewayActionResponse {
	actors, _ := g.backend.ListActors(r.Context())
	status := "RUNNING"
	ver := 1
	for _, a := range actors {
		if a.ActorName == actorName {
			status = a.Status
			ver = a.SnapshotVersion
			if shell == nil {
				shell = a.LastShell
			}
			if forensics == nil {
				forensics = a.LastForensics
			}
			break
		}
	}
	return GatewayActionResponse{
		ActorName:        actorName,
		Status:           status,
		SnapshotVersion:  ver,
		AutoCheckpointed: autoSaved,
		CheckpointReason: reason,
		Shell:            shell,
		Forensics:        forensics,
		Actors:           actors,
	}
}

func (g *GatewayServer) Mux() *http.ServeMux {
	mux := http.NewServeMux()
	g.authGate.RegisterRoutes(mux)

	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})

	mux.HandleFunc("/api/state", g.authGate.RequireAPI(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		actorName := r.URL.Query().Get("actor")
		if actorName == "" {
			actorName = "honeypot-203-0-113-5"
		}
		if !isValidActorOrTagName(actorName) {
			http.Error(w, "invalid actor name", http.StatusBadRequest)
			return
		}
		wake := r.URL.Query().Get("wake") == "1"
		var forensics *ForensicsResponse
		if wake {
			forensics, _ = g.backend.GetForensics(r.Context(), actorName)
		} else {
			actors, _ := g.backend.ListActors(r.Context())
			for _, a := range actors {
				if a.ActorName == actorName {
					if a.Status == "RUNNING" && a.LastForensics == nil {
						forensics, _ = g.backend.GetForensics(r.Context(), actorName)
					} else {
						forensics = a.LastForensics
					}
					break
				}
			}
		}
		resp := g.buildActionResponse(r, actorName, false, "", nil, forensics)
		writePrettyJSON(w, http.StatusOK, resp)
	}))

	mux.HandleFunc("/api/attacker/exec", g.authGate.RequireAPI(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			ActorName string `json:"actor_name"`
			Cmd       string `json:"cmd"`
			DropFile  string `json:"drop_file"`
			Content   string `json:"content"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if req.ActorName == "" {
			req.ActorName = "honeypot-203-0-113-5"
		}
		if !isValidActorOrTagName(req.ActorName) {
			http.Error(w, "invalid actor name", http.StatusBadRequest)
			return
		}

		prevForensics := g.cachedForensics(r, req.ActorName)
		shellResp, forensicsResp, err := g.backend.ExecShell(r.Context(), req.ActorName, req.Cmd, req.DropFile, req.Content)
		if err != nil {
			status := http.StatusInternalServerError
			if strings.Contains(err.Error(), "ACTOR_STATE_CRASHED") {
				status = http.StatusConflict
			}
			http.Error(w, err.Error(), status)
			return
		}

		autoSaved := false
		saveReason := ""
		if ok, reason := shouldAutoCheckpoint(req.Cmd, prevForensics, forensicsResp); ok {
			if _, err := g.backend.SaveActor(r.Context(), req.ActorName, reason); err == nil {
				autoSaved = true
				saveReason = reason
			}
		}

		resp := g.buildActionResponse(r, req.ActorName, autoSaved, saveReason, shellResp, forensicsResp)
		writePrettyJSON(w, http.StatusOK, resp)
	}))

	mux.HandleFunc("/api/soc/crash", g.authGate.RequireAPI(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			ActorName string `json:"actor_name"`
		}
		if err := decodeOptionalJSON(r, &req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if req.ActorName == "" {
			req.ActorName = "honeypot-203-0-113-5"
		}
		if !isValidActorOrTagName(req.ActorName) {
			http.Error(w, "invalid actor name", http.StatusBadRequest)
			return
		}
		forensics, err := g.backend.CrashActor(r.Context(), req.ActorName)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		resp := g.buildActionResponse(r, req.ActorName, false, "PID 1 exited non-zero (137) -> ACTOR_STATE_CRASHED", nil, forensics)
		writePrettyJSON(w, http.StatusOK, resp)
	}))

	mux.HandleFunc("/api/soc/save", g.authGate.RequireAPI(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			ActorName string `json:"actor_name"`
			Reason    string `json:"reason"`
		}
		if err := decodeOptionalJSON(r, &req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if req.ActorName == "" {
			req.ActorName = "honeypot-203-0-113-5"
		}
		if !isValidActorOrTagName(req.ActorName) {
			http.Error(w, "invalid actor name", http.StatusBadRequest)
			return
		}
		if req.Reason == "" {
			req.Reason = "Manual SOC Checkpoint"
		}
		if _, err := g.backend.SaveActor(r.Context(), req.ActorName, req.Reason); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		forensics, _ := g.backend.GetForensics(r.Context(), actorNameOrDefault(req.ActorName))
		resp := g.buildActionResponse(r, req.ActorName, true, req.Reason, nil, forensics)
		writePrettyJSON(w, http.StatusOK, resp)
	}))

	mux.HandleFunc("/api/soc/suspend", g.authGate.RequireAPI(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			ActorName string `json:"actor_name"`
		}
		if err := decodeOptionalJSON(r, &req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if req.ActorName == "" {
			req.ActorName = "honeypot-203-0-113-5"
		}
		if !isValidActorOrTagName(req.ActorName) {
			http.Error(w, "invalid actor name", http.StatusBadRequest)
			return
		}
		if err := g.backend.SuspendActor(r.Context(), req.ActorName); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		resp := g.buildActionResponse(r, req.ActorName, false, "Suspended to GCS ($0/hr idle compute)", nil, nil)
		writePrettyJSON(w, http.StatusOK, resp)
	}))

	mux.HandleFunc("/api/soc/revert", g.authGate.RequireAPI(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			ActorName     string `json:"actor_name"`
			TargetVersion int    `json:"target_version"`
		}
		if err := decodeOptionalJSON(r, &req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if req.ActorName == "" {
			req.ActorName = "honeypot-203-0-113-5"
		}
		if !isValidActorOrTagName(req.ActorName) {
			http.Error(w, "invalid actor name", http.StatusBadRequest)
			return
		}
		forensics, err := g.backend.RevertActor(r.Context(), req.ActorName, req.TargetVersion)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		resp := g.buildActionResponse(r, req.ActorName, false, "Reverted to pre-wipe snapshot", nil, forensics)
		writePrettyJSON(w, http.StatusOK, resp)
	}))

	mux.HandleFunc("/api/soc/clone", g.authGate.RequireAPI(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			SourceActor string `json:"source_actor"`
			NewActor    string `json:"new_actor"`
			TagName     string `json:"tag_name"`
		}
		if err := decodeOptionalJSON(r, &req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if req.SourceActor == "" {
			req.SourceActor = "honeypot-203-0-113-5"
		}
		if req.NewActor == "" {
			req.NewActor = "honeypot-forensic-lab"
		}
		if req.TagName == "" {
			req.TagName = fmt.Sprintf("tag-forensic-%d", time.Now().Unix()%10000)
		}
		if !isValidActorOrTagName(req.SourceActor) || !isValidActorOrTagName(req.NewActor) || !isValidActorOrTagName(req.TagName) {
			http.Error(w, "invalid actor or tag name", http.StatusBadRequest)
			return
		}
		forensics, err := g.backend.CloneActor(r.Context(), req.SourceActor, req.NewActor, req.TagName)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		resp := g.buildActionResponse(r, req.NewActor, false, "Cloned live sandbox via tag "+req.TagName, nil, forensics)
		writePrettyJSON(w, http.StatusOK, resp)
	}))

	mux.HandleFunc("/api/soc/reset", g.authGate.RequireAPI(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			ActorName string `json:"actor_name"`
			Scope     string `json:"scope"`
		}
		if err := decodeOptionalJSON(r, &req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		scope := strings.ToLower(strings.TrimSpace(req.Scope))
		if scope == "stage" {
			forensics, err := g.backend.ResetStage(r.Context())
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			resp := g.buildActionResponse(r, defaultHoneypotActors[0], false, "Golden Snapshot Baseline Ready", nil, forensics)
			writePrettyJSON(w, http.StatusOK, resp)
			return
		}
		if req.ActorName == "" {
			req.ActorName = defaultHoneypotActors[0]
		}
		if !isValidActorOrTagName(req.ActorName) {
			http.Error(w, "invalid actor name", http.StatusBadRequest)
			return
		}
		forensics, err := g.backend.ResetActor(r.Context(), req.ActorName)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		resp := g.buildActionResponse(r, req.ActorName, false, fmt.Sprintf("Recreated %s from clean Golden Snapshot", req.ActorName), nil, forensics)
		writePrettyJSON(w, http.StatusOK, resp)
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

func actorNameOrDefault(name string) string {
	if strings.TrimSpace(name) == "" {
		return "honeypot-203-0-113-5"
	}
	return name
}
