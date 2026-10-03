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

// Command tenantdb is a single-tenant embedded database server ("One DB
// Process Per Customer") backed by a Substrate DurableDir volume (/data).
//
// It demonstrates the interplay between:
//   - DurableDir (/data/wal.jsonl): survives both DATA-scope and FULL-scope snapshots
//   - Hot RAM buffer pool & session counters: survive Pause (FULL-scope) and
//     FULL-scope Suspend, or rebuild automatically from /data on a DATA-scope Resume.
package main

import (
	"bufio"
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

type Row struct {
	Key       string    `json:"key"`
	Value     string    `json:"value"`
	UpdatedAt time.Time `json:"updated_at"`
}

type TenantDB struct {
	mu                  sync.Mutex
	DataDir             string
	WALPath             string
	ProcessBootedAt     time.Time
	RAMBufferPool       map[string]Row
	RAMQueriesSinceBoot int
	WALReplayedOnBoot   int
}

func openTenantDB(dataDir string) (*TenantDB, error) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir %s: %w", dataDir, err)
	}
	db := &TenantDB{
		DataDir:         dataDir,
		WALPath:         filepath.Join(dataDir, "wal.jsonl"),
		ProcessBootedAt: time.Now().UTC(),
		RAMBufferPool:   make(map[string]Row),
	}
	if err := db.replayWAL(); err != nil {
		return nil, err
	}
	return db, nil
}

func (db *TenantDB) replayWAL() error {
	f, err := os.Open(db.WALPath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		var r Row
		if err := json.Unmarshal(scanner.Bytes(), &r); err == nil && r.Key != "" {
			db.RAMBufferPool[r.Key] = r
			db.WALReplayedOnBoot++
		}
	}
	return scanner.Err()
}

func (db *TenantDB) Put(key, value string) error {
	db.mu.Lock()
	defer db.mu.Unlock()

	// Before mutating, if the WAL file appeared underneath us (e.g. DATA-scope restore),
	// ensure we have replayed it if RAMBufferPool is empty.
	if len(db.RAMBufferPool) == 0 {
		_ = db.replayWAL()
	}

	row := Row{
		Key:       key,
		Value:     value,
		UpdatedAt: time.Now().UTC(),
	}
	f, err := os.OpenFile(db.WALPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()

	line, err := json.Marshal(row)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		return err
	}
	db.RAMBufferPool[key] = row
	db.RAMQueriesSinceBoot++
	return nil
}

func (db *TenantDB) Snapshot(podIP string) map[string]any {
	db.mu.Lock()
	defer db.mu.Unlock()

	if len(db.RAMBufferPool) == 0 {
		_ = db.replayWAL()
	}
	db.RAMQueriesSinceBoot++

	return map[string]any{
		"engine":                  "Substrate Per-Tenant Embedded DB",
		"durable_dir_wal_path":    db.WALPath,
		"rows_in_durable_dir":     len(db.RAMBufferPool),
		"rows":                    db.RAMBufferPool,
		"ram_queries_since_boot":  db.RAMQueriesSinceBoot,
		"wal_replayed_on_boot":    db.WALReplayedOnBoot,
		"process_booted_at":       db.ProcessBootedAt.Format(time.RFC3339Nano),
		"current_pod_ip":          podIP,
		"gdpr_delete_instruction": "kubectl ate delete actor <tenant-actor> -a <atespace> --any-state",
	}
}

func newMux(db *TenantDB) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		key := r.URL.Query().Get("key")
		val := r.URL.Query().Get("val")
		if key != "" {
			if err := db.Put(key, val); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(db.Snapshot(currentPodIP()))
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
	dataDir := flag.String("data-dir", "/data", "Path to DurableDir mount")
	flag.Parse()

	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	db, err := openTenantDB(*dataDir)
	if err != nil {
		slog.Error("Failed to open tenant DB", slog.Any("err", err))
		os.Exit(1)
	}
	slog.Info("Starting Per-Tenant DB server", slog.String("addr", *addr), slog.String("data_dir", *dataDir))
	if err := http.ListenAndServe(*addr, newMux(db)); err != nil {
		slog.Error("Server exited", slog.Any("err", err))
		os.Exit(1)
	}
}
