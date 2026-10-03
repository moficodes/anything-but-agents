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
	"testing"
)

func TestTenantDBPersistenceAcrossDataScopeRestart(t *testing.T) {
	dir := t.TempDir()

	db1, err := openTenantDB(dir)
	if err != nil {
		t.Fatalf("openTenantDB: %v", err)
	}
	if err := db1.Put("plan", "enterprise"); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := db1.Put("seats", "250"); err != nil {
		t.Fatalf("Put: %v", err)
	}

	// Simulate a DATA-scope Resume: process restarts from scratch with the same DurableDir (/data).
	db2, err := openTenantDB(dir)
	if err != nil {
		t.Fatalf("reopen TenantDB: %v", err)
	}
	snap := db2.Snapshot("10.0.0.9")
	if snap["rows_in_durable_dir"] != 2 {
		t.Fatalf("expected 2 rows restored from DurableDir WAL, got %v", snap["rows_in_durable_dir"])
	}
	if snap["wal_replayed_on_boot"] != 2 {
		t.Fatalf("expected 2 WAL entries replayed on boot, got %v", snap["wal_replayed_on_boot"])
	}
}
