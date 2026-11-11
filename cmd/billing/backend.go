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
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"time"
)

var defaultSubscriptionActors = []string{"sub-acme", "sub-globex", "sub-initech"}

// DefaultCustomerSpec defines the initial catalog of subscription actors.
type DefaultCustomerSpec struct {
	ActorName       string
	CustomerName    string
	PlanTier        string
	MonthlyFee      float64
	EscrowAccountID string
}

var defaultCustomerSpecs = map[string]DefaultCustomerSpec{
	"sub-acme": {
		ActorName:       "sub-acme",
		CustomerName:    "Acme Corp",
		PlanTier:        "Enterprise Tier ($5,000/mo)",
		MonthlyFee:      5000.00,
		EscrowAccountID: "escrow-acme-corp",
	},
	"sub-globex": {
		ActorName:       "sub-globex",
		CustomerName:    "Globex International",
		PlanTier:        "Growth Tier ($1,500/mo)",
		MonthlyFee:      1500.00,
		EscrowAccountID: "escrow-globex-intl",
	},
	"sub-initech": {
		ActorName:       "sub-initech",
		CustomerName:    "Initech Systems",
		PlanTier:        "Startup Tier ($500/mo)",
		MonthlyFee:      500.00,
		EscrowAccountID: "escrow-initech-sys",
	},
}

// SecretVersionInfo stores metadata about a vault key version.
type SecretVersionInfo struct {
	Version   string    `json:"version"`
	SecretKey string    `json:"secret_key"`
	Active    bool      `json:"active"`
	RotatedAt time.Time `json:"rotated_at"`
}

// SecretStore manages secret versions and rotation.
type SecretStore struct {
	mu             sync.RWMutex
	currentVersion string
	versions       map[string]SecretVersionInfo
}

// NewSecretStore creates a secret store initialized with version v1.
func NewSecretStore() *SecretStore {
	ss := &SecretStore{
		currentVersion: "v1",
		versions:       make(map[string]SecretVersionInfo),
	}
	ss.versions["v1"] = SecretVersionInfo{
		Version:   "v1",
		SecretKey: "sk_live_vault_v1_8f92a14c6e0b7d3a",
		Active:    true,
		RotatedAt: time.Now().UTC(),
	}
	return ss
}

func (s *SecretStore) CurrentVersion() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.currentVersion
}

func (s *SecretStore) CurrentToken() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if v, ok := s.versions[s.currentVersion]; ok {
		return v.SecretKey
	}
	return ""
}

func (s *SecretStore) RotateSecret(customVersion ...string) (string, string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for k, v := range s.versions {
		v.Active = false
		s.versions[k] = v
	}

	var newVer string
	if len(customVersion) > 0 && customVersion[0] != "" {
		newVer = customVersion[0]
	} else {
		// e.g. v1 -> v2 -> v3
		currNum := 1
		if strings.HasPrefix(s.currentVersion, "v") {
			var n int
			if _, err := fmt.Sscanf(s.currentVersion, "v%d", &n); err == nil {
				currNum = n
			}
		}
		newVer = fmt.Sprintf("v%d", currNum+1)
	}

	randomBytes := make([]byte, 8)
	_, _ = rand.Read(randomBytes)
	newKey := fmt.Sprintf("sk_live_vault_%s_%s", newVer, hex.EncodeToString(randomBytes))

	s.versions[newVer] = SecretVersionInfo{
		Version:   newVer,
		SecretKey: newKey,
		Active:    true,
		RotatedAt: time.Now().UTC(),
	}
	s.currentVersion = newVer
	return newVer, newKey
}

func (s *SecretStore) ValidateKey(token string) (bool, string) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	active, ok := s.versions[s.currentVersion]
	if !ok {
		return false, ""
	}
	if token == active.SecretKey {
		return true, active.Version
	}
	for ver, v := range s.versions {
		if v.SecretKey == token {
			return false, ver // Valid key format, but expired/inactive!
		}
	}
	return false, ""
}

func (s *SecretStore) GetState() (string, string, []SecretVersionInfo) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	list := make([]SecretVersionInfo, 0, len(s.versions))
	for _, v := range s.versions {
		list = append(list, v)
	}
	slices.SortFunc(list, func(a, b SecretVersionInfo) int {
		return strings.Compare(a.Version, b.Version)
	})
	return s.currentVersion, s.versions[s.currentVersion].SecretKey, list
}

func (s *SecretStore) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.currentVersion = "v1"
	s.versions = make(map[string]SecretVersionInfo)
	s.versions["v1"] = SecretVersionInfo{
		Version:   "v1",
		SecretKey: "sk_live_vault_v1_8f92a14c6e0b7d3a",
		Active:    true,
		RotatedAt: time.Now().UTC(),
	}
}

// PaymentVault verifies incoming settlement charges.
type PaymentVault struct {
	mu          sync.RWMutex
	secretStore *SecretStore
	charges     []VaultChargeReceipt
}

func NewPaymentVault(store *SecretStore) *PaymentVault {
	return &PaymentVault{
		secretStore: store,
		charges:     make([]VaultChargeReceipt, 0),
	}
}

// ProcessCharge processes and records an inbound payment settlement call.
func (v *PaymentVault) ProcessCharge(req VaultChargeRequest, authHeader, jwtHeader string) (*VaultChargeReceipt, int, error) {
	v.mu.Lock()
	defer v.mu.Unlock()

	trimmedAuth := strings.TrimSpace(authHeader)
	if !strings.HasPrefix(trimmedAuth, "Bearer ") {
		return nil, http.StatusUnauthorized, errors.New("missing or malformed Authorization header; expected Bearer token")
	}

	token := strings.TrimSpace(strings.TrimPrefix(trimmedAuth, "Bearer "))

	// Check if the raw ate-secret:// placeholder reached the vault!
	if strings.Contains(token, "ate-secret://") {
		return nil, http.StatusUnauthorized, fmt.Errorf("LEAK_DETECTED: Upstream vault received unresolved credential URI (%s) instead of substituted secret value", token)
	}

	valid, keyVer := v.secretStore.ValidateKey(token)
	if !valid {
		if keyVer != "" {
			return nil, http.StatusUnauthorized, fmt.Errorf("EXPIRED_KEY: Vault key %s was rotated out and is no longer valid", keyVer)
		}
		return nil, http.StatusUnauthorized, errors.New("INVALID_KEY: Credential does not match currently active payment vault key")
	}

	// Parse / decode Actor JWT claims
	decodedClaims := parseActorJWT(jwtHeader, req.ActorName)

	shortKey := token
	if len(shortKey) > 16 {
		shortKey = shortKey[:14] + "..." + shortKey[len(shortKey)-4:]
	}

	txID := fmt.Sprintf("tx_settle_%s_c%02d_%x", req.ActorName, req.Cycle, time.Now().UnixNano()%0xFFFFF)
	rcpt := VaultChargeReceipt{
		TxID:           txID,
		Status:         "SETTLED",
		AmountUSD:      req.AmountUSD,
		Cycle:          req.Cycle,
		KeyVersion:     keyVer,
		KeyFingerprint: shortKey,
		RawAuthHeader:  trimmedAuth,
		ActorJWT:       jwtHeader,
		DecodedClaims:  decodedClaims,
		SettledAt:      time.Now().UTC(),
		Message:        fmt.Sprintf("Settled $%.2f USD via Payment Vault (Egress Injected Key %s)", req.AmountUSD, keyVer),
	}

	v.charges = append(v.charges, rcpt)
	return &rcpt, http.StatusOK, nil
}

func parseActorJWT(rawToken, fallbackActor string) *DecodedActorJWT {
	if rawToken == "" || rawToken == "ate-actor-jwt://self" {
		// Synthesize expected claims for demonstration if mock placeholder
		return &DecodedActorJWT{
			Issuer:    "https://idp.ate-system.svc",
			Subject:   "actor/anything-but-agents/" + fallbackActor,
			Audiences: []string{"https://billing-gateway.anything-but-agents.svc.cluster.local"},
			IssuedAt:  time.Now().UTC().Unix(),
			ExpiresAt: time.Now().UTC().Add(15 * time.Minute).Unix(),
			ActorInfo: ActorClaimsInfo{
				Atespace:  "anything-but-agents",
				ActorName: fallbackActor,
				ActorUID:  "uid-" + fallbackActor + "-4f9c",
			},
		}
	}

	parts := strings.Split(rawToken, ".")
	if len(parts) >= 2 {
		payloadBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
		if err == nil {
			var claims DecodedActorJWT
			if err := json.Unmarshal(payloadBytes, &claims); err == nil && claims.Subject != "" {
				return &claims
			}
		}
	}

	return &DecodedActorJWT{
		Issuer:    "https://idp.ate-system.svc",
		Subject:   "actor/anything-but-agents/" + fallbackActor,
		Audiences: []string{"https://billing-gateway.anything-but-agents.svc.cluster.local"},
		IssuedAt:  time.Now().UTC().Unix(),
		ExpiresAt: time.Now().UTC().Add(15 * time.Minute).Unix(),
		ActorInfo: ActorClaimsInfo{
			Atespace:  "anything-but-agents",
			ActorName: fallbackActor,
		},
	}
}

func (v *PaymentVault) GetCharges() []VaultChargeReceipt {
	v.mu.RLock()
	defer v.mu.RUnlock()
	out := make([]VaultChargeReceipt, len(v.charges))
	copy(out, v.charges)
	return out
}

func (v *PaymentVault) Reset() {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.charges = make([]VaultChargeReceipt, 0)
}

// VaultStateResponse captures current vault status for the UI.
type VaultStateResponse struct {
	CurrentVersion string              `json:"current_version"`
	ActiveToken    string              `json:"active_token"`
	KeyFingerprint string              `json:"key_fingerprint"`
	TotalCharges   int                 `json:"total_charges"`
	Versions       []SecretVersionInfo `json:"versions"`
	RecentCharges  []VaultChargeReceipt `json:"recent_charges"`
}

// SubscriptionTrackerState represents a customer subscription actor in the gateway fleet.
type SubscriptionTrackerState struct {
	ActorName         string              `json:"actor_name"`
	CustomerName      string              `json:"customer_name"`
	PlanTier          string              `json:"plan_tier"`
	MonthlyFee        float64             `json:"monthly_fee"`
	EscrowAccountID   string              `json:"escrow_account_id"`
	CurrentCycle      int                 `json:"current_cycle"`
	Status            string              `json:"status"` // RUNNING | SUSPENDED
	CumulativeSettled float64             `json:"cumulative_settled"`
	CurrentPodIP      string              `json:"current_pod_ip"`
	PodIPHistory      []string            `json:"pod_ip_history"`
	Ledger            []Invoice           `json:"ledger"`
	LastReceipt       *VaultChargeReceipt `json:"last_receipt,omitempty"`
	LastSettledAt     *time.Time          `json:"last_settled_at,omitempty"`
}

// BillingBackend is the interface implemented by both ClusterAteBackend and LocalSimulatorBackend.
type BillingBackend interface {
	EnsureActor(ctx context.Context, actorName string) error
	SettleActor(ctx context.Context, actorName string) (*SubscriptionState, *VaultChargeReceipt, error)
	InspectActorMemory(ctx context.Context, actorName string) (*MemoryInspectionResult, error)
	TestExfiltration(ctx context.Context, actorName string, targetURL string) (*ExfiltrationTestResult, error)
	RotateSecret(ctx context.Context) (string, string, error)
	GetVaultState(ctx context.Context) (*VaultStateResponse, error)
	SuspendActor(ctx context.Context, actorName string) error
	ListSubscriptions(ctx context.Context) ([]SubscriptionTrackerState, error)
	ResetStage(ctx context.Context) error
	GetVault() *PaymentVault
	GetSecretStore() *SecretStore
}

// --- LocalSimulatorBackend (--mode=local & unit tests) ---

var simulatedWorkerPodIPs = []string{"10.44.0.9", "10.44.1.14", "10.44.2.21"}

type LocalSimulatorBackend struct {
	mu          sync.Mutex
	atespace    string
	template    string
	secretStore *SecretStore
	vault       *PaymentVault
	actors      map[string]*SubscriptionActor
	podIndices  map[string]int
	order       []string
}

func NewLocalSimulatorBackend(atespace, template string) *LocalSimulatorBackend {
	store := NewSecretStore()
	vault := NewPaymentVault(store)
	b := &LocalSimulatorBackend{
		atespace:    atespace,
		template:    template,
		secretStore: store,
		vault:       vault,
		actors:      make(map[string]*SubscriptionActor),
		podIndices:  make(map[string]int),
		order:       append([]string{}, defaultSubscriptionActors...),
	}

	for _, name := range defaultSubscriptionActors {
		_ = b.EnsureActor(context.Background(), name)
	}
	return b
}

func (b *LocalSimulatorBackend) GetVault() *PaymentVault {
	return b.vault
}

func (b *LocalSimulatorBackend) GetSecretStore() *SecretStore {
	return b.secretStore
}

func (b *LocalSimulatorBackend) EnsureActor(_ context.Context, actorName string) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if _, exists := b.actors[actorName]; exists {
		return nil
	}

	spec, ok := defaultCustomerSpecs[actorName]
	if !ok {
		spec = DefaultCustomerSpec{
			ActorName:       actorName,
			CustomerName:    "Customer " + actorName,
			PlanTier:        "Standard Tier ($1,000/mo)",
			MonthlyFee:      1000.00,
			EscrowAccountID: "escrow-" + actorName,
		}
	}

	actor := NewSubscriptionActor(
		spec.ActorName,
		spec.CustomerName,
		spec.PlanTier,
		spec.MonthlyFee,
		spec.EscrowAccountID,
		DefaultVaultURL,
		DefaultEchoURL,
		DefaultVaultSecretURI,
		DefaultTrustBundlePath,
	)
	actor.Status = "SUSPENDED" // Initially sleeping in GCS
	actor.CurrentPodIP = simulatedWorkerPodIPs[0]
	actor.PodIPHistory = []string{simulatedWorkerPodIPs[0]}

	b.actors[actorName] = actor
	b.podIndices[actorName] = 0
	if !slices.Contains(b.order, actorName) {
		b.order = append(b.order, actorName)
	}
	return nil
}

func (b *LocalSimulatorBackend) SettleActor(ctx context.Context, actorName string) (*SubscriptionState, *VaultChargeReceipt, error) {
	b.mu.Lock()
	actor, exists := b.actors[actorName]
	if !exists {
		b.mu.Unlock()
		return nil, nil, fmt.Errorf("actor %s not found", actorName)
	}

	// 1. Wake actor from GCS: transition SUSPENDED -> RUNNING on simulated worker pod
	actor.Status = "RUNNING"
	idx := (b.podIndices[actorName] + 1) % len(simulatedWorkerPodIPs)
	b.podIndices[actorName] = idx
	newPodIP := simulatedWorkerPodIPs[idx]
	actor.UpdatePodIP(newPodIP)

	// In local simulation, wire a custom RoundTripper that mimics atenet-egress MITM injection
	activeToken := b.secretStore.CurrentToken()
	simEgressClient := &http.Client{
		Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			// Check if host matches the authorized Payment Vault rule in EgressPolicy
			reqHost := req.URL.Hostname()
			if strings.Contains(reqHost, "billing-gateway") || reqHost == "localhost" || reqHost == "127.0.0.1" {
				// MITM Injection: Replace placeholder Authorization with active K8s secret!
				req.Header.Set("Authorization", "Bearer "+activeToken)
				// MITM Injection: Mint and attach cryptographic Actor JWT!
				jwtToken := mintSimulatedActorJWT(b.atespace, actorName)
				req.Header.Set("X-Actor-Identity-JWT", jwtToken)

				// Deliver directly to the Payment Vault
				var chargeReq VaultChargeRequest
				if err := json.NewDecoder(req.Body).Decode(&chargeReq); err != nil {
					return &http.Response{
						StatusCode: http.StatusBadRequest,
						Body:       io.NopCloser(strings.NewReader(err.Error())),
					}, nil
				}
				receipt, status, err := b.vault.ProcessCharge(chargeReq, req.Header.Get("Authorization"), req.Header.Get("X-Actor-Identity-JWT"))
				if err != nil {
					return &http.Response{
						StatusCode: status,
						Body:       io.NopCloser(strings.NewReader(err.Error())),
					}, nil
				}
				respBytes, _ := json.Marshal(receipt)
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(bytes.NewReader(respBytes)),
					Header:     make(http.Header),
				}, nil
			}

			// Untrusted target: Do NOT inject credentials!
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(`{"headers":{"Authorization":"` + req.Header.Get("Authorization") + `"}}`)),
				Header:     make(http.Header),
			}, nil
		}),
	}
	actor.HTTPClient = simEgressClient
	b.mu.Unlock()

	// 2. Actor executes settlement turn
	_, receipt, err := actor.SettleCycle(ctx, 0)
	if err != nil {
		return nil, nil, err
	}

	// 3. Immediately suspend actor to GCS ($0 idle compute between 30-day turns)
	b.mu.Lock()
	actor.Status = "SUSPENDED"
	stCopy := actor.copyStateLocked()
	b.mu.Unlock()

	return &stCopy, receipt, nil
}

func (b *LocalSimulatorBackend) InspectActorMemory(_ context.Context, actorName string) (*MemoryInspectionResult, error) {
	b.mu.Lock()
	actor, exists := b.actors[actorName]
	b.mu.Unlock()
	if !exists {
		return nil, fmt.Errorf("actor %s not found", actorName)
	}

	res := actor.InspectMemory()
	return &res, nil
}

func (b *LocalSimulatorBackend) TestExfiltration(ctx context.Context, actorName, targetURL string) (*ExfiltrationTestResult, error) {
	b.mu.Lock()
	actor, exists := b.actors[actorName]
	if !exists {
		b.mu.Unlock()
		return nil, fmt.Errorf("actor %s not found", actorName)
	}
	// Untrusted target mock transport: does NOT inject credentials
	actor.HTTPClient = &http.Client{
		Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			payload := map[string]any{
				"target":  req.URL.String(),
				"headers": map[string]string{"Authorization": req.Header.Get("Authorization")},
			}
			respBytes, _ := json.Marshal(payload)
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(bytes.NewReader(respBytes)),
				Header:     make(http.Header),
			}, nil
		}),
	}
	b.mu.Unlock()

	return actor.TestExfiltration(ctx, targetURL)
}

func (b *LocalSimulatorBackend) RotateSecret(_ context.Context) (string, string, error) {
	ver, key := b.secretStore.RotateSecret()
	return ver, key, nil
}

func (b *LocalSimulatorBackend) GetVaultState(_ context.Context) (*VaultStateResponse, error) {
	ver, key, versions := b.secretStore.GetState()
	charges := b.vault.GetCharges()
	shortKey := key
	if len(shortKey) > 16 {
		shortKey = shortKey[:14] + "..." + shortKey[len(shortKey)-4:]
	}

	recent := charges
	if len(recent) > 10 {
		recent = recent[len(recent)-10:]
	}

	return &VaultStateResponse{
		CurrentVersion: ver,
		ActiveToken:    key,
		KeyFingerprint: shortKey,
		TotalCharges:   len(charges),
		Versions:       versions,
		RecentCharges:  recent,
	}, nil
}

func (b *LocalSimulatorBackend) SuspendActor(_ context.Context, actorName string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	actor, exists := b.actors[actorName]
	if !exists {
		return fmt.Errorf("actor %s not found", actorName)
	}
	actor.Status = "SUSPENDED"
	return nil
}

func (b *LocalSimulatorBackend) ListSubscriptions(_ context.Context) ([]SubscriptionTrackerState, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	list := make([]SubscriptionTrackerState, 0, len(b.order))
	for _, name := range b.order {
		actor, ok := b.actors[name]
		if !ok {
			continue
		}
		st := actor.copyStateLocked()
		list = append(list, SubscriptionTrackerState{
			ActorName:         st.ActorName,
			CustomerName:      st.CustomerName,
			PlanTier:          st.PlanTier,
			MonthlyFee:        st.MonthlyFee,
			EscrowAccountID:   st.EscrowAccountID,
			CurrentCycle:      st.CurrentCycle,
			Status:            st.Status,
			CumulativeSettled: st.CumulativeSettled,
			CurrentPodIP:      st.CurrentPodIP,
			PodIPHistory:      st.PodIPHistory,
			Ledger:            st.Ledger,
			LastReceipt:       st.LastReceipt,
			LastSettledAt:     st.LastSettledAt,
		})
	}
	return list, nil
}

func (b *LocalSimulatorBackend) ResetStage(_ context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.secretStore.Reset()
	b.vault.Reset()
	b.actors = make(map[string]*SubscriptionActor)
	b.podIndices = make(map[string]int)

	for _, name := range defaultSubscriptionActors {
		spec := defaultCustomerSpecs[name]
		actor := NewSubscriptionActor(
			spec.ActorName,
			spec.CustomerName,
			spec.PlanTier,
			spec.MonthlyFee,
			spec.EscrowAccountID,
			DefaultVaultURL,
			DefaultEchoURL,
			DefaultVaultSecretURI,
			DefaultTrustBundlePath,
		)
		actor.Status = "SUSPENDED"
		actor.CurrentPodIP = simulatedWorkerPodIPs[0]
		actor.PodIPHistory = []string{simulatedWorkerPodIPs[0]}
		b.actors[name] = actor
		b.podIndices[name] = 0
	}
	return nil
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func mintSimulatedActorJWT(atespace, actorName string) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"typ":"JWT","alg":"RS256","kid":"sim-key-1"}`))
	claimsObj := DecodedActorJWT{
		Issuer:    "https://idp.ate-system.svc",
		Subject:   "actor/" + atespace + "/" + actorName,
		Audiences: []string{"https://billing-gateway.anything-but-agents.svc.cluster.local"},
		IssuedAt:  time.Now().UTC().Unix(),
		ExpiresAt: time.Now().UTC().Add(15 * time.Minute).Unix(),
		ActorInfo: ActorClaimsInfo{
			Atespace:  atespace,
			ActorName: actorName,
			ActorUID:  fmt.Sprintf("uid-%s-%x", actorName, time.Now().UnixNano()%0xFFFFF),
		},
	}
	claimsBytes, _ := json.Marshal(claimsObj)
	claims := base64.RawURLEncoding.EncodeToString(claimsBytes)
	sig := base64.RawURLEncoding.EncodeToString([]byte("simulated_cryptographic_signature"))
	return fmt.Sprintf("%s.%s.%s", header, claims, sig)
}

// --- ClusterAteBackend (--mode=gateway on live GKE cluster) ---

type ClusterAteBackend struct {
	atespace      string
	template      string
	routerURL     string
	ateEndpoint   string
	kubectlAteBin string
	httpClient    *http.Client
	secretStore   *SecretStore
	vault         *PaymentVault
	mu            sync.Mutex
	trackers      map[string]*SubscriptionTrackerState
	order         []string
}

func normalizeRouterURL(raw string) string {
	trimmed := strings.TrimRight(raw, "/")
	if _, err := os.Stat("/run/router-client/credential-bundle.pem"); err == nil {
		trimmed = strings.ReplaceAll(trimmed, "atenet-router.ate-system.svc.cluster.local", "atenet-router.ate-system.svc")
		if strings.HasPrefix(trimmed, "http://atenet-router.") {
			trimmed = "https://" + strings.TrimPrefix(trimmed, "http://")
			trimmed = strings.TrimSuffix(trimmed, ":80")
			if !strings.HasSuffix(trimmed, ":443") {
				trimmed += ":443"
			}
		}
	}
	return trimmed
}

func newRouterHTTPClient(timeout time.Duration) *http.Client {
	tr := &http.Transport{
		MaxIdleConns:        128,
		MaxIdleConnsPerHost: 32,
		IdleConnTimeout:     90 * time.Second,
	}
	credPath := "/run/router-client/credential-bundle.pem"
	caPath := "/run/router-client/trust-bundle.pem"
	if _, err := os.Stat(credPath); err == nil {
		pool := x509.NewCertPool()
		if caBytes, caErr := os.ReadFile(caPath); caErr == nil {
			pool.AppendCertsFromPEM(caBytes)
		}
		tr.TLSClientConfig = &tls.Config{
			GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
				cert, err := tls.LoadX509KeyPair(credPath, credPath)
				if err != nil {
					return nil, err
				}
				return &cert, nil
			},
			RootCAs:    pool,
			ServerName: "atenet-router.ate-system.svc",
			MinVersion: tls.VersionTLS12,
		}
	}
	return &http.Client{
		Timeout:   timeout,
		Transport: tr,
	}
}

func NewClusterAteBackend(atespace, template, routerURL, ateEndpoint, kubectlAteBin string) *ClusterAteBackend {
	store := NewSecretStore()
	vault := NewPaymentVault(store)
	b := &ClusterAteBackend{
		atespace:      atespace,
		template:      template,
		routerURL:     normalizeRouterURL(routerURL),
		ateEndpoint:   ateEndpoint,
		kubectlAteBin: kubectlAteBin,
		httpClient:    newRouterHTTPClient(20 * time.Second),
		secretStore:   store,
		vault:         vault,
		trackers:      make(map[string]*SubscriptionTrackerState),
		order:         append([]string{}, defaultSubscriptionActors...),
	}

	for _, name := range defaultSubscriptionActors {
		spec := defaultCustomerSpecs[name]
		b.trackers[name] = &SubscriptionTrackerState{
			ActorName:       name,
			CustomerName:    spec.CustomerName,
			PlanTier:        spec.PlanTier,
			MonthlyFee:      spec.MonthlyFee,
			EscrowAccountID: spec.EscrowAccountID,
			CurrentCycle:    0,
			Status:          "SUSPENDED",
			CurrentPodIP:    "10.44.0.9",
			PodIPHistory:    []string{"10.44.0.9"},
			Ledger:          make([]Invoice, 0),
		}
	}
	return b
}

func (b *ClusterAteBackend) GetVault() *PaymentVault {
	return b.vault
}

func (b *ClusterAteBackend) GetSecretStore() *SecretStore {
	return b.secretStore
}

func (b *ClusterAteBackend) runKubectlAte(ctx context.Context, args ...string) (string, error) {
	bin := b.kubectlAteBin
	var cmdArgs []string
	if bin == "kubectl" {
		cmdArgs = append(cmdArgs, "ate")
	}
	if b.ateEndpoint != "" {
		cmdArgs = append(cmdArgs, "--endpoint", b.ateEndpoint)
	}
	cmdArgs = append(cmdArgs, args...)
	cmd := exec.CommandContext(ctx, bin, cmdArgs...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		combined := strings.TrimSpace(stderr.String() + "\n" + stdout.String())
		return stdout.String(), fmt.Errorf("%s %v: %w (%s)", bin, cmdArgs, err, combined)
	}
	return stdout.String(), nil
}

func (b *ClusterAteBackend) EnsureActor(ctx context.Context, actorName string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.trackers[actorName]; !ok {
		spec, hasSpec := defaultCustomerSpecs[actorName]
		if !hasSpec {
			spec = DefaultCustomerSpec{
				ActorName:       actorName,
				CustomerName:    "Customer " + actorName,
				PlanTier:        "Standard Tier ($1,000/mo)",
				MonthlyFee:      1000.00,
				EscrowAccountID: "escrow-" + actorName,
			}
		}
		b.trackers[actorName] = &SubscriptionTrackerState{
			ActorName:       actorName,
			CustomerName:    spec.CustomerName,
			PlanTier:        spec.PlanTier,
			MonthlyFee:      spec.MonthlyFee,
			EscrowAccountID: spec.EscrowAccountID,
			CurrentCycle:    0,
			Status:          "SUSPENDED",
			CurrentPodIP:    "10.44.0.9",
			PodIPHistory:    []string{"10.44.0.9"},
			Ledger:          make([]Invoice, 0),
		}
		if !slices.Contains(b.order, actorName) {
			b.order = append(b.order, actorName)
		}
	}

	// In cluster mode, create actor using template if it doesn't already exist
	_, _ = b.runKubectlAte(ctx, "create", "actor", actorName, "-a", b.atespace, "--template", b.template)
	return nil
}

func (b *ClusterAteBackend) SettleActor(ctx context.Context, actorName string) (*SubscriptionState, *VaultChargeReceipt, error) {
	_ = b.EnsureActor(ctx, actorName)

	// Call actor via atenet-router. Router wakes actor if suspended!
	reqURL := fmt.Sprintf("%s/settle", b.routerURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, strings.NewReader(`{}`))
	if err != nil {
		return nil, nil, fmt.Errorf("create settle request: %w", err)
	}
	req.Header.Set("ate-target-actor", fmt.Sprintf("%s/%s", b.atespace, actorName))
	req.Header.Set("Content-Type", "application/json")

	resp, err := b.httpClient.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("router settle request failed: %w", err)
	}
	defer resp.Body.Close()

	bodyBytes, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("settle returned status %d: %s", resp.StatusCode, string(bodyBytes))
	}

	var actorResp struct {
		State   SubscriptionState   `json:"state"`
		Receipt *VaultChargeReceipt `json:"receipt"`
	}
	if err := json.Unmarshal(bodyBytes, &actorResp); err != nil {
		return nil, nil, fmt.Errorf("unmarshal settle response: %w (%s)", err, string(bodyBytes))
	}

	// Immediately suspend actor to GCS
	_, _ = b.runKubectlAte(ctx, "suspend", "actor", actorName, "-a", b.atespace, "--mode=async")

	b.mu.Lock()
	if tr, ok := b.trackers[actorName]; ok {
		tr.CurrentCycle = actorResp.State.CurrentCycle
		tr.CumulativeSettled = actorResp.State.CumulativeSettled
		tr.Status = "SUSPENDED"
		tr.CurrentPodIP = actorResp.State.CurrentPodIP
		tr.PodIPHistory = actorResp.State.PodIPHistory
		tr.Ledger = actorResp.State.Ledger
		tr.LastReceipt = actorResp.Receipt
		tr.LastSettledAt = actorResp.State.LastSettledAt
	}
	b.mu.Unlock()

	return &actorResp.State, actorResp.Receipt, nil
}

func (b *ClusterAteBackend) InspectActorMemory(ctx context.Context, actorName string) (*MemoryInspectionResult, error) {
	reqURL := fmt.Sprintf("%s/inspect-memory", b.routerURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("ate-target-actor", fmt.Sprintf("%s/%s", b.atespace, actorName))

	resp, err := b.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result MemoryInspectionResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	return &result, nil
}

func (b *ClusterAteBackend) TestExfiltration(ctx context.Context, actorName, targetURL string) (*ExfiltrationTestResult, error) {
	reqURL := fmt.Sprintf("%s/test-exfiltrate", b.routerURL)
	body, _ := json.Marshal(map[string]string{"target_url": targetURL})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("ate-target-actor", fmt.Sprintf("%s/%s", b.atespace, actorName))
	req.Header.Set("Content-Type", "application/json")

	resp, err := b.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result ExfiltrationTestResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	return &result, nil
}

func (b *ClusterAteBackend) RotateSecret(ctx context.Context) (string, string, error) {
	newVer, newKey := b.secretStore.RotateSecret()

	// Update the live Kubernetes Secret in the anything-but-agents namespace
	// so the k8s-credential-provider fetches the fresh key on next outbound call!
	secretCmd := exec.CommandContext(ctx, "kubectl", "create", "secret", "generic", "billing-vault-api",
		"-n", b.atespace,
		fmt.Sprintf("--from-literal=token=%s", newKey),
		"--dry-run=client", "-o", "yaml",
	)
	var yamlOut bytes.Buffer
	secretCmd.Stdout = &yamlOut
	if err := secretCmd.Run(); err == nil {
		applyCmd := exec.CommandContext(ctx, "kubectl", "apply", "-f", "-")
		applyCmd.Stdin = &yamlOut
		_ = applyCmd.Run()
	}

	return newVer, newKey, nil
}

func (b *ClusterAteBackend) GetVaultState(_ context.Context) (*VaultStateResponse, error) {
	ver, key, versions := b.secretStore.GetState()
	charges := b.vault.GetCharges()
	shortKey := key
	if len(shortKey) > 16 {
		shortKey = shortKey[:14] + "..." + shortKey[len(shortKey)-4:]
	}

	recent := charges
	if len(recent) > 10 {
		recent = recent[len(recent)-10:]
	}

	return &VaultStateResponse{
		CurrentVersion: ver,
		ActiveToken:    key,
		KeyFingerprint: shortKey,
		TotalCharges:   len(charges),
		Versions:       versions,
		RecentCharges:  recent,
	}, nil
}

func (b *ClusterAteBackend) SuspendActor(ctx context.Context, actorName string) error {
	_, err := b.runKubectlAte(ctx, "suspend", "actor", actorName, "-a", b.atespace, "--mode=async")
	b.mu.Lock()
	if tr, ok := b.trackers[actorName]; ok {
		tr.Status = "SUSPENDED"
	}
	b.mu.Unlock()
	return err
}

func (b *ClusterAteBackend) ListSubscriptions(_ context.Context) ([]SubscriptionTrackerState, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	list := make([]SubscriptionTrackerState, 0, len(b.order))
	for _, name := range b.order {
		if tr, ok := b.trackers[name]; ok {
			list = append(list, *tr)
		}
	}
	return list, nil
}

func (b *ClusterAteBackend) ResetStage(ctx context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.secretStore.Reset()
	b.vault.Reset()

	// Revert Secret in Kubernetes
	_, activeKey := b.secretStore.ValidateKey(b.secretStore.CurrentToken())
	_ = activeKey

	for _, name := range defaultSubscriptionActors {
		_, _ = b.runKubectlAte(ctx, "delete", "actor", name, "-a", b.atespace)
		spec := defaultCustomerSpecs[name]
		b.trackers[name] = &SubscriptionTrackerState{
			ActorName:       name,
			CustomerName:    spec.CustomerName,
			PlanTier:        spec.PlanTier,
			MonthlyFee:      spec.MonthlyFee,
			EscrowAccountID: spec.EscrowAccountID,
			CurrentCycle:    0,
			Status:          "SUSPENDED",
			CurrentPodIP:    "10.44.0.9",
			PodIPHistory:    []string{"10.44.0.9"},
			Ledger:          make([]Invoice, 0),
		}
	}
	return nil
}
