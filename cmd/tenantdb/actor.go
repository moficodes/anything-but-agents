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
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

const defaultBufferPoolBytes = 16 * 1024 * 1024 // 16.0 MB hot buffer pool slab

type Row struct {
	Seq       int       `json:"seq"`
	Table     string    `json:"table"`
	Key       string    `json:"key"`
	Value     string    `json:"value"`
	AmountUSD int       `json:"amount_usd,omitempty"`
	PodIP     string    `json:"pod_ip,omitempty"`
	SHA256    string    `json:"sha256"`
	UpdatedAt time.Time `json:"updated_at"`
}

type MaterializedStats struct {
	TotalRows           int            `json:"total_rows"`
	InvoiceCount        int            `json:"invoice_count"`
	TotalRevenueUSD     int            `json:"total_revenue_usd"`
	Tables              map[string]int `json:"tables"`
	BufferPoolPagesWarm int            `json:"buffer_pool_pages_warm"`
	BufferPoolBytes     int            `json:"buffer_pool_bytes"`
	Checksum            string         `json:"checksum"`
}

type TenantSnapshot struct {
	Engine                string            `json:"engine"`
	QueryExecuted         string            `json:"query_executed"`
	RecoveryMode          string            `json:"recovery_mode"`
	RecoveryExplanation   string            `json:"recovery_explanation"`
	RAMBufferPoolStatus   string            `json:"ram_buffer_pool_status"`
	RAMBufferPoolBytes    int               `json:"ram_buffer_pool_bytes"`
	RAMBufferPoolMB       float64           `json:"ram_buffer_pool_mb"`
	DurableWALBytes       int64             `json:"durable_wal_bytes"`
	GCSSnapshotSavingsPct float64           `json:"gcs_snapshot_savings_pct"`
	DurableDirWALPath     string            `json:"durable_dir_wal_path"`
	RowsInDurableDir      int               `json:"rows_in_durable_dir"`
	Rows                  map[string]Row    `json:"rows"`
	WALEntries            []Row             `json:"wal_entries"`
	MaterializedStats     MaterializedStats `json:"materialized_stats"`
	RAMQueriesSinceBoot   int               `json:"ram_queries_since_boot"`
	WALReplayedOnBoot     int               `json:"wal_replayed_on_boot"`
	ProcessBootedAt       string            `json:"process_booted_at"`
	CurrentPodIP          string            `json:"current_pod_ip"`
	PodIPHistory          []string          `json:"pod_ip_history"`
	GDPRDeleteInstruction string            `json:"gdpr_delete_instruction"`
}

type TenantDB struct {
	mu                  sync.Mutex
	DataDir             string
	WALPath             string
	ProcessBootedAt     time.Time
	BufferPoolTargetB   int
	RAMBufferPoolSlab   []byte
	RAMBufferPool       map[string]Row
	WALEntries          []Row
	PodIPHistory        []string
	RAMQueriesSinceBoot int
	WALReplayedOnBoot   int
	LastRecoveryMode    string
	RAMBufferPoolStatus string
}

func openTenantDB(dataDir string) (*TenantDB, error) {
	return openTenantDBWithConfig(dataDir, defaultBufferPoolBytes)
}

func openTenantDBWithConfig(dataDir string, bufferPoolBytes int) (*TenantDB, error) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir %s: %w", dataDir, err)
	}
	if bufferPoolBytes <= 0 {
		bufferPoolBytes = defaultBufferPoolBytes
	}
	db := &TenantDB{
		DataDir:           dataDir,
		WALPath:           filepath.Join(dataDir, "wal.jsonl"),
		ProcessBootedAt:   time.Now().UTC(),
		BufferPoolTargetB: bufferPoolBytes,
		RAMBufferPool:     make(map[string]Row),
	}
	// If /data/wal.jsonl already has entries at startup (e.g., cold start after DATA-scope commit),
	// replay it immediately and hydrate the RAM buffer pool. If /data/wal.jsonl does not exist yet
	// (e.g., Golden Snapshot creation before /readyz), keep RAMBufferPoolSlab nil so the Golden
	// Snapshot has 0 dirty buffer pool pages.
	if err := db.replayWAL(); err != nil {
		return nil, err
	}
	if db.WALReplayedOnBoot > 0 {
		db.ensureBufferPoolLocked()
		db.LastRecoveryMode = "COLD_DATA_REPLAY"
		db.RAMBufferPoolStatus = "REHYDRATED_FROM_WAL"
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

	if db.RAMBufferPool == nil {
		db.RAMBufferPool = make(map[string]Row)
	}
	db.WALEntries = nil
	db.WALReplayedOnBoot = 0

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		var r Row
		if err := json.Unmarshal(scanner.Bytes(), &r); err == nil && r.Key != "" {
			if r.Table == "" {
				r.Table = "kv"
			}
			if r.Seq == 0 {
				r.Seq = len(db.WALEntries) + 1
			}
			if r.SHA256 == "" {
				r.SHA256 = computeRowHash(r.Table, r.Key, r.Value, r.AmountUSD)
			}
			db.RAMBufferPool[r.Key] = r
			db.WALEntries = append(db.WALEntries, r)
			db.recordPodIPLocked(r.PodIP)
			db.WALReplayedOnBoot++
		}
	}
	return scanner.Err()
}

func computeRowHash(table, key, value string, amountUSD int) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s|%s|%s|%d", table, key, value, amountUSD)))
	return hex.EncodeToString(sum[:8])
}

func (db *TenantDB) recordPodIPLocked(podIP string) {
	podIP = strings.TrimSpace(podIP)
	if podIP == "" || podIP == "unknown" {
		return
	}
	if !slices.Contains(db.PodIPHistory, podIP) {
		db.PodIPHistory = append(db.PodIPHistory, podIP)
	}
}

func (db *TenantDB) ensureBufferPoolLocked() {
	target := db.BufferPoolTargetB
	if target <= 0 {
		target = defaultBufferPoolBytes
	}
	if len(db.RAMBufferPoolSlab) != target {
		db.RAMBufferPoolSlab = make([]byte, target)
		// Touch one byte per 4KB page so the buffer pool represents real dirty heap pages.
		for offset := 0; offset < len(db.RAMBufferPoolSlab); offset += 4096 {
			db.RAMBufferPoolSlab[offset] = byte((offset / 4096) % 251)
		}
	}
}

// hydrateIfNeededLocked checks whether the container just woke from the Golden Snapshot
// with a populated /data/wal.jsonl (DATA-scope restore) or is continuing with a warm heap.
func (db *TenantDB) hydrateIfNeededLocked() {
	if len(db.RAMBufferPool) == 0 && len(db.WALEntries) == 0 {
		_ = db.replayWAL()
		db.ensureBufferPoolLocked()
		if db.WALReplayedOnBoot > 0 {
			db.LastRecoveryMode = "COLD_DATA_REPLAY"
			db.RAMBufferPoolStatus = "REHYDRATED_FROM_WAL"
			return
		}
		db.LastRecoveryMode = "FRESH_BOOT"
		db.RAMBufferPoolStatus = "HOT_INTACT"
		return
	}

	db.ensureBufferPoolLocked()
	if db.RAMQueriesSinceBoot == 0 && db.WALReplayedOnBoot > 0 {
		db.LastRecoveryMode = "COLD_DATA_REPLAY"
		db.RAMBufferPoolStatus = "REHYDRATED_FROM_WAL"
		return
	}
	if db.RAMQueriesSinceBoot >= 1 {
		db.LastRecoveryMode = "HOT_RAM_RESUME"
		db.RAMBufferPoolStatus = "HOT_INTACT"
	}
}

// DiscardRAMForDataScope resets in-memory state back to the pre-wakeup Golden Snapshot
// while leaving /data/wal.jsonl untouched on disk.
func (db *TenantDB) DiscardRAMForDataScope() {
	db.mu.Lock()
	defer db.mu.Unlock()

	db.RAMBufferPoolSlab = nil
	db.RAMBufferPool = make(map[string]Row)
	db.WALEntries = nil
	db.RAMQueriesSinceBoot = 0
	db.WALReplayedOnBoot = 0
	db.LastRecoveryMode = ""
	db.RAMBufferPoolStatus = "DISCARDED_ON_DATA_COMMIT"
}

func (db *TenantDB) Put(key, value string) error {
	return db.PutRow(inferTableForKey(key), key, value, inferAmountForKeyVal(key, value), currentPodIP())
}

func inferTableForKey(key string) string {
	k := strings.ToLower(key)
	switch {
	case strings.HasPrefix(k, "inv") || strings.Contains(k, "invoice") || strings.Contains(k, "order"):
		return "invoices"
	case strings.Contains(k, "tier") || strings.Contains(k, "plan") || strings.Contains(k, "seat") || strings.Contains(k, "arr"):
		return "subscriptions"
	case strings.Contains(k, "region") || strings.Contains(k, "compliance") || strings.Contains(k, "kms"):
		return "settings"
	default:
		return "kv"
	}
}

func inferAmountForKeyVal(key, value string) int {
	k := strings.ToLower(key)
	v := strings.ToLower(value)
	if n, err := strconv.Atoi(strings.TrimPrefix(v, "$")); err == nil && (strings.Contains(k, "inv") || strings.Contains(k, "arr") || strings.Contains(k, "amount")) {
		return n
	}
	if strings.Contains(k, "tier") && strings.Contains(v, "enterprise") {
		return 48000
	}
	return 0
}

func (db *TenantDB) PutRow(table, key, value string, amountUSD int, podIP string) error {
	db.mu.Lock()
	defer db.mu.Unlock()

	db.hydrateIfNeededLocked()
	if table == "" {
		table = inferTableForKey(key)
	}
	if podIP == "" {
		podIP = currentPodIP()
	}
	db.recordPodIPLocked(podIP)

	row := Row{
		Seq:       len(db.WALEntries) + 1,
		Table:     table,
		Key:       key,
		Value:     value,
		AmountUSD: amountUSD,
		PodIP:     podIP,
		SHA256:    computeRowHash(table, key, value, amountUSD),
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
	db.WALEntries = append(db.WALEntries, row)
	db.RAMQueriesSinceBoot++
	if db.RAMQueriesSinceBoot > 1 && db.LastRecoveryMode != "COLD_DATA_REPLAY" {
		db.LastRecoveryMode = "HOT_RAM_RESUME"
		db.RAMBufferPoolStatus = "HOT_INTACT"
	}
	return nil
}

func (db *TenantDB) computeMaterializedStatsLocked() MaterializedStats {
	tables := make(map[string]int)
	totalRev := 0
	invCount := 0
	var hashInput strings.Builder

	keys := make([]string, 0, len(db.RAMBufferPool))
	for k := range db.RAMBufferPool {
		keys = append(keys, k)
	}
	slices.Sort(keys)

	for _, k := range keys {
		r := db.RAMBufferPool[k]
		tbl := r.Table
		if tbl == "" {
			tbl = "kv"
		}
		tables[tbl]++
		if tbl == "invoices" || r.AmountUSD > 0 {
			totalRev += r.AmountUSD
		}
		if tbl == "invoices" {
			invCount++
		}
		hashInput.WriteString(r.Key)
		hashInput.WriteByte('=')
		hashInput.WriteString(r.Value)
		hashInput.WriteByte(';')
	}

	sum := sha256.Sum256([]byte(hashInput.String()))
	bufBytes := len(db.RAMBufferPoolSlab)
	pagesWarm := bufBytes / (1024 * 1024)

	return MaterializedStats{
		TotalRows:           len(db.RAMBufferPool),
		InvoiceCount:        invCount,
		TotalRevenueUSD:     totalRev,
		Tables:              tables,
		BufferPoolPagesWarm: pagesWarm,
		BufferPoolBytes:     bufBytes,
		Checksum:            hex.EncodeToString(sum[:6]),
	}
}

func (db *TenantDB) BuildSnapshot(podIP string) TenantSnapshot {
	db.mu.Lock()
	defer db.mu.Unlock()

	if podIP == "" {
		podIP = currentPodIP()
	}
	db.hydrateIfNeededLocked()
	db.recordPodIPLocked(podIP)
	db.RAMQueriesSinceBoot++

	var walBytes int64
	if info, err := os.Stat(db.WALPath); err == nil {
		walBytes = info.Size()
	}

	ramBytes := len(db.RAMBufferPoolSlab)
	ramMB := float64(ramBytes) / (1024.0 * 1024.0)
	savingsPct := 0.0
	if ramBytes > 0 {
		savingsPct = (1.0 - (float64(walBytes) / float64(ramBytes))) * 100.0
		if savingsPct < 0 {
			savingsPct = 0
		}
		savingsPct = math.Round(savingsPct*1000) / 1000
	}

	explanation := "Hot RAM Buffer Pool (16.0 MB) and Materialized View served directly from process heap (0 WAL entries replayed)."
	switch db.LastRecoveryMode {
	case "COLD_DATA_REPLAY":
		explanation = fmt.Sprintf(
			"DATA-Scope Restore: 16.0 MB dirty RAM heap was discarded on GCS commit (%.3f%% bandwidth saved); %d committed rows replayed from %s (%d B) in <1ms.",
			savingsPct, db.WALReplayedOnBoot, db.WALPath, walBytes,
		)
	case "FRESH_BOOT":
		explanation = "Fresh tenant database initialized with 16.0 MB Hot RAM Buffer Pool and /data/wal.jsonl DurableDir."
	}

	rowsCopy := make(map[string]Row, len(db.RAMBufferPool))
	for k, v := range db.RAMBufferPool {
		rowsCopy[k] = v
	}
	walCopy := append([]Row(nil), db.WALEntries...)
	historyCopy := append([]string(nil), db.PodIPHistory...)

	return TenantSnapshot{
		Engine:                "Substrate Per-Tenant Embedded DB (DurableDir + Split-Scope Engine)",
		QueryExecuted:         "SELECT * FROM tenant_records; -- Zero WHERE tenant_id clause (Hardware & DurableDir isolated)",
		RecoveryMode:          db.LastRecoveryMode,
		RecoveryExplanation:   explanation,
		RAMBufferPoolStatus:   db.RAMBufferPoolStatus,
		RAMBufferPoolBytes:    ramBytes,
		RAMBufferPoolMB:       ramMB,
		DurableWALBytes:       walBytes,
		GCSSnapshotSavingsPct: savingsPct,
		DurableDirWALPath:     db.WALPath,
		RowsInDurableDir:      len(rowsCopy),
		Rows:                  rowsCopy,
		WALEntries:            walCopy,
		MaterializedStats:     db.computeMaterializedStatsLocked(),
		RAMQueriesSinceBoot:   db.RAMQueriesSinceBoot,
		WALReplayedOnBoot:     db.WALReplayedOnBoot,
		ProcessBootedAt:       db.ProcessBootedAt.Format(time.RFC3339Nano),
		CurrentPodIP:          podIP,
		PodIPHistory:          historyCopy,
		GDPRDeleteInstruction: "kubectl ate delete actor <tenant-actor> -a <atespace> --any-state",
	}
}

func (db *TenantDB) Snapshot(podIP string) map[string]any {
	s := db.BuildSnapshot(podIP)
	return map[string]any{
		"engine":                   s.Engine,
		"query_executed":           s.QueryExecuted,
		"recovery_mode":            s.RecoveryMode,
		"recovery_explanation":     s.RecoveryExplanation,
		"ram_buffer_pool_status":   s.RAMBufferPoolStatus,
		"ram_buffer_pool_bytes":    s.RAMBufferPoolBytes,
		"ram_buffer_pool_mb":       s.RAMBufferPoolMB,
		"durable_wal_bytes":        s.DurableWALBytes,
		"gcs_snapshot_savings_pct": s.GCSSnapshotSavingsPct,
		"durable_dir_wal_path":     s.DurableDirWALPath,
		"rows_in_durable_dir":      s.RowsInDurableDir,
		"rows":                     s.Rows,
		"wal_entries":              s.WALEntries,
		"materialized_stats":       s.MaterializedStats,
		"ram_queries_since_boot":   s.RAMQueriesSinceBoot,
		"wal_replayed_on_boot":     s.WALReplayedOnBoot,
		"process_booted_at":        s.ProcessBootedAt,
		"current_pod_ip":           s.CurrentPodIP,
		"pod_ip_history":           s.PodIPHistory,
		"gdpr_delete_instruction":  s.GDPRDeleteInstruction,
	}
}

func newMux(db *TenantDB) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("/checkpoint/discard-ram", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		db.DiscardRAMForDataScope()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "ram_discarded_for_data_scope"})
	})
	handleQuery := func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("resume_hint") == "suspend_data" {
			db.DiscardRAMForDataScope()
		}
		key := strings.TrimSpace(r.URL.Query().Get("key"))
		val := r.URL.Query().Get("val")
		table := strings.TrimSpace(r.URL.Query().Get("table"))
		amountUSD, _ := strconv.Atoi(r.URL.Query().Get("amount"))
		podIP := currentPodIP()

		if key != "" {
			if amountUSD == 0 {
				amountUSD = inferAmountForKeyVal(key, val)
			}
			if err := db.PutRow(table, key, val, amountUSD, podIP); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(db.BuildSnapshot(podIP))
	}
	mux.HandleFunc("/query", handleQuery)
	mux.HandleFunc("/", handleQuery)
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
