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

type GatewayServer struct {
	backend HoneypotBackend
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

func NewGatewayServer(backend HoneypotBackend) *GatewayServer {
	return &GatewayServer{backend: backend}
}

func shouldAutoCheckpoint(cmd string, forensics *ForensicsResponse) (bool, string) {
	if forensics == nil {
		return false, ""
	}
	trimmed := strings.TrimSpace(cmd)
	// Never overwrite pre-wipe snapshot when attacker deletes files!
	if strings.Contains(trimmed, "rm ") || strings.HasPrefix(trimmed, "rm\t") {
		return false, ""
	}
	for _, d := range forensics.RootfsDelta {
		if d.Status == "DELETED" {
			return false, ""
		}
	}
	for _, d := range forensics.RootfsDelta {
		if d.Status == "ADDED" {
			return true, fmt.Sprintf("Auto-Checkpoint: %s planted in rootfs", d.Name)
		}
		if d.Status == "MODIFIED" {
			return true, fmt.Sprintf("Auto-Checkpoint: %s modified in rootfs", d.Name)
		}
	}
	if len(forensics.CanaryAlerts) > 0 {
		lastAlert := forensics.CanaryAlerts[len(forensics.CanaryAlerts)-1]
		if lastAlert.Rule == "DECOY_CREDENTIAL_EXFILTRATION" {
			return true, "Auto-Checkpoint: Decoy credentials accessed"
		}
	}
	return false, ""
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

	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})

	mux.HandleFunc("/api/state", func(w http.ResponseWriter, r *http.Request) {
		actorName := r.URL.Query().Get("actor")
		if actorName == "" {
			actorName = "honeypot-203-0-113-5"
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
	})

	mux.HandleFunc("/api/attacker/exec", func(w http.ResponseWriter, r *http.Request) {
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

		shellResp, forensicsResp, err := g.backend.ExecShell(r.Context(), req.ActorName, req.Cmd, req.DropFile, req.Content)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		autoSaved := false
		saveReason := ""
		if ok, reason := shouldAutoCheckpoint(req.Cmd, forensicsResp); ok {
			if _, err := g.backend.SaveActor(r.Context(), req.ActorName, reason); err == nil {
				autoSaved = true
				saveReason = reason
			}
		}

		resp := g.buildActionResponse(r, req.ActorName, autoSaved, saveReason, shellResp, forensicsResp)
		writePrettyJSON(w, http.StatusOK, resp)
	})

	mux.HandleFunc("/api/soc/save", func(w http.ResponseWriter, r *http.Request) {
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
		if req.Reason == "" {
			req.Reason = "Manual SOC Checkpoint"
		}
		if _, err := g.backend.SaveActor(r.Context(), req.ActorName, req.Reason); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		forensics, _ := g.backend.GetForensics(r.Context(), req.ActorName)
		resp := g.buildActionResponse(r, req.ActorName, true, req.Reason, nil, forensics)
		writePrettyJSON(w, http.StatusOK, resp)
	})

	mux.HandleFunc("/api/soc/suspend", func(w http.ResponseWriter, r *http.Request) {
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
		if err := g.backend.SuspendActor(r.Context(), req.ActorName); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		resp := g.buildActionResponse(r, req.ActorName, false, "Suspended to GCS ($0/hr idle compute)", nil, nil)
		writePrettyJSON(w, http.StatusOK, resp)
	})

	mux.HandleFunc("/api/soc/revert", func(w http.ResponseWriter, r *http.Request) {
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
		forensics, err := g.backend.RevertActor(r.Context(), req.ActorName, req.TargetVersion)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		resp := g.buildActionResponse(r, req.ActorName, false, "Reverted to pre-wipe snapshot", nil, forensics)
		writePrettyJSON(w, http.StatusOK, resp)
	})

	mux.HandleFunc("/api/soc/clone", func(w http.ResponseWriter, r *http.Request) {
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
		forensics, err := g.backend.CloneActor(r.Context(), req.SourceActor, req.NewActor, req.TagName)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		resp := g.buildActionResponse(r, req.NewActor, false, "Cloned live sandbox via tag "+req.TagName, nil, forensics)
		writePrettyJSON(w, http.StatusOK, resp)
	})

	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		data, err := webFS.ReadFile("web/index.html")
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(data)
	})

	return mux
}
