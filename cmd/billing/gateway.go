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

//go:embed web/*.html
var webFS embed.FS

const (
	PresenterCookieName = "bill_presenter"
)

type GatewayServer struct {
	backend  BillingBackend
	authGate *presenterauth.Gate
}

type XRayComparison struct {
	InsideActorRAM struct {
		ActorName          string `json:"actor_name"`
		TokenEnv           string `json:"token_env"`
		RawSecretsInRAM    int    `json:"raw_secrets_in_ram"`
		TrustBundleMounted bool   `json:"trust_bundle_mounted"`
		TrustBundlePath    string `json:"trust_bundle_path"`
		ProcessPID         int    `json:"process_pid"`
		HeapStorage        string `json:"heap_storage"`
		SnapshotFidelity   string `json:"snapshot_fidelity"`
		VerifiedStatus     string `json:"verified_status"`
	} `json:"inside_actor_ram"`
	OnTheWireVault struct {
		InjectedAuthorization string           `json:"injected_authorization"`
		KeyVersion            string           `json:"key_version"`
		KeyFingerprint        string           `json:"key_fingerprint"`
		InjectedActorJWT      string           `json:"injected_actor_jwt"`
		DecodedClaims         *DecodedActorJWT `json:"decoded_claims,omitempty"`
		UpstreamStatus        string           `json:"upstream_status"`
		VerifiedStatus        string           `json:"verified_status"`
	} `json:"on_the_wire_vault"`
}

type StateResponse struct {
	Subscriptions  []SubscriptionTrackerState `json:"subscriptions"`
	Vault          *VaultStateResponse        `json:"vault"`
	SelectedActor  string                     `json:"selected_actor"`
	XRay           XRayComparison             `json:"xray"`
	Inspection     *MemoryInspectionResult    `json:"inspection,omitempty"`
	LastExfilTest  *ExfiltrationTestResult    `json:"last_exfil_test,omitempty"`
	LastActionMsg  string                     `json:"last_action_msg,omitempty"`
}

func NewGatewayServer(backend BillingBackend, presenterTokens ...string) *GatewayServer {
	var token string
	if len(presenterTokens) > 0 {
		token = strings.TrimSpace(presenterTokens[0])
	}
	return &GatewayServer{
		backend: backend,
		authGate: presenterauth.NewGate(presenterauth.GateConfig{
			Title:            "Demo 7: The 30-Day Sleeping Billing & Escrow Engine",
			Subtitle:         "Sign in with an authorized Google Workspace or event account to inspect zero-secret memory snapshots and rotate vault credentials across frozen GCS actors.",
			SecretName:       "billing-gateway",
			LegacyCookieName: PresenterCookieName,
			PresenterToken:   token,
		}),
	}
}

func (g *GatewayServer) PresenterKey() string {
	return g.authGate.PresenterKey()
}

func writePrettyJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(data)
}

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

func (g *GatewayServer) buildXRay(r *http.Request, actorName string) XRayComparison {
	var xr XRayComparison
	xr.InsideActorRAM.ActorName = actorName
	xr.InsideActorRAM.TokenEnv = DefaultVaultSecretURI
	xr.InsideActorRAM.RawSecretsInRAM = 0
	xr.InsideActorRAM.TrustBundleMounted = true
	xr.InsideActorRAM.TrustBundlePath = DefaultTrustBundlePath
	xr.InsideActorRAM.ProcessPID = 1
	xr.InsideActorRAM.HeapStorage = "100% In-Memory Go Heap RAM (Zero Database or Disk I/O)"
	xr.InsideActorRAM.SnapshotFidelity = "SNAPSHOT_FIDELITY_MEMORY"
	xr.InsideActorRAM.VerifiedStatus = "0 Raw Secrets in RAM (Inert URI Only)"

	vaultState, _ := g.backend.GetVaultState(r.Context())
	if vaultState != nil {
		xr.OnTheWireVault.KeyVersion = vaultState.CurrentVersion
		xr.OnTheWireVault.KeyFingerprint = vaultState.KeyFingerprint
		xr.OnTheWireVault.InjectedAuthorization = fmt.Sprintf("Bearer %s", vaultState.ActiveToken)
		if len(vaultState.RecentCharges) > 0 {
			last := vaultState.RecentCharges[len(vaultState.RecentCharges)-1]
			xr.OnTheWireVault.InjectedActorJWT = last.ActorJWT
			xr.OnTheWireVault.DecodedClaims = last.DecodedClaims
			xr.OnTheWireVault.UpstreamStatus = fmt.Sprintf("200 OK - SETTLED ($%.2f USD)", last.AmountUSD)
		} else {
			xr.OnTheWireVault.InjectedActorJWT = fmt.Sprintf("eyJhbGciOiJSUzI1NiIs... [sub: actor/anything-but-agents/%s]", actorName)
			xr.OnTheWireVault.DecodedClaims = &DecodedActorJWT{
				Issuer:    "https://idp.ate-system.svc",
				Subject:   "actor/anything-but-agents/" + actorName,
				Audiences: []string{"https://billing-gateway.anything-but-agents.svc.cluster.local"},
				IssuedAt:  time.Now().UTC().Unix(),
				ExpiresAt: time.Now().UTC().Add(15 * time.Minute).Unix(),
				ActorInfo: ActorClaimsInfo{
					Atespace:  "anything-but-agents",
					ActorName: actorName,
					ActorUID:  "uid-" + actorName + "-4f9c",
				},
			}
			xr.OnTheWireVault.UpstreamStatus = "Ready for settlement turn"
		}
		xr.OnTheWireVault.VerifiedStatus = fmt.Sprintf("Rotated Credential (%s) + Actor JWT Injected", vaultState.CurrentVersion)
	}
	return xr
}

func (g *GatewayServer) Mux() *http.ServeMux {
	mux := http.NewServeMux()
	g.authGate.RegisterRoutes(mux)

	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})

	// Public / In-Cluster Payment Vault Charge API
	mux.HandleFunc("/api/vault/charge", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var chargeReq VaultChargeRequest
		if err := json.NewDecoder(r.Body).Decode(&chargeReq); err != nil {
			http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
			return
		}

		authHeader := r.Header.Get("Authorization")
		jwtHeader := r.Header.Get("X-Actor-Identity-JWT")

		receipt, status, err := g.backend.GetVault().ProcessCharge(chargeReq, authHeader, jwtHeader)
		if err != nil {
			http.Error(w, err.Error(), status)
			return
		}
		writePrettyJSON(w, http.StatusOK, receipt)
	})

	// Public / In-Cluster Untrusted Echo API for Host-Binding Exfiltration Proof
	mux.HandleFunc("/api/echo", func(w http.ResponseWriter, r *http.Request) {
		bodyBytes, _ := io.ReadAll(r.Body)
		headersMap := make(map[string]any)
		for k, v := range r.Header {
			if len(v) == 1 {
				headersMap[k] = v[0]
			} else {
				headersMap[k] = v
			}
		}
		resp := map[string]any{
			"status":        "ECHO_OK",
			"method":        r.Method,
			"path":          r.URL.Path,
			"headers":       headersMap,
			"body_received": string(bodyBytes),
			"timestamp":     time.Now().UTC(),
		}
		writePrettyJSON(w, http.StatusOK, resp)
	})

	// Authenticated Presenter APIs

	mux.HandleFunc("/api/state", g.authGate.RequireAPI(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		actorName := r.URL.Query().Get("actor")
		if actorName == "" {
			actorName = defaultSubscriptionActors[0]
		}
		subs, err := g.backend.ListSubscriptions(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		vState, _ := g.backend.GetVaultState(r.Context())
		xr := g.buildXRay(r, actorName)

		resp := StateResponse{
			Subscriptions: subs,
			Vault:         vState,
			SelectedActor: actorName,
			XRay:          xr,
		}
		writePrettyJSON(w, http.StatusOK, resp)
	}))

	mux.HandleFunc("/api/subscription/settle", g.authGate.RequireAPI(func(w http.ResponseWriter, r *http.Request) {
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
			req.ActorName = defaultSubscriptionActors[0]
		}

		st, receipt, err := g.backend.SettleActor(r.Context(), req.ActorName)
		if err != nil {
			http.Error(w, "settle failed: "+err.Error(), http.StatusInternalServerError)
			return
		}

		subs, _ := g.backend.ListSubscriptions(r.Context())
		vState, _ := g.backend.GetVaultState(r.Context())
		xr := g.buildXRay(r, req.ActorName)

		resp := StateResponse{
			Subscriptions: subs,
			Vault:         vState,
			SelectedActor: req.ActorName,
			XRay:          xr,
			LastActionMsg: fmt.Sprintf("Settled Cycle #%d ($%.2f USD) for %s via Payment Vault (Key %s). Actor suspended to GCS.", st.CurrentCycle, st.MonthlyFee, st.CustomerName, receipt.KeyVersion),
		}
		writePrettyJSON(w, http.StatusOK, resp)
	}))

	mux.HandleFunc("/api/subscription/inspect-memory", g.authGate.RequireAPI(func(w http.ResponseWriter, r *http.Request) {
		actorName := r.URL.Query().Get("actor")
		if actorName == "" {
			actorName = defaultSubscriptionActors[0]
		}
		res, err := g.backend.InspectActorMemory(r.Context(), actorName)
		if err != nil {
			http.Error(w, "inspect memory failed: "+err.Error(), http.StatusInternalServerError)
			return
		}
		writePrettyJSON(w, http.StatusOK, res)
	}))

	mux.HandleFunc("/api/subscription/test-exfiltrate", g.authGate.RequireAPI(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			ActorName string `json:"actor_name"`
			TargetURL string `json:"target_url"`
		}
		if err := decodeOptionalJSON(r, &req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if req.ActorName == "" {
			req.ActorName = defaultSubscriptionActors[0]
		}

		res, err := g.backend.TestExfiltration(r.Context(), req.ActorName, req.TargetURL)
		if err != nil {
			http.Error(w, "exfiltration test failed: "+err.Error(), http.StatusInternalServerError)
			return
		}
		writePrettyJSON(w, http.StatusOK, res)
	}))

	mux.HandleFunc("/api/vault/rotate", g.authGate.RequireAPI(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		newVer, newKey, err := g.backend.RotateSecret(r.Context())
		if err != nil {
			http.Error(w, "rotate secret failed: "+err.Error(), http.StatusInternalServerError)
			return
		}

		subs, _ := g.backend.ListSubscriptions(r.Context())
		vState, _ := g.backend.GetVaultState(r.Context())
		actorName := r.URL.Query().Get("actor")
		if actorName == "" {
			actorName = defaultSubscriptionActors[0]
		}
		xr := g.buildXRay(r, actorName)

		resp := StateResponse{
			Subscriptions: subs,
			Vault:         vState,
			SelectedActor: actorName,
			XRay:          xr,
			LastActionMsg: fmt.Sprintf("Rotated vault secret to %s (%s). All sleeping actors in GCS will transparently receive this new key on their next wake turn!", newVer, newKey[:16]+"..."),
		}
		writePrettyJSON(w, http.StatusOK, resp)
	}))

	mux.HandleFunc("/api/reset", g.authGate.RequireAPI(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if err := g.backend.ResetStage(r.Context()); err != nil {
			http.Error(w, "reset stage failed: "+err.Error(), http.StatusInternalServerError)
			return
		}
		subs, _ := g.backend.ListSubscriptions(r.Context())
		vState, _ := g.backend.GetVaultState(r.Context())
		xr := g.buildXRay(r, defaultSubscriptionActors[0])
		resp := StateResponse{
			Subscriptions: subs,
			Vault:         vState,
			SelectedActor: defaultSubscriptionActors[0],
			XRay:          xr,
			LastActionMsg: "Stage reset: Vault rotated to clean v1 and all subscription actors reset to Cycle 0 (sleeping in GCS).",
		}
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

	mux.HandleFunc("/", serveUI)
	mux.HandleFunc("/dashboard", serveUI)

	return mux
}
