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
	"math/rand"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// CanvasBackend defines the control plane contract for the collaborative canvas gateway.
type CanvasBackend interface {
	GetCanvasState(ctx context.Context, principal, docID string) (*CanvasStateResponse, error)
	MutateCanvas(ctx context.Context, principal, docID string, op CanvasOp) (*CanvasStateResponse, error)
	GrantRole(ctx context.Context, principal, targetUser, role, docID string) (*PerimeterLogEntry, error)
	RevokeRole(ctx context.Context, principal, targetUser, docID string) (*PerimeterLogEntry, error)
	SuspendDocument(ctx context.Context, principal, docID string) (*PerimeterLogEntry, error)
	WakeDocument(ctx context.Context, principal, docID string) (*CanvasStateResponse, error)
	ResetDemo(ctx context.Context) error
	GetPolicyYAML(ctx context.Context, docID string) (string, error)
	GetMetrics() PerimeterMetrics
	GetDecisionLog() []PerimeterLogEntry
}

// LocalSimulatorBackend provides an in-memory simulation of Substrate actors,
// OpenFGA AccessPolicy evaluation, and perimeter wakeup gating.
type LocalSimulatorBackend struct {
	mu           sync.Mutex
	atespace     string
	template     string
	actors       map[string]*CanvasActor
	fga          *OpenFGAEngine
	metrics      PerimeterMetrics
	decisionLog  []PerimeterLogEntry
	mockWorkerIPs []string
}

// NewLocalSimulatorBackend creates a local simulator backend.
func NewLocalSimulatorBackend(atespace, template string) *LocalSimulatorBackend {
	b := &LocalSimulatorBackend{
		atespace:      atespace,
		template:      template,
		actors:        make(map[string]*CanvasActor),
		fga:           NewOpenFGAEngine(atespace),
		mockWorkerIPs: []string{"10.120.1.14", "10.120.2.28", "10.120.3.42"},
	}
	b.initActors()
	return b
}

func (b *LocalSimulatorBackend) initActors() {
	for _, entry := range DefaultDocumentCatalog {
		b.actors[entry.DocID] = NewCanvasActor(entry.DocID)
	}
	// Initial state: doc-merger-acquisition starts SUSPENDED to showcase denied-before-wakeup out of the box!
	if actor, ok := b.actors[DocMergerAcquisition]; ok {
		actor.Suspend()
	}
	b.fga.Reset()
	b.metrics = PerimeterMetrics{
		ActiveTuplesCount: len(b.fga.AllTuples()),
	}
	b.decisionLog = []PerimeterLogEntry{
		{
			Timestamp:       time.Now(),
			Principal:       PrincipalAlice,
			Action:          "bootstrap_policy",
			Resource:        fmt.Sprintf("actor:%s/*", b.atespace),
			Role:            RoleOwner,
			Decision:        "POLICY_UPDATE",
			Reason:          "Loaded default OpenFGA AccessPolicy tuples into perimeter gate",
			ActorState:      "INITIALIZED",
			ActorWoken:      false,
			ComputeWastedMs: 0,
			LatencyMs:       1,
		},
		{
			Timestamp:       time.Now(),
			Principal:       "system:substrate-controller",
			Action:          "suspend_idle",
			Resource:        fmt.Sprintf("actor:%s/%s", b.atespace, DocMergerAcquisition),
			Decision:        "ALLOWED",
			Reason:          "Document actor idle in GCS (ACTOR_STATE_SUSPENDED, 0 slots held)",
			ActorState:      ActorStateSuspended,
			ActorWoken:      false,
			ComputeWastedMs: 0,
			LatencyMs:       1,
		},
	}
}

func (b *LocalSimulatorBackend) recordLog(entry PerimeterLogEntry) {
	b.decisionLog = append(b.decisionLog, entry)
	if len(b.decisionLog) > 100 {
		b.decisionLog = b.decisionLog[len(b.decisionLog)-100:]
	}
}

func (b *LocalSimulatorBackend) GetMetrics() PerimeterMetrics {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.metrics.ActiveTuplesCount = len(b.fga.AllTuples())
	return b.metrics
}

func (b *LocalSimulatorBackend) GetDecisionLog() []PerimeterLogEntry {
	b.mu.Lock()
	defer b.mu.Unlock()
	logCopy := make([]PerimeterLogEntry, len(b.decisionLog))
	copy(logCopy, b.decisionLog)
	return logCopy
}

func (b *LocalSimulatorBackend) GetPolicyYAML(_ context.Context, docID string) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.fga.GenerateAccessPolicyYAML(docID), nil
}

func (b *LocalSimulatorBackend) GetCanvasState(ctx context.Context, principal, docID string) (*CanvasStateResponse, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	actor, ok := b.actors[docID]
	if !ok {
		return nil, fmt.Errorf("document actor %s not found", docID)
	}

	start := time.Now()
	allowed, role, reason := b.fga.Check(principal, PermCanGet, docID)
	latencyMs := int(time.Since(start).Milliseconds())

	actorState := actor.state

	// DENIED CASE: Check if denied before wakeup!
	if !allowed {
		b.metrics.TotalBlockedUnauthorized++
		computeWasted := 0
		if actorState == ActorStateSuspended {
			b.metrics.BlockedWhileAsleep++
			// Saved compute: estimate ~500ms startup + 256MB container provisioning saved
			b.metrics.ComputeSavedSeconds += 0.50
		}

		logEntry := PerimeterLogEntry{
			Timestamp:       time.Now(),
			Principal:       principal,
			Action:          PermCanGet,
			Resource:        fmt.Sprintf("actor:%s/%s", b.atespace, docID),
			Role:            role,
			Decision:        "DENIED_AT_PERIMETER",
			Reason:          reason,
			ActorState:      actorState,
			ActorWoken:      false,
			ComputeWastedMs: computeWasted,
			LatencyMs:       latencyMs,
		}
		b.recordLog(logEntry)

		return &CanvasStateResponse{
			DocID:                docID,
			Principal:            principal,
			PrincipalRole:        role,
			CanEdit:              false,
			CanManage:            false,
			Tuples:               b.fga.ListTuples(docID),
			Metrics:              b.metrics,
			DecisionLog:          b.decisionLog,
			PolicyYAML:           b.fga.GenerateAccessPolicyYAML(docID),
			ActorState:           actorState,
			IsDeniedBeforeWakeup: true,
			DenialReason:         reason,
		}, nil
	}

	// ALLOWED CASE: If suspended, wake it up!
	actorWoken := false
	if actorState == ActorStateSuspended {
		// Simulate fast Substrate memory wake-on-HTTP (~80-110ms)
		time.Sleep(20 * time.Millisecond)
		newIP := b.mockWorkerIPs[rand.Intn(len(b.mockWorkerIPs))]
		actor.Wake(newIP)
		actorState = ActorStateRunning
		actorWoken = true
		b.metrics.WakeupsOnValidTraffic++
	}

	b.metrics.TotalAllowedCalls++
	logEntry := PerimeterLogEntry{
		Timestamp:       time.Now(),
		Principal:       principal,
		Action:          PermCanGet,
		Resource:        fmt.Sprintf("actor:%s/%s", b.atespace, docID),
		Role:            role,
		Decision:        "ALLOWED",
		Reason:          reason,
		ActorState:      actorState,
		ActorWoken:      actorWoken,
		ComputeWastedMs: 0,
		LatencyMs:       latencyMs + int(time.Since(start).Milliseconds()),
	}
	b.recordLog(logEntry)

	sg := actor.BuildSceneGraph()
	canEdit := (role == RoleOwner || role == RoleEditor)
	canManage := (role == RoleOwner)

	return &CanvasStateResponse{
		Scene:                &sg,
		DocID:                docID,
		Principal:            principal,
		PrincipalRole:        role,
		CanEdit:              canEdit,
		CanManage:            canManage,
		Tuples:               b.fga.ListTuples(docID),
		Metrics:              b.metrics,
		DecisionLog:          b.decisionLog,
		PolicyYAML:           b.fga.GenerateAccessPolicyYAML(docID),
		ActorState:           actorState,
		IsDeniedBeforeWakeup: false,
	}, nil
}

func (b *LocalSimulatorBackend) MutateCanvas(ctx context.Context, principal, docID string, op CanvasOp) (*CanvasStateResponse, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	actor, ok := b.actors[docID]
	if !ok {
		return nil, fmt.Errorf("document actor %s not found", docID)
	}

	start := time.Now()
	// Mutation requires can_update permission
	allowed, role, reason := b.fga.Check(principal, PermCanUpdate, docID)
	latencyMs := int(time.Since(start).Milliseconds())

	actorState := actor.state

	if !allowed {
		b.metrics.TotalBlockedUnauthorized++
		if actorState == ActorStateSuspended {
			b.metrics.BlockedWhileAsleep++
			b.metrics.ComputeSavedSeconds += 0.50
		}

		logEntry := PerimeterLogEntry{
			Timestamp:       time.Now(),
			Principal:       principal,
			Action:          PermCanUpdate,
			Resource:        fmt.Sprintf("actor:%s/%s", b.atespace, docID),
			Role:            role,
			Decision:        "DENIED_AT_PERIMETER",
			Reason:          reason,
			ActorState:      actorState,
			ActorWoken:      false,
			ComputeWastedMs: 0,
			LatencyMs:       latencyMs,
		}
		b.recordLog(logEntry)

		return &CanvasStateResponse{
			DocID:                docID,
			Principal:            principal,
			PrincipalRole:        role,
			CanEdit:              false,
			CanManage:            false,
			Tuples:               b.fga.ListTuples(docID),
			Metrics:              b.metrics,
			DecisionLog:          b.decisionLog,
			PolicyYAML:           b.fga.GenerateAccessPolicyYAML(docID),
			ActorState:           actorState,
			IsDeniedBeforeWakeup: (actorState == ActorStateSuspended),
			DenialReason:         reason,
		}, fmt.Errorf("permission denied: %s", reason)
	}

	// Wake if sleeping
	actorWoken := false
	if actorState == ActorStateSuspended {
		newIP := b.mockWorkerIPs[rand.Intn(len(b.mockWorkerIPs))]
		actor.Wake(newIP)
		actorState = ActorStateRunning
		actorWoken = true
		b.metrics.WakeupsOnValidTraffic++
	}

	// Apply mutation directly to Go heap RAM in actor
	op.Principal = principal
	sg, err := actor.Mutate(op)
	if err != nil {
		return nil, err
	}

	b.metrics.TotalAllowedCalls++
	logEntry := PerimeterLogEntry{
		Timestamp:       time.Now(),
		Principal:       principal,
		Action:          fmt.Sprintf("mutate:%s", op.Op),
		Resource:        fmt.Sprintf("actor:%s/%s", b.atespace, docID),
		Role:            role,
		Decision:        "ALLOWED",
		Reason:          fmt.Sprintf("OpenFGA permitted %s (holding '%s')", PermCanUpdate, role),
		ActorState:      actorState,
		ActorWoken:      actorWoken,
		ComputeWastedMs: 0,
		LatencyMs:       latencyMs + int(time.Since(start).Milliseconds()),
	}
	b.recordLog(logEntry)

	return &CanvasStateResponse{
		Scene:                &sg,
		DocID:                docID,
		Principal:            principal,
		PrincipalRole:        role,
		CanEdit:              true,
		CanManage:            (role == RoleOwner),
		Tuples:               b.fga.ListTuples(docID),
		Metrics:              b.metrics,
		DecisionLog:          b.decisionLog,
		PolicyYAML:           b.fga.GenerateAccessPolicyYAML(docID),
		ActorState:           actorState,
		IsDeniedBeforeWakeup: false,
	}, nil
}

func (b *LocalSimulatorBackend) GrantRole(_ context.Context, principal, targetUser, role, docID string) (*PerimeterLogEntry, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	allowed, callerRole, reason := b.fga.Check(principal, PermCanManagePolicy, docID)
	if !allowed {
		b.metrics.TotalBlockedUnauthorized++
		entry := PerimeterLogEntry{
			Timestamp: time.Now(),
			Principal: principal,
			Action:    PermCanManagePolicy,
			Resource:  fmt.Sprintf("actor:%s/%s", b.atespace, docID),
			Role:      callerRole,
			Decision:  "DENIED_AT_PERIMETER",
			Reason:    fmt.Sprintf("Only document owner can grant roles: %s", reason),
		}
		b.recordLog(entry)
		return &entry, fmt.Errorf("permission denied: %s", reason)
	}

	if err := b.fga.Grant(targetUser, role, docID); err != nil {
		return nil, err
	}

	b.metrics.ActiveTuplesCount = len(b.fga.AllTuples())
	entry := PerimeterLogEntry{
		Timestamp: time.Now(),
		Principal: principal,
		Action:    "grant_role",
		Resource:  fmt.Sprintf("actor:%s/%s", b.atespace, docID),
		Role:      callerRole,
		Decision:  "POLICY_UPDATE",
		Reason:    fmt.Sprintf("Granted '%s' role to %s (applied live to OpenFGA AccessPolicy)", role, targetUser),
	}
	b.recordLog(entry)
	return &entry, nil
}

func (b *LocalSimulatorBackend) RevokeRole(_ context.Context, principal, targetUser, docID string) (*PerimeterLogEntry, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	allowed, callerRole, reason := b.fga.Check(principal, PermCanManagePolicy, docID)
	if !allowed {
		b.metrics.TotalBlockedUnauthorized++
		entry := PerimeterLogEntry{
			Timestamp: time.Now(),
			Principal: principal,
			Action:    PermCanManagePolicy,
			Resource:  fmt.Sprintf("actor:%s/%s", b.atespace, docID),
			Role:      callerRole,
			Decision:  "DENIED_AT_PERIMETER",
			Reason:    fmt.Sprintf("Only document owner can revoke roles: %s", reason),
		}
		b.recordLog(entry)
		return &entry, fmt.Errorf("permission denied: %s", reason)
	}

	if err := b.fga.Revoke(targetUser, docID); err != nil {
		return nil, err
	}

	b.metrics.ActiveTuplesCount = len(b.fga.AllTuples())
	entry := PerimeterLogEntry{
		Timestamp: time.Now(),
		Principal: principal,
		Action:    "revoke_role",
		Resource:  fmt.Sprintf("actor:%s/%s", b.atespace, docID),
		Role:      callerRole,
		Decision:  "POLICY_UPDATE",
		Reason:    fmt.Sprintf("Revoked tuple for %s (removed binding from OpenFGA AccessPolicy)", targetUser),
	}
	b.recordLog(entry)
	return &entry, nil
}

func (b *LocalSimulatorBackend) SuspendDocument(_ context.Context, principal, docID string) (*PerimeterLogEntry, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	allowed, callerRole, reason := b.fga.Check(principal, PermCanSuspend, docID)
	if !allowed {
		b.metrics.TotalBlockedUnauthorized++
		entry := PerimeterLogEntry{
			Timestamp: time.Now(),
			Principal: principal,
			Action:    PermCanSuspend,
			Resource:  fmt.Sprintf("actor:%s/%s", b.atespace, docID),
			Role:      callerRole,
			Decision:  "DENIED_AT_PERIMETER",
			Reason:    reason,
		}
		b.recordLog(entry)
		return &entry, fmt.Errorf("permission denied: %s", reason)
	}

	actor, ok := b.actors[docID]
	if !ok {
		return nil, fmt.Errorf("actor %s not found", docID)
	}

	actor.Suspend()
	entry := PerimeterLogEntry{
		Timestamp:  time.Now(),
		Principal:  principal,
		Action:     "suspend_actor",
		Resource:   fmt.Sprintf("actor:%s/%s", b.atespace, docID),
		Role:       callerRole,
		Decision:   "ALLOWED",
		Reason:     "Actor checkpointed to GCS (ACTOR_STATE_SUSPENDED, 0/3 worker slots held)",
		ActorState: ActorStateSuspended,
		ActorWoken: false,
	}
	b.recordLog(entry)
	return &entry, nil
}

func (b *LocalSimulatorBackend) WakeDocument(_ context.Context, principal, docID string) (*CanvasStateResponse, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	allowed, callerRole, reason := b.fga.Check(principal, PermCanResume, docID)
	if !allowed {
		b.metrics.TotalBlockedUnauthorized++
		entry := PerimeterLogEntry{
			Timestamp: time.Now(),
			Principal: principal,
			Action:    PermCanResume,
			Resource:  fmt.Sprintf("actor:%s/%s", b.atespace, docID),
			Role:      callerRole,
			Decision:  "DENIED_AT_PERIMETER",
			Reason:    reason,
		}
		b.recordLog(entry)
		return nil, fmt.Errorf("permission denied: %s", reason)
	}

	actor, ok := b.actors[docID]
	if !ok {
		return nil, fmt.Errorf("actor %s not found", docID)
	}

	newIP := b.mockWorkerIPs[rand.Intn(len(b.mockWorkerIPs))]
	actor.Wake(newIP)
	b.metrics.WakeupsOnValidTraffic++

	entry := PerimeterLogEntry{
		Timestamp:  time.Now(),
		Principal:  principal,
		Action:     "wake_actor",
		Resource:   fmt.Sprintf("actor:%s/%s", b.atespace, docID),
		Role:       callerRole,
		Decision:   "ALLOWED",
		Reason:     fmt.Sprintf("Actor restored from GCS in ~98ms (now RUNNING on %s)", newIP),
		ActorState: ActorStateRunning,
		ActorWoken: true,
	}
	b.recordLog(entry)

	sg := actor.BuildSceneGraph()
	return &CanvasStateResponse{
		Scene:                &sg,
		DocID:                docID,
		Principal:            principal,
		PrincipalRole:        callerRole,
		CanEdit:              (callerRole == RoleOwner || callerRole == RoleEditor),
		CanManage:            (callerRole == RoleOwner),
		Tuples:               b.fga.ListTuples(docID),
		Metrics:              b.metrics,
		DecisionLog:          b.decisionLog,
		PolicyYAML:           b.fga.GenerateAccessPolicyYAML(docID),
		ActorState:           ActorStateRunning,
		IsDeniedBeforeWakeup: false,
	}, nil
}

func (b *LocalSimulatorBackend) ResetDemo(_ context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.initActors()
	return nil
}

// ClusterAteBackend connects to real Agent Substrate cluster components:
// atenet-router for wake-on-HTTP dispatching and kubectl-ate for actor lifecycle.
// It also enforces the OpenFGA perimeter check before dispatching to the cluster router.
type ClusterAteBackend struct {
	mu            sync.Mutex
	atespace      string
	template      string
	routerURL     string
	ateEndpoint   string
	kubectlAte    string
	fga           *OpenFGAEngine
	metrics       PerimeterMetrics
	decisionLog   []PerimeterLogEntry
	httpClient    *http.Client
	actorStates   map[string]string // Tracks local known actor state (RUNNING | SUSPENDED)
	createdActors map[string]bool   // Tracks whether actor has been created via kubectl-ate
}

// NewClusterAteBackend creates a cluster backend.
func NewClusterAteBackend(atespace, template, routerURL, ateEndpoint, kubectlAte string) *ClusterAteBackend {
	b := &ClusterAteBackend{
		atespace:      atespace,
		template:      template,
		routerURL:     routerURL,
		ateEndpoint:   ateEndpoint,
		kubectlAte:    kubectlAte,
		fga:           NewOpenFGAEngine(atespace),
		actorStates:   make(map[string]string),
		createdActors: make(map[string]bool),
	}
	b.httpClient = b.buildHTTPClient()
	b.initClusterState()
	return b
}

func (b *ClusterAteBackend) initClusterState() {
	b.fga.Reset()
	b.metrics = PerimeterMetrics{
		ActiveTuplesCount: len(b.fga.AllTuples()),
	}
	b.createdActors = make(map[string]bool)
	for _, entry := range DefaultDocumentCatalog {
		b.actorStates[entry.DocID] = ActorStateRunning
	}
	// Default doc-merger-acquisition to suspended for initial demo setup
	b.actorStates[DocMergerAcquisition] = ActorStateSuspended

	b.decisionLog = []PerimeterLogEntry{
		{
			Timestamp:  time.Now(),
			Principal:  PrincipalAlice,
			Action:     "bootstrap_cluster_policy",
			Resource:   fmt.Sprintf("actor:%s/*", b.atespace),
			Role:       RoleOwner,
			Decision:   "POLICY_UPDATE",
			Reason:     "Initialized OpenFGA AccessPolicy perimeter evaluator in cluster gateway",
			ActorState: "INITIALIZED",
		},
	}
}

func (b *ClusterAteBackend) ensureActor(ctx context.Context, docID string) error {
	b.mu.Lock()
	if b.createdActors[docID] {
		b.mu.Unlock()
		return nil
	}
	b.mu.Unlock()

	args := []string{}
	if b.ateEndpoint != "" {
		args = append(args, "--endpoint", b.ateEndpoint)
	}
	args = append(args, "create", "actor", docID, "-a", b.atespace, "--template", b.template)

	cmd := exec.CommandContext(ctx, b.kubectlAte, args...)
	out, err := cmd.CombinedOutput()
	outStr := strings.ToLower(string(out))
	if err != nil && !strings.Contains(outStr, "already exists") && !strings.Contains(outStr, "alreadyexists") {
		return fmt.Errorf("kubectl-ate create actor %s failed: %v (%s)", docID, err, strings.TrimSpace(string(out)))
	}

	b.mu.Lock()
	b.createdActors[docID] = true
	b.mu.Unlock()
	return nil
}

func (b *ClusterAteBackend) buildHTTPClient() *http.Client {
	tlsConfig := &tls.Config{
		MinVersion: tls.VersionTLS13,
	}

	// Load podCertificate and clusterTrustBundle if available
	certDir := "/run/router-client"
	credBundle := filepath.Join(certDir, "credential-bundle.pem")
	trustBundle := filepath.Join(certDir, "trust-bundle.pem")

	if _, err := os.Stat(credBundle); err == nil {
		tlsConfig.GetClientCertificate = func(_ *tls.CertificateRequestInfo) (*tls.Certificate, error) {
			cert, err := tls.LoadX509KeyPair(credBundle, credBundle)
			if err != nil {
				return nil, err
			}
			return &cert, nil
		}
	}

	if tbBytes, err := os.ReadFile(trustBundle); err == nil {
		rootCAs := x509.NewCertPool()
		if rootCAs.AppendCertsFromPEM(tbBytes) {
			tlsConfig.RootCAs = rootCAs
		}
	} else {
		// Fallback for cluster internal services
		tlsConfig.InsecureSkipVerify = true
	}

	return &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: tlsConfig,
		},
		Timeout: 30 * time.Second,
	}
}

func (b *ClusterAteBackend) recordLog(entry PerimeterLogEntry) {
	b.decisionLog = append(b.decisionLog, entry)
	if len(b.decisionLog) > 100 {
		b.decisionLog = b.decisionLog[len(b.decisionLog)-100:]
	}
}

func (b *ClusterAteBackend) GetMetrics() PerimeterMetrics {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.metrics.ActiveTuplesCount = len(b.fga.AllTuples())
	return b.metrics
}

func (b *ClusterAteBackend) GetDecisionLog() []PerimeterLogEntry {
	b.mu.Lock()
	defer b.mu.Unlock()
	logCopy := make([]PerimeterLogEntry, len(b.decisionLog))
	copy(logCopy, b.decisionLog)
	return logCopy
}

func (b *ClusterAteBackend) GetPolicyYAML(_ context.Context, docID string) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.fga.GenerateAccessPolicyYAML(docID), nil
}

func (b *ClusterAteBackend) GetCanvasState(ctx context.Context, principal, docID string) (*CanvasStateResponse, error) {
	b.mu.Lock()
	start := time.Now()
	allowed, role, reason := b.fga.Check(principal, PermCanGet, docID)
	latencyMs := int(time.Since(start).Milliseconds())

	actorState := b.actorStates[docID]
	if actorState == "" {
		actorState = ActorStateRunning
	}

	// 1. REJECTED AT PERIMETER: Do NOT contact router or wake sleeping actor!
	if !allowed {
		b.metrics.TotalBlockedUnauthorized++
		if actorState == ActorStateSuspended {
			b.metrics.BlockedWhileAsleep++
			b.metrics.ComputeSavedSeconds += 0.50
		}

		logEntry := PerimeterLogEntry{
			Timestamp:       time.Now(),
			Principal:       principal,
			Action:          PermCanGet,
			Resource:        fmt.Sprintf("actor:%s/%s", b.atespace, docID),
			Role:            role,
			Decision:        "DENIED_AT_PERIMETER",
			Reason:          reason,
			ActorState:      actorState,
			ActorWoken:      false,
			ComputeWastedMs: 0,
			LatencyMs:       latencyMs,
		}
		b.recordLog(logEntry)
		b.mu.Unlock()

		return &CanvasStateResponse{
			DocID:                docID,
			Principal:            principal,
			PrincipalRole:        role,
			CanEdit:              false,
			CanManage:            false,
			Tuples:               b.fga.ListTuples(docID),
			Metrics:              b.metrics,
			DecisionLog:          b.decisionLog,
			PolicyYAML:           b.fga.GenerateAccessPolicyYAML(docID),
			ActorState:           actorState,
			IsDeniedBeforeWakeup: true,
			DenialReason:         reason,
		}, nil
	}

	b.mu.Unlock()

	// 2. ALLOWED: Ensure actor exists and dispatch through atenet-router
	if err := b.ensureActor(ctx, docID); err != nil {
		return nil, err
	}

	reqURL := fmt.Sprintf("%s/canvas", strings.TrimRight(b.routerURL, "/"))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("ate-target-actor", fmt.Sprintf("%s/%s", b.atespace, docID))
	req.Header.Set("X-Principal", principal)

	resp, err := b.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed calling router for actor %s: %w", docID, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("actor returned http %d: %s", resp.StatusCode, string(body))
	}

	var sg SceneGraph
	if err := json.NewDecoder(resp.Body).Decode(&sg); err != nil {
		return nil, fmt.Errorf("failed decoding scene graph: %w", err)
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	b.actorStates[docID] = ActorStateRunning
	b.metrics.TotalAllowedCalls++

	logEntry := PerimeterLogEntry{
		Timestamp:       time.Now(),
		Principal:       principal,
		Action:          PermCanGet,
		Resource:        fmt.Sprintf("actor:%s/%s", b.atespace, docID),
		Role:            role,
		Decision:        "ALLOWED",
		Reason:          reason,
		ActorState:      ActorStateRunning,
		ActorWoken:      (actorState == ActorStateSuspended),
		ComputeWastedMs: 0,
		LatencyMs:       int(time.Since(start).Milliseconds()),
	}
	b.recordLog(logEntry)

	return &CanvasStateResponse{
		Scene:                &sg,
		DocID:                docID,
		Principal:            principal,
		PrincipalRole:        role,
		CanEdit:              (role == RoleOwner || role == RoleEditor),
		CanManage:            (role == RoleOwner),
		Tuples:               b.fga.ListTuples(docID),
		Metrics:              b.metrics,
		DecisionLog:          b.decisionLog,
		PolicyYAML:           b.fga.GenerateAccessPolicyYAML(docID),
		ActorState:           ActorStateRunning,
		IsDeniedBeforeWakeup: false,
	}, nil
}

func (b *ClusterAteBackend) MutateCanvas(ctx context.Context, principal, docID string, op CanvasOp) (*CanvasStateResponse, error) {
	b.mu.Lock()
	start := time.Now()
	allowed, role, reason := b.fga.Check(principal, PermCanUpdate, docID)
	latencyMs := int(time.Since(start).Milliseconds())

	actorState := b.actorStates[docID]
	if actorState == "" {
		actorState = ActorStateRunning
	}

	if !allowed {
		b.metrics.TotalBlockedUnauthorized++
		if actorState == ActorStateSuspended {
			b.metrics.BlockedWhileAsleep++
			b.metrics.ComputeSavedSeconds += 0.50
		}

		logEntry := PerimeterLogEntry{
			Timestamp:       time.Now(),
			Principal:       principal,
			Action:          PermCanUpdate,
			Resource:        fmt.Sprintf("actor:%s/%s", b.atespace, docID),
			Role:            role,
			Decision:        "DENIED_AT_PERIMETER",
			Reason:          reason,
			ActorState:      actorState,
			ActorWoken:      false,
			ComputeWastedMs: 0,
			LatencyMs:       latencyMs,
		}
		b.recordLog(logEntry)
		b.mu.Unlock()

		return &CanvasStateResponse{
			DocID:                docID,
			Principal:            principal,
			PrincipalRole:        role,
			CanEdit:              false,
			CanManage:            false,
			Tuples:               b.fga.ListTuples(docID),
			Metrics:              b.metrics,
			DecisionLog:          b.decisionLog,
			PolicyYAML:           b.fga.GenerateAccessPolicyYAML(docID),
			ActorState:           actorState,
			IsDeniedBeforeWakeup: (actorState == ActorStateSuspended),
			DenialReason:         reason,
		}, fmt.Errorf("permission denied: %s", reason)
	}

	b.mu.Unlock()

	if err := b.ensureActor(ctx, docID); err != nil {
		return nil, err
	}

	// Dispatch mutation to router
	var path string
	var payload any
	switch op.Op {
	case "add_node":
		path = "/canvas/node"
		payload = op.Node
	case "move_node":
		path = "/canvas/move"
		payload = op.Move
	case "add_note":
		path = "/canvas/note"
		payload = op.Note
	case "add_edge":
		path = "/canvas/edge"
		payload = op.Edge
	case "delete":
		path = "/canvas/delete"
		payload = op.Delete
	case "reset":
		path = "/canvas/reset"
		payload = map[string]string{}
	default:
		return nil, fmt.Errorf("unknown op: %s", op.Op)
	}

	bodyBytes, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	reqURL := fmt.Sprintf("%s%s", strings.TrimRight(b.routerURL, "/"), path)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("ate-target-actor", fmt.Sprintf("%s/%s", b.atespace, docID))
	req.Header.Set("X-Principal", principal)

	resp, err := b.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("mutation request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("actor returned http %d on mutate: %s", resp.StatusCode, string(respBody))
	}

	var sg SceneGraph
	if err := json.NewDecoder(resp.Body).Decode(&sg); err != nil {
		return nil, fmt.Errorf("failed decoding mutation response: %w", err)
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	b.actorStates[docID] = ActorStateRunning
	b.metrics.TotalAllowedCalls++

	logEntry := PerimeterLogEntry{
		Timestamp:       time.Now(),
		Principal:       principal,
		Action:          fmt.Sprintf("mutate:%s", op.Op),
		Resource:        fmt.Sprintf("actor:%s/%s", b.atespace, docID),
		Role:            role,
		Decision:        "ALLOWED",
		Reason:          fmt.Sprintf("OpenFGA permitted %s (holding '%s')", PermCanUpdate, role),
		ActorState:      ActorStateRunning,
		ActorWoken:      false,
		ComputeWastedMs: 0,
		LatencyMs:       int(time.Since(start).Milliseconds()),
	}
	b.recordLog(logEntry)

	return &CanvasStateResponse{
		Scene:                &sg,
		DocID:                docID,
		Principal:            principal,
		PrincipalRole:        role,
		CanEdit:              true,
		CanManage:            (role == RoleOwner),
		Tuples:               b.fga.ListTuples(docID),
		Metrics:              b.metrics,
		DecisionLog:          b.decisionLog,
		PolicyYAML:           b.fga.GenerateAccessPolicyYAML(docID),
		ActorState:           ActorStateRunning,
		IsDeniedBeforeWakeup: false,
	}, nil
}

func (b *ClusterAteBackend) GrantRole(_ context.Context, principal, targetUser, role, docID string) (*PerimeterLogEntry, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	allowed, callerRole, reason := b.fga.Check(principal, PermCanManagePolicy, docID)
	if !allowed {
		b.metrics.TotalBlockedUnauthorized++
		entry := PerimeterLogEntry{
			Timestamp: time.Now(),
			Principal: principal,
			Action:    PermCanManagePolicy,
			Resource:  fmt.Sprintf("actor:%s/%s", b.atespace, docID),
			Role:      callerRole,
			Decision:  "DENIED_AT_PERIMETER",
			Reason:    fmt.Sprintf("Only document owner can grant roles: %s", reason),
		}
		b.recordLog(entry)
		return &entry, fmt.Errorf("permission denied: %s", reason)
	}

	if err := b.fga.Grant(targetUser, role, docID); err != nil {
		return nil, err
	}

	b.metrics.ActiveTuplesCount = len(b.fga.AllTuples())
	entry := PerimeterLogEntry{
		Timestamp: time.Now(),
		Principal: principal,
		Action:    "grant_role",
		Resource:  fmt.Sprintf("actor:%s/%s", b.atespace, docID),
		Role:      callerRole,
		Decision:  "POLICY_UPDATE",
		Reason:    fmt.Sprintf("Granted '%s' role to %s (applied live to OpenFGA AccessPolicy)", role, targetUser),
	}
	b.recordLog(entry)
	return &entry, nil
}

func (b *ClusterAteBackend) RevokeRole(_ context.Context, principal, targetUser, docID string) (*PerimeterLogEntry, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	allowed, callerRole, reason := b.fga.Check(principal, PermCanManagePolicy, docID)
	if !allowed {
		b.metrics.TotalBlockedUnauthorized++
		entry := PerimeterLogEntry{
			Timestamp: time.Now(),
			Principal: principal,
			Action:    PermCanManagePolicy,
			Resource:  fmt.Sprintf("actor:%s/%s", b.atespace, docID),
			Role:      callerRole,
			Decision:  "DENIED_AT_PERIMETER",
			Reason:    fmt.Sprintf("Only document owner can revoke roles: %s", reason),
		}
		b.recordLog(entry)
		return &entry, fmt.Errorf("permission denied: %s", reason)
	}

	if err := b.fga.Revoke(targetUser, docID); err != nil {
		return nil, err
	}

	b.metrics.ActiveTuplesCount = len(b.fga.AllTuples())
	entry := PerimeterLogEntry{
		Timestamp: time.Now(),
		Principal: principal,
		Action:    "revoke_role",
		Resource:  fmt.Sprintf("actor:%s/%s", b.atespace, docID),
		Role:      callerRole,
		Decision:  "POLICY_UPDATE",
		Reason:    fmt.Sprintf("Revoked tuple for %s (removed binding from OpenFGA AccessPolicy)", targetUser),
	}
	b.recordLog(entry)
	return &entry, nil
}

func (b *ClusterAteBackend) SuspendDocument(ctx context.Context, principal, docID string) (*PerimeterLogEntry, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	allowed, callerRole, reason := b.fga.Check(principal, PermCanSuspend, docID)
	if !allowed {
		b.metrics.TotalBlockedUnauthorized++
		entry := PerimeterLogEntry{
			Timestamp: time.Now(),
			Principal: principal,
			Action:    PermCanSuspend,
			Resource:  fmt.Sprintf("actor:%s/%s", b.atespace, docID),
			Role:      callerRole,
			Decision:  "DENIED_AT_PERIMETER",
			Reason:    reason,
		}
		b.recordLog(entry)
		return &entry, fmt.Errorf("permission denied: %s", reason)
	}

	// Run kubectl-ate suspend actor --mode=async
	args := []string{}
	if b.ateEndpoint != "" {
		args = append(args, "--endpoint", b.ateEndpoint)
	}
	args = append(args, "suspend", "actor", docID, "-a", b.atespace, "--mode=async")
	cmd := exec.CommandContext(ctx, b.kubectlAte, args...)
	_ = cmd.Run()

	b.actorStates[docID] = ActorStateSuspended
	entry := PerimeterLogEntry{
		Timestamp:  time.Now(),
		Principal:  principal,
		Action:     "suspend_actor",
		Resource:   fmt.Sprintf("actor:%s/%s", b.atespace, docID),
		Role:       callerRole,
		Decision:   "ALLOWED",
		Reason:     "Actor checkpointed to GCS (ACTOR_STATE_SUSPENDED, 0 slots held)",
		ActorState: ActorStateSuspended,
		ActorWoken: false,
	}
	b.recordLog(entry)
	return &entry, nil
}

func (b *ClusterAteBackend) WakeDocument(ctx context.Context, principal, docID string) (*CanvasStateResponse, error) {
	// Calling GetCanvasState with authorized principal wakes the actor via router!
	return b.GetCanvasState(ctx, principal, docID)
}

func (b *ClusterAteBackend) ResetDemo(ctx context.Context) error {
	for _, entry := range DefaultDocumentCatalog {
		args := []string{}
		if b.ateEndpoint != "" {
			args = append(args, "--endpoint", b.ateEndpoint)
		}
		args = append(args, "delete", "actor", entry.DocID, "-a", b.atespace)
		cmd := exec.CommandContext(ctx, b.kubectlAte, args...)
		_ = cmd.Run()
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	b.initClusterState()
	return nil
}
