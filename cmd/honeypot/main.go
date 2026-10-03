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

// Command honeypot is a "Truman Show" deception sandbox.
//
// Each attacker IP or session is routed to its own gVisor-sandboxed Actor
// (e.g. `ate-target-actor: talk-demos/honeypot-203-0-113-5`). When the
// "attacker" drops files into /tmp or modifies fake configs, Substrate's Full
// snapshot preserves both the rootfs overlay (/tmp/*) and the in-memory
// forensic log across suspends — costing $0 in compute while the attacker is
// away, and waking up in ~100ms with all of their "compromised" artifacts
// intact when they return.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type ForensicEvent struct {
	Timestamp time.Time `json:"timestamp"`
	Action    string    `json:"action"`
	Detail    string    `json:"detail"`
	PodIP     string    `json:"pod_ip"`
}

type Honeypot struct {
	mu          sync.Mutex
	ScratchDir  string
	ForensicLog []ForensicEvent
}

func newHoneypot(scratchDir string) (*Honeypot, error) {
	if err := os.MkdirAll(scratchDir, 0o755); err != nil {
		return nil, err
	}
	// Seed a decoy file in the rootfs overlay.
	decoyPath := filepath.Join(scratchDir, "payroll_db_credentials.env.bak")
	if _, err := os.Stat(decoyPath); os.IsNotExist(err) {
		_ = os.WriteFile(decoyPath, []byte("DB_HOST=10.99.0.14\nDB_USER=payroll_admin\nDB_PASS=Summer2026!Decoy\n"), 0o644)
	}
	return &Honeypot{
		ScratchDir: scratchDir,
	}, nil
}

func (h *Honeypot) Interact(dropFile, content, cmd, podIP string) (map[string]any, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if dropFile != "" {
		cleanName := filepath.Base(dropFile)
		target := filepath.Join(h.ScratchDir, cleanName)
		if err := os.WriteFile(target, []byte(content), 0o644); err != nil {
			return nil, fmt.Errorf("write %s: %w", target, err)
		}
		h.ForensicLog = append(h.ForensicLog, ForensicEvent{
			Timestamp: time.Now().UTC(),
			Action:    "FILE_DROPPED_IN_ROOTFS",
			Detail:    fmt.Sprintf("wrote %d bytes to %s", len(content), target),
			PodIP:     podIP,
		})
	}

	if cmd != "" {
		h.ForensicLog = append(h.ForensicLog, ForensicEvent{
			Timestamp: time.Now().UTC(),
			Action:    "COMMAND_EXECUTED",
			Detail:    cmd,
			PodIP:     podIP,
		})
	}

	entries, err := os.ReadDir(h.ScratchDir)
	if err != nil {
		return nil, err
	}
	filesInTmp := make(map[string]string, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		b, err := os.ReadFile(filepath.Join(h.ScratchDir, entry.Name()))
		if err == nil {
			filesInTmp[entry.Name()] = string(b)
		}
	}

	return map[string]any{
		"banner":                "CorpInternal Admin Console v1.4 (gVisor Deception Sandbox)",
		"sandbox_isolation":     "SANDBOX_CLASS_GVISOR (rootfs overlay + RAM checkpointed on Suspend)",
		"scratch_directory":     h.ScratchDir,
		"files_in_rootfs_tmp":   filesInTmp,
		"forensic_audit_trail":  h.ForensicLog,
		"current_worker_pod_ip": podIP,
	}, nil
}

func newMux(hp *Honeypot) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		dropFile := r.URL.Query().Get("drop_file")
		content := r.URL.Query().Get("content")
		cmd := r.URL.Query().Get("cmd")

		resp, err := hp.Interact(dropFile, content, cmd, currentPodIP())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(resp)
	})
	return mux
}

func currentPodIP() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return "unknown"
	}
	for _, addr := range addrs {
		if ipnet, ok := addr.(*net.IPNet); ok && !ipnet.IP.IsLoopback() && ipnet.IP.To4() != nil {
			return ipnet.IP.String()
		}
	}
	return "127.0.0.1"
}

func main() {
	addr := flag.String("addr", ":80", "HTTP listen address")
	scratchDir := flag.String("scratch-dir", "/tmp/honeypot", "Rootfs directory for attacker-dropped files")
	flag.Parse()

	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	hp, err := newHoneypot(*scratchDir)
	if err != nil {
		slog.Error("Failed to initialize honeypot", slog.Any("err", err))
		os.Exit(1)
	}
	slog.Info("Starting Truman Show Honeypot server", slog.String("addr", *addr), slog.String("scratch_dir", *scratchDir))
	if err := http.ListenAndServe(*addr, newMux(hp)); err != nil {
		slog.Error("Server exited", slog.Any("err", err))
		os.Exit(1)
	}
}
