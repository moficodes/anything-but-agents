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
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestActor_LedgerPersistenceAcrossCycles(t *testing.T) {
	backend := NewLocalSimulatorBackend("anything-but-agents", "billing")
	ctx := context.Background()

	// Settle Cycle 1 for sub-acme
	st1, rcpt1, err := backend.SettleActor(ctx, "sub-acme")
	if err != nil {
		t.Fatalf("SettleActor Cycle 1 failed: %v", err)
	}
	if st1.CurrentCycle != 1 {
		t.Errorf("expected CurrentCycle 1, got %d", st1.CurrentCycle)
	}
	if st1.CumulativeSettled != 5000.00 {
		t.Errorf("expected CumulativeSettled 5000.00, got %.2f", st1.CumulativeSettled)
	}
	if len(st1.Ledger) != 1 {
		t.Fatalf("expected 1 invoice in ledger, got %d", len(st1.Ledger))
	}
	if rcpt1.KeyVersion != "v1" {
		t.Errorf("expected receipt KeyVersion v1, got %s", rcpt1.KeyVersion)
	}

	// Settle Cycle 2 for sub-acme
	st2, rcpt2, err := backend.SettleActor(ctx, "sub-acme")
	if err != nil {
		t.Fatalf("SettleActor Cycle 2 failed: %v", err)
	}
	if st2.CurrentCycle != 2 {
		t.Errorf("expected CurrentCycle 2, got %d", st2.CurrentCycle)
	}
	if st2.CumulativeSettled != 10000.00 {
		t.Errorf("expected CumulativeSettled 10000.00, got %.2f", st2.CumulativeSettled)
	}
	if len(st2.Ledger) != 2 {
		t.Fatalf("expected 2 invoices in ledger, got %d", len(st2.Ledger))
	}
	if st2.Ledger[0].Cycle != 1 || st2.Ledger[1].Cycle != 2 {
		t.Errorf("ledger cycles mismatch: %+v", st2.Ledger)
	}
	if rcpt2.KeyVersion != "v1" {
		t.Errorf("expected receipt KeyVersion v1, got %s", rcpt2.KeyVersion)
	}
}

func TestActor_ZeroSecretMemoryInspection(t *testing.T) {
	backend := NewLocalSimulatorBackend("anything-but-agents", "billing")
	ctx := context.Background()

	insp, err := backend.InspectActorMemory(ctx, "sub-acme")
	if err != nil {
		t.Fatalf("InspectActorMemory failed: %v", err)
	}

	if insp.RawSecretsInEnv != 0 {
		t.Errorf("expected 0 raw secrets in env, found %d", insp.RawSecretsInEnv)
	}
	if insp.RawSecretsInHeap != 0 {
		t.Errorf("expected 0 raw secrets in heap RAM, found %d", insp.RawSecretsInHeap)
	}
	if !insp.ZeroSecretsVerified {
		t.Errorf("expected ZeroSecretsVerified true, got false")
	}
	if !strings.HasPrefix(insp.PaymentVaultTokenEnv, "ate-secret://") {
		t.Errorf("expected PaymentVaultTokenEnv to start with ate-secret://, got %s", insp.PaymentVaultTokenEnv)
	}
	if insp.ProcessPID <= 0 {
		t.Errorf("expected valid process PID, got %d", insp.ProcessPID)
	}
}

func TestSecretRotationWhileSuspended(t *testing.T) {
	backend := NewLocalSimulatorBackend("anything-but-agents", "billing")
	ctx := context.Background()

	// Step 1: Settle Cycle 1 with v1
	_, rcpt1, err := backend.SettleActor(ctx, "sub-acme")
	if err != nil {
		t.Fatalf("Cycle 1 settlement failed: %v", err)
	}
	if rcpt1.KeyVersion != "v1" {
		t.Fatalf("expected KeyVersion v1 for Cycle 1, got %s", rcpt1.KeyVersion)
	}

	// Verify actor is suspended in GCS
	subs, _ := backend.ListSubscriptions(ctx)
	for _, s := range subs {
		if s.ActorName == "sub-acme" && s.Status != "SUSPENDED" {
			t.Errorf("expected sub-acme to be SUSPENDED after settlement, got %s", s.Status)
		}
	}

	// Step 2: Rotate secret from v1 to v2 while actor is FROZEN in GCS
	newVer, newKey, err := backend.RotateSecret(ctx)
	if err != nil {
		t.Fatalf("RotateSecret failed: %v", err)
	}
	if newVer != "v2" {
		t.Fatalf("expected new version v2, got %s", newVer)
	}
	if !strings.Contains(newKey, "vault_v2_") {
		t.Fatalf("expected new key to contain vault_v2_, got %s", newKey)
	}

	// Step 3: Wake sub-acme for Cycle 2 settlement
	st2, rcpt2, err := backend.SettleActor(ctx, "sub-acme")
	if err != nil {
		t.Fatalf("Cycle 2 settlement failed: %v", err)
	}

	// Verify that the woken actor automatically received v2 on the wire!
	if rcpt2.KeyVersion != "v2" {
		t.Errorf("expected KeyVersion v2 for Cycle 2, got %s", rcpt2.KeyVersion)
	}

	// Verify that Cycle 1 from RAM was preserved across the suspend/resume!
	if len(st2.Ledger) != 2 {
		t.Fatalf("expected 2 invoices in ledger, got %d", len(st2.Ledger))
	}
	if st2.Ledger[0].KeyVersionUsed != "v1" || st2.Ledger[1].KeyVersionUsed != "v2" {
		t.Errorf("expected ledger key versions [v1, v2], got [%s, %s]",
			st2.Ledger[0].KeyVersionUsed, st2.Ledger[1].KeyVersionUsed)
	}
	if st2.CumulativeSettled != 10000.00 {
		t.Errorf("expected cumulative settled 10000.00, got %.2f", st2.CumulativeSettled)
	}
}

func TestHostBoundHeaderReplacement_UntrustedEcho(t *testing.T) {
	backend := NewLocalSimulatorBackend("anything-but-agents", "billing")
	ctx := context.Background()

	// Have actor attempt to send placeholder to untrusted echo server
	res, err := backend.TestExfiltration(ctx, "sub-acme", "http://untrusted-echo.external.test/api/echo")
	if err != nil {
		t.Fatalf("TestExfiltration failed: %v", err)
	}

	if res.RealSecretExfiltrated {
		t.Errorf("ALERT: Real secret was exfiltrated to untrusted host!")
	}
	if !strings.Contains(res.HeaderReceivedByTarget, "ate-secret://") {
		t.Errorf("expected target to receive placeholder URI ate-secret://, got %s", res.HeaderReceivedByTarget)
	}
	if strings.Contains(res.HeaderReceivedByTarget, "sk_live_") {
		t.Errorf("target unexpectedly received real live secret: %s", res.HeaderReceivedByTarget)
	}
}

func TestPaymentVault_RejectionCases(t *testing.T) {
	store := NewSecretStore()
	vault := NewPaymentVault(store)

	req := VaultChargeRequest{
		ActorName:     "sub-acme",
		CustomerID:    "Acme Corp",
		AmountUSD:     5000.00,
		Cycle:         1,
		EscrowAccount: "escrow-acme-corp",
	}

	// Case 1: Raw unresolved placeholder leaked to vault -> rejected with leak alert
	_, status1, err1 := vault.ProcessCharge(req, "Bearer ate-secret://k8s.io/default/anything-but-agents/billing-vault-api/token", "")
	if status1 != http.StatusUnauthorized || err1 == nil || !strings.Contains(err1.Error(), "LEAK_DETECTED") {
		t.Errorf("expected 401 LEAK_DETECTED, got status %d, err %v", status1, err1)
	}

	// Case 2: Valid v1 token -> 200 OK
	v1Token := store.CurrentToken()
	rcpt, status2, err2 := vault.ProcessCharge(req, "Bearer "+v1Token, "test-jwt")
	if status2 != http.StatusOK || err2 != nil || rcpt.KeyVersion != "v1" {
		t.Errorf("expected 200 OK v1, got status %d, err %v, rcpt: %+v", status2, err2, rcpt)
	}

	// Case 3: Rotate secret to v2, then try presenting old v1 token -> 401 EXPIRED_KEY
	store.RotateSecret("v2")
	_, status3, err3 := vault.ProcessCharge(req, "Bearer "+v1Token, "")
	if status3 != http.StatusUnauthorized || err3 == nil || !strings.Contains(err3.Error(), "EXPIRED_KEY") {
		t.Errorf("expected 401 EXPIRED_KEY for old v1, got status %d, err %v", status3, err3)
	}

	// Case 4: Random invalid token -> 401 INVALID_KEY
	_, status4, err4 := vault.ProcessCharge(req, "Bearer sk_invalid_key_random", "")
	if status4 != http.StatusUnauthorized || err4 == nil || !strings.Contains(err4.Error(), "INVALID_KEY") {
		t.Errorf("expected 401 INVALID_KEY, got status %d, err %v", status4, err4)
	}
}

func TestGateway_Endpoints(t *testing.T) {
	backend := NewLocalSimulatorBackend("anything-but-agents", "billing")
	gw := NewGatewayServer(backend, "test-presenter-token-12345")
	mux := gw.Mux()

	// 1. /readyz & /healthz
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("/readyz expected 200, got %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("/healthz expected 200, got %d", rec.Code)
	}

	// Helper for authenticated requests
	authReq := func(method, path string, body any) *http.Response {
		var rdr io.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			rdr = bytes.NewReader(b)
		}
		req := httptest.NewRequest(method, path, rdr)
		req.Header.Set("X-Presenter-Key", "test-presenter-token-12345")
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		return w.Result()
	}

	// 2. /api/state
	res := authReq(http.MethodGet, "/api/state?actor=sub-acme", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("/api/state failed: %d", res.StatusCode)
	}
	var stateResp StateResponse
	_ = json.NewDecoder(res.Body).Decode(&stateResp)
	if len(stateResp.Subscriptions) != 3 {
		t.Errorf("expected 3 subscriptions, got %d", len(stateResp.Subscriptions))
	}
	if stateResp.XRay.InsideActorRAM.RawSecretsInRAM != 0 {
		t.Errorf("expected 0 raw secrets in XRay, got %d", stateResp.XRay.InsideActorRAM.RawSecretsInRAM)
	}

	// 3. /api/subscription/settle
	res = authReq(http.MethodPost, "/api/subscription/settle", map[string]string{"actor_name": "sub-acme"})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("/api/subscription/settle failed: %d", res.StatusCode)
	}
	var settleResp StateResponse
	_ = json.NewDecoder(res.Body).Decode(&settleResp)
	if settleResp.XRay.OnTheWireVault.KeyVersion != "v1" {
		t.Errorf("expected XRay KeyVersion v1, got %s", settleResp.XRay.OnTheWireVault.KeyVersion)
	}

	// 4. /api/subscription/inspect-memory
	res = authReq(http.MethodGet, "/api/subscription/inspect-memory?actor=sub-acme", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("/api/subscription/inspect-memory failed: %d", res.StatusCode)
	}
	var insp MemoryInspectionResult
	_ = json.NewDecoder(res.Body).Decode(&insp)
	if !insp.ZeroSecretsVerified {
		t.Errorf("expected ZeroSecretsVerified true")
	}

	// 5. /api/vault/rotate
	res = authReq(http.MethodPost, "/api/vault/rotate", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("/api/vault/rotate failed: %d", res.StatusCode)
	}
	var rotateResp StateResponse
	_ = json.NewDecoder(res.Body).Decode(&rotateResp)
	if rotateResp.Vault.CurrentVersion != "v2" {
		t.Errorf("expected vault version v2 after rotate, got %s", rotateResp.Vault.CurrentVersion)
	}

	// 6. /api/echo
	echoReq := httptest.NewRequest(http.MethodPost, "/api/echo", strings.NewReader("hello echo"))
	echoReq.Header.Set("Authorization", "Bearer test-placeholder")
	echoRec := httptest.NewRecorder()
	mux.ServeHTTP(echoRec, echoReq)
	if echoRec.Code != http.StatusOK {
		t.Fatalf("/api/echo failed: %d", echoRec.Code)
	}
	var echoResp map[string]any
	_ = json.NewDecoder(echoRec.Body).Decode(&echoResp)
	if echoResp["status"] != "ECHO_OK" {
		t.Errorf("expected ECHO_OK, got %v", echoResp["status"])
	}

	// 7. /api/reset
	res = authReq(http.MethodPost, "/api/reset", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("/api/reset failed: %d", res.StatusCode)
	}
	var resetResp StateResponse
	_ = json.NewDecoder(res.Body).Decode(&resetResp)
	if resetResp.Vault.CurrentVersion != "v1" {
		t.Errorf("expected reset to restore v1, got %s", resetResp.Vault.CurrentVersion)
	}
}

func TestIndexHTML_RotateKeyButtonLabel(t *testing.T) {
	data, err := webFS.ReadFile("web/index.html")
	if err != nil {
		t.Fatalf("failed to read web/index.html: %v", err)
	}
	content := string(data)
	if !strings.Contains(content, "<span>Rotate Key</span>") {
		t.Errorf("expected button label to say '<span>Rotate Key</span>', but not found")
	}
	if strings.Contains(content, "v1 &rarr; v2") || strings.Contains(content, "v1 -> v2") {
		t.Errorf("expected no hardcoded 'v1 -> v2' button label in index.html")
	}
}
