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
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"time"
)

var defaultBuildJobs = []string{
	"build-pr-104",
	"build-release-v2",
	"build-hotfix-88",
}

// BuildboxBackend defines operations against either a local simulation or real Agent Substrate cluster.
type BuildboxBackend interface {
	ListJobs(ctx context.Context) ([]BuildJobTracker, error)
	GetJobState(ctx context.Context, jobName string, wake bool) (*BuildJobTracker, error)
	FetchDeps(ctx context.Context, jobName string) (*BuildJobTracker, error)
	CompileAndTest(ctx context.Context, jobName string) (*BuildJobTracker, error)
	TestRogueEgress(ctx context.Context, jobName string) (*BuildJobTracker, error)
	PublishArtifact(ctx context.Context, jobName string) (*BuildJobTracker, error)
	AutoRunPipeline(ctx context.Context, jobName string) (*BuildJobTracker, error)
	SuspendActor(ctx context.Context, jobName string) error
	ResetJob(ctx context.Context, jobName string) (*BuildJobTracker, error)
	ResetStage(ctx context.Context) (*BuildJobTracker, error)
	GetEgressPolicyYAML(ctx context.Context, jobName string) (string, error)
}

func isValidJobName(name string) bool {
	if len(name) < 3 || len(name) > 64 {
		return false
	}
	for _, ch := range name {
		if !((ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9') || ch == '-') {
			return false
		}
	}
	return true
}

func generateEgressPolicyYAML(atespace, actorName, phase string) string {
	switch phase {
	case PhaseHermeticCompile:
		return fmt.Sprintf(`# Copyright 2026 Google LLC
# SLSA L3 Hermetic Build: Total Airgap (0 allowed rules)
# Actor: %s/%s
# Outbound DNS queries return NXDOMAIN and all TCP egress is refused.
metadata:
  atespace: %s
  name: default
rules: []
`, atespace, actorName, atespace)
	case PhaseArtifactPublish:
		return fmt.Sprintf(`# Copyright 2026 Google LLC
# Phase 3: Artifact Registry Release EgressPolicy
# Actor: %s/%s
# Allows release upload to internal registry while package mirror remains blocked.
metadata:
  atespace: %s
  name: default
rules:
- http:
    hostnames:
    - buildbox-gateway.anything-but-agents.svc.cluster.local
`, atespace, actorName, atespace)
	default:
		// Phase 1 (DEPENDENCY_FETCH) or IDLE
		return fmt.Sprintf(`# Copyright 2026 Google LLC
# Phase 1: Package Mirror EgressPolicy
# Actor: %s/%s
# Allows fetching dependencies from internal mirror only; external hosts denied.
metadata:
  atespace: %s
  name: default
rules:
- http:
    hostnames:
    - buildbox-gateway.anything-but-agents.svc.cluster.local
`, atespace, actorName, atespace)
	}
}

// --- LocalSimulatorBackend (--mode=local & unit tests) ---

var simulatedWorkerPodIPs = []string{"10.44.0.12", "10.44.1.18", "10.44.2.25"}

type localSimulatedJob struct {
	actor   *Buildbox
	status  string // "RUNNING" or "SUSPENDED"
	podIdx  int
	history []string
}

type LocalSimulatorBackend struct {
	mu          sync.Mutex
	atespace    string
	template    string
	mirrorURL   string
	registryURL string
	jobs        map[string]*localSimulatedJob
	order       []string
}

func NewLocalSimulatorBackend(atespace, template string) *LocalSimulatorBackend {
	b := &LocalSimulatorBackend{
		atespace:    atespace,
		template:    template,
		mirrorURL:   "http://localhost:8087",
		registryURL: "http://localhost:8087",
		jobs:        make(map[string]*localSimulatedJob),
	}
	_ = b.ensureDefaultJobs()
	return b
}

func (b *LocalSimulatorBackend) SetUpstreamURLs(mirrorURL, registryURL string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.mirrorURL = mirrorURL
	b.registryURL = registryURL
	for _, j := range b.jobs {
		j.actor.mirrorURL = mirrorURL
		j.actor.registryURL = registryURL
	}
}

func (b *LocalSimulatorBackend) ensureDefaultJobs() error {
	for i, name := range defaultBuildJobs {
		if _, exists := b.jobs[name]; !exists {
			actor := NewBuildbox(name, b.mirrorURL, b.registryURL)
			podIP := simulatedWorkerPodIPs[i%len(simulatedWorkerPodIPs)]
			actor.currentPodIP = podIP
			actor.podIPHistory = []string{podIP}
			status := "SUSPENDED"
			if i == 0 {
				status = "RUNNING"
			}
			b.jobs[name] = &localSimulatedJob{
				actor:   actor,
				status:  status,
				podIdx:  i % len(simulatedWorkerPodIPs),
				history: []string{podIP},
			}
			b.order = append(b.order, name)
		}
	}
	return nil
}

func (b *LocalSimulatorBackend) getOrInitJobLocked(jobName string) *localSimulatedJob {
	if job, ok := b.jobs[jobName]; ok {
		return job
	}
	podIdx := len(b.jobs) % len(simulatedWorkerPodIPs)
	podIP := simulatedWorkerPodIPs[podIdx]
	actor := NewBuildbox(jobName, b.mirrorURL, b.registryURL)
	actor.currentPodIP = podIP
	actor.podIPHistory = []string{podIP}
	sim := &localSimulatedJob{
		actor:   actor,
		status:  "RUNNING",
		podIdx:  podIdx,
		history: []string{podIP},
	}
	b.jobs[jobName] = sim
	b.order = append(b.order, jobName)
	return sim
}

func (b *LocalSimulatorBackend) ListJobs(_ context.Context) ([]BuildJobTracker, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	out := make([]BuildJobTracker, 0, len(b.order))
	for _, name := range b.order {
		if sim, ok := b.jobs[name]; ok {
			state := sim.actor.State(sim.actor.currentPodIP)
			state.Status = sim.status
			state.EgressPolicyYAML = generateEgressPolicyYAML(b.atespace, name, state.Phase)
			out = append(out, state)
		}
	}
	return out, nil
}

func (b *LocalSimulatorBackend) GetJobState(_ context.Context, jobName string, wake bool) (*BuildJobTracker, error) {
	if !isValidJobName(jobName) {
		return nil, fmt.Errorf("invalid job name %q", jobName)
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	sim := b.getOrInitJobLocked(jobName)
	if wake && sim.status == "SUSPENDED" {
		sim.status = "RUNNING"
		sim.podIdx = (sim.podIdx + 1) % len(simulatedWorkerPodIPs)
		newIP := simulatedWorkerPodIPs[sim.podIdx]
		sim.actor.currentPodIP = newIP
		sim.history = append(sim.history, newIP)
		sim.actor.podIPHistory = slices.Clone(sim.history)
	}
	state := sim.actor.State(sim.actor.currentPodIP)
	state.Status = sim.status
	if sim.status == "SUSPENDED" {
		state.CurrentPodIP = ""
	}
	state.EgressPolicyYAML = generateEgressPolicyYAML(b.atespace, jobName, state.Phase)
	return &state, nil
}

func (b *LocalSimulatorBackend) FetchDeps(ctx context.Context, jobName string) (*BuildJobTracker, error) {
	if !isValidJobName(jobName) {
		return nil, fmt.Errorf("invalid job name %q", jobName)
	}
	b.mu.Lock()
	sim := b.getOrInitJobLocked(jobName)
	sim.status = "RUNNING"
	mirrorURL := b.mirrorURL
	b.mu.Unlock()

	if err := sim.actor.FetchDependencies(ctx, mirrorURL); err != nil {
		// Fallback to simulated cache in self-contained test/local mode
		sim.actor.PopulateSimulatedCache()
	}

	state := sim.actor.State(sim.actor.currentPodIP)
	state.Status = sim.status
	if sim.status == "SUSPENDED" {
		state.CurrentPodIP = ""
	}
	state.EgressPolicyYAML = generateEgressPolicyYAML(b.atespace, jobName, state.Phase)
	return &state, nil
}

func (b *LocalSimulatorBackend) CompileAndTest(ctx context.Context, jobName string) (*BuildJobTracker, error) {
	if !isValidJobName(jobName) {
		return nil, fmt.Errorf("invalid job name %q", jobName)
	}
	b.mu.Lock()
	sim := b.getOrInitJobLocked(jobName)
	sim.status = "RUNNING"
	b.mu.Unlock()

	if err := sim.actor.CompileAndTest(ctx); err != nil {
		state := sim.actor.State(sim.actor.currentPodIP)
		state.Status = sim.status
		state.EgressPolicyYAML = generateEgressPolicyYAML(b.atespace, jobName, state.Phase)
		return &state, err
	}

	state := sim.actor.State(sim.actor.currentPodIP)
	state.Status = sim.status
	if sim.status == "SUSPENDED" {
		state.CurrentPodIP = ""
	}
	state.EgressPolicyYAML = generateEgressPolicyYAML(b.atespace, jobName, state.Phase)
	return &state, nil
}

func (b *LocalSimulatorBackend) TestRogueEgress(ctx context.Context, jobName string) (*BuildJobTracker, error) {
	if !isValidJobName(jobName) {
		return nil, fmt.Errorf("invalid job name %q", jobName)
	}
	b.mu.Lock()
	sim := b.getOrInitJobLocked(jobName)
	sim.status = "RUNNING"
	b.mu.Unlock()

	_, _ = sim.actor.TestRogueEgress(ctx)
	state := sim.actor.State(sim.actor.currentPodIP)
	state.Status = sim.status
	if sim.status == "SUSPENDED" {
		state.CurrentPodIP = ""
	}
	state.EgressPolicyYAML = generateEgressPolicyYAML(b.atespace, jobName, state.Phase)
	return &state, nil
}

func (b *LocalSimulatorBackend) PublishArtifact(ctx context.Context, jobName string) (*BuildJobTracker, error) {
	if !isValidJobName(jobName) {
		return nil, fmt.Errorf("invalid job name %q", jobName)
	}
	b.mu.Lock()
	sim := b.getOrInitJobLocked(jobName)
	sim.status = "RUNNING"
	regURL := b.registryURL
	b.mu.Unlock()

	if _, err := sim.actor.PublishArtifact(ctx, regURL); err != nil {
		// Fallback to simulated release in self-contained test/local mode
		sim.actor.mu.Lock()
		if sim.actor.artifact != nil {
			sim.actor.artifact.ReleaseURI = fmt.Sprintf("us-docker.pkg.dev/internal-repo/releases/%s@%s", jobName, sim.actor.artifact.BinaryDigest)
			sim.actor.phase = PhaseCompleted
			sim.actor.recordAuditLocked(PhaseArtifactPublish, fmt.Sprintf("%s/api/registry/releases", regURL), "POST", "HTTP", "ALLOWED", http.StatusCreated, 25, "", "Published signed binary & SBOM to registry: "+sim.actor.artifact.ReleaseURI)
		}
		sim.actor.mu.Unlock()
	}

	state := sim.actor.State(sim.actor.currentPodIP)
	state.Status = sim.status
	if sim.status == "SUSPENDED" {
		state.CurrentPodIP = ""
	}
	state.EgressPolicyYAML = generateEgressPolicyYAML(b.atespace, jobName, state.Phase)
	return &state, nil
}

func (b *LocalSimulatorBackend) AutoRunPipeline(ctx context.Context, jobName string) (*BuildJobTracker, error) {
	if _, err := b.FetchDeps(ctx, jobName); err != nil {
		return nil, err
	}
	if _, err := b.CompileAndTest(ctx, jobName); err != nil {
		return nil, err
	}
	if _, err := b.TestRogueEgress(ctx, jobName); err != nil {
		return nil, err
	}
	if _, err := b.PublishArtifact(ctx, jobName); err != nil {
		return nil, err
	}
	_ = b.SuspendActor(ctx, jobName)
	return b.GetJobState(ctx, jobName, false)
}

func (b *LocalSimulatorBackend) SuspendActor(_ context.Context, jobName string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if sim, ok := b.jobs[jobName]; ok {
		sim.status = "SUSPENDED"
		return nil
	}
	return fmt.Errorf("job %s not found", jobName)
}

func (b *LocalSimulatorBackend) ResetJob(_ context.Context, jobName string) (*BuildJobTracker, error) {
	if !isValidJobName(jobName) {
		return nil, fmt.Errorf("invalid job name %q", jobName)
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	sim := b.getOrInitJobLocked(jobName)
	sim.actor.Reset(true)
	sim.status = "RUNNING"
	state := sim.actor.State(sim.actor.currentPodIP)
	state.Status = sim.status
	if sim.status == "SUSPENDED" {
		state.CurrentPodIP = ""
	}
	state.EgressPolicyYAML = generateEgressPolicyYAML(b.atespace, jobName, state.Phase)
	return &state, nil
}

func (b *LocalSimulatorBackend) ResetStage(_ context.Context) (*BuildJobTracker, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.jobs = make(map[string]*localSimulatedJob)
	b.order = nil
	_ = b.ensureDefaultJobs()

	first := defaultBuildJobs[0]
	sim := b.jobs[first]
	state := sim.actor.State(sim.actor.currentPodIP)
	state.Status = sim.status
	state.EgressPolicyYAML = generateEgressPolicyYAML(b.atespace, first, state.Phase)
	return &state, nil
}

func (b *LocalSimulatorBackend) GetEgressPolicyYAML(_ context.Context, jobName string) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	sim := b.getOrInitJobLocked(jobName)
	return generateEgressPolicyYAML(b.atespace, jobName, sim.actor.phase), nil
}

// --- ClusterAteBackend (--mode=gateway against real Agent Substrate cluster) ---

type ClusterAteBackend struct {
	mu              sync.Mutex
	atespace        string
	template        string
	routerURL       string
	ateEndpoint     string
	kubectlAteBin   string
	httpClient      *http.Client
	currentPhases   map[string]string
	createdActors   map[string]bool
	cachedTrackers  map[string]*BuildJobTracker
	suspendedActors map[string]bool
}

func NewClusterAteBackend(atespace, template, routerURL, ateEndpoint, kubectlAteBin string) *ClusterAteBackend {
	if kubectlAteBin == "" {
		kubectlAteBin = "kubectl-ate"
	}
	if routerURL == "" {
		routerURL = "https://atenet-router.ate-system.svc:443"
	}
	b := &ClusterAteBackend{
		atespace:        atespace,
		template:        template,
		routerURL:       strings.TrimRight(routerURL, "/"),
		ateEndpoint:     ateEndpoint,
		kubectlAteBin:   kubectlAteBin,
		currentPhases:   make(map[string]string),
		createdActors:   make(map[string]bool),
		cachedTrackers:  make(map[string]*BuildJobTracker),
		suspendedActors: make(map[string]bool),
		httpClient:      createClusterHTTPClient(),
	}
	// Initially mark secondary baseline actors as suspended
	b.suspendedActors[defaultBuildJobs[1]] = true
	b.suspendedActors[defaultBuildJobs[2]] = true
	return b
}

func createClusterHTTPClient() *http.Client {
	tlsConfig := &tls.Config{
		MinVersion: tls.VersionTLS13,
		GetClientCertificate: func(_ *tls.CertificateRequestInfo) (*tls.Certificate, error) {
			certPath := "/run/router-client/credential-bundle.pem"
			certData, err := os.ReadFile(certPath)
			if err != nil {
				return nil, err
			}
			cert, err := tls.X509KeyPair(certData, certData)
			if err != nil {
				return nil, err
			}
			return &cert, nil
		},
	}
	if tbData, err := os.ReadFile("/run/router-client/trust-bundle.pem"); err == nil && len(tbData) > 0 {
		pool := x509.NewCertPool()
		if pool.AppendCertsFromPEM(tbData) {
			tlsConfig.RootCAs = pool
		}
	} else {
		// In local dev port-forward, accept self-signed router certs if any
		tlsConfig.InsecureSkipVerify = true
	}

	return &http.Client{
		Timeout: 20 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: tlsConfig,
			DialContext: (&net.Dialer{
				Timeout: 5 * time.Second,
			}).DialContext,
			DisableKeepAlives: true,
		},
	}
}

func (b *ClusterAteBackend) runKubectlAte(ctx context.Context, args ...string) ([]byte, error) {
	cmdArgs := slices.Clone(args)
	if b.ateEndpoint != "" {
		cmdArgs = append(cmdArgs, "--endpoint", b.ateEndpoint)
	}
	cmd := exec.CommandContext(ctx, b.kubectlAteBin, cmdArgs...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		return nil, fmt.Errorf("kubectl-ate %s failed: %w (stderr: %s)", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

func (b *ClusterAteBackend) EnsureActor(ctx context.Context, actorName string) error {
	b.mu.Lock()
	alreadyCreated := b.createdActors[actorName]
	b.mu.Unlock()
	if alreadyCreated {
		return nil
	}

	_, err := b.runKubectlAte(ctx, "create", "actor", actorName, "-a", b.atespace, "--template", b.template)
	if err != nil && !strings.Contains(strings.ToLower(err.Error()), "already exists") {
		return err
	}
	_ = b.applyEgressPolicy(ctx, actorName, PhaseDependencyFetch)

	b.mu.Lock()
	b.createdActors[actorName] = true
	b.mu.Unlock()
	return nil
}

func (b *ClusterAteBackend) applyEgressPolicy(ctx context.Context, actorName, phase string) error {
	b.mu.Lock()
	b.currentPhases[actorName] = phase
	b.mu.Unlock()

	// In Phase 2 (Hermetic Airgap), we delete the egress policy so DNS returns NXDOMAIN and all TCP is refused!
	if phase == PhaseHermeticCompile {
		_, _ = b.runKubectlAte(ctx, "delete", "egress-policy", actorName, "-a", b.atespace)
		time.Sleep(450 * time.Millisecond)
		return nil
	}

	policyYAML := generateEgressPolicyYAML(b.atespace, actorName, phase)
	tmp, err := os.CreateTemp("", fmt.Sprintf("buildbox-egress-%s-*.yaml", actorName))
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(policyYAML); err != nil {
		_ = tmp.Close()
		return err
	}
	_ = tmp.Close()

	// Try create first; if already exists, delete and recreate
	_, err = b.runKubectlAte(ctx, "create", "egress-policy", actorName, "-a", b.atespace, "-f", tmp.Name())
	if err != nil {
		_, _ = b.runKubectlAte(ctx, "delete", "egress-policy", actorName, "-a", b.atespace)
		_, err = b.runKubectlAte(ctx, "create", "egress-policy", actorName, "-a", b.atespace, "-f", tmp.Name())
	}
	return err
}

func (b *ClusterAteBackend) callActor(ctx context.Context, actorName, method, subPath string, payload []byte) ([]byte, error) {
	if err := b.EnsureActor(ctx, actorName); err != nil {
		return nil, err
	}

	target := fmt.Sprintf("%s%s", b.routerURL, subPath)
	req, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(payload))
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
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return body, fmt.Errorf("actor returned HTTP %d: %s", resp.StatusCode, string(body))
	}
	return body, nil
}

func (b *ClusterAteBackend) inspectActor(ctx context.Context, actorName string) (status, workerIP, workerPod string) {
	b.mu.Lock()
	if b.suspendedActors[actorName] {
		b.mu.Unlock()
		return "SUSPENDED", "", ""
	}
	b.mu.Unlock()

	raw, err := b.runKubectlAte(ctx, "get", "actors", actorName, "-a", b.atespace, "-o", "json")
	if err != nil {
		b.mu.Lock()
		isSusp := b.suspendedActors[actorName]
		b.mu.Unlock()
		if isSusp {
			return "SUSPENDED", "", ""
		}
		return "RUNNING", "", ""
	}

	var doc struct {
		Status struct {
			State            string `json:"state"`
			WorkerAssignment struct {
				WorkerPodIps []string `json:"workerPodIps"`
				WorkerPod    string   `json:"workerPod"`
			} `json:"workerAssignment"`
		} `json:"status"`
	}
	if err := json.Unmarshal(raw, &doc); err == nil && doc.Status.State != "" {
		state := doc.Status.State
		if strings.HasPrefix(state, "ACTOR_STATE_") {
			state = strings.TrimPrefix(state, "ACTOR_STATE_")
		}
		if len(doc.Status.WorkerAssignment.WorkerPodIps) > 0 {
			workerIP = doc.Status.WorkerAssignment.WorkerPodIps[0]
		}
		workerPod = doc.Status.WorkerAssignment.WorkerPod
		return state, workerIP, workerPod
	}

	var docList struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Status struct {
				State            string `json:"state"`
				WorkerAssignment struct {
					WorkerPodIps []string `json:"workerPodIps"`
					WorkerPod    string   `json:"workerPod"`
				} `json:"workerAssignment"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(raw, &docList); err == nil && len(docList.Items) > 0 {
		for _, item := range docList.Items {
			if item.Metadata.Name == actorName {
				state := item.Status.State
				if strings.HasPrefix(state, "ACTOR_STATE_") {
					state = strings.TrimPrefix(state, "ACTOR_STATE_")
				}
				if len(item.Status.WorkerAssignment.WorkerPodIps) > 0 {
					workerIP = item.Status.WorkerAssignment.WorkerPodIps[0]
				}
				workerPod = item.Status.WorkerAssignment.WorkerPod
				return state, workerIP, workerPod
			}
		}
	}

	return "RUNNING", "", ""
}

func (b *ClusterAteBackend) ListJobs(ctx context.Context) ([]BuildJobTracker, error) {
	out := make([]BuildJobTracker, 0, len(defaultBuildJobs))
	for _, name := range defaultBuildJobs {
		st, err := b.GetJobState(ctx, name, false)
		if err == nil && st != nil {
			out = append(out, *st)
		} else {
			out = append(out, BuildJobTracker{
				JobName:          name,
				Label:            formatJobLabel(name),
				Phase:            PhaseIdle,
				Status:           "SUSPENDED",
				EgressPolicyYAML: generateEgressPolicyYAML(b.atespace, name, PhaseIdle),
			})
		}
	}
	return out, nil
}

func (b *ClusterAteBackend) GetJobState(ctx context.Context, jobName string, wake bool) (*BuildJobTracker, error) {
	if !isValidJobName(jobName) {
		return nil, fmt.Errorf("invalid job name %q", jobName)
	}

	b.mu.Lock()
	cached := b.cachedTrackers[jobName]
	explicitlySuspended := b.suspendedActors[jobName]
	phase := b.currentPhases[jobName]
	if phase == "" {
		phase = PhaseIdle
	}
	b.mu.Unlock()

	status, workerIP, workerPod := b.inspectActor(ctx, jobName)
	isSuspended := explicitlySuspended || strings.Contains(status, "SUSPEND")

	if isSuspended && !wake {
		// When the actor is sleeping in GCS, do NOT call callActor over atenet-router,
		// because any incoming HTTP request will immediately wake the actor!
		if cached != nil {
			cachedCopy := *cached
			cachedCopy.Status = "SUSPENDED"
			cachedCopy.CurrentPodIP = ""
			cachedCopy.WorkerPodName = ""
			cachedCopy.EgressPolicyYAML = generateEgressPolicyYAML(b.atespace, jobName, cachedCopy.Phase)
			return &cachedCopy, nil
		}
		return &BuildJobTracker{
			JobName:          jobName,
			Label:            formatJobLabel(jobName),
			Phase:            phase,
			Status:           "SUSPENDED",
			WorkerPodName:    workerPod,
			EgressPolicyYAML: generateEgressPolicyYAML(b.atespace, jobName, phase),
		}, nil
	}

	body, err := b.callActor(ctx, jobName, http.MethodGet, "/state", nil)
	if err != nil {
		if isSuspended && cached != nil {
			cachedCopy := *cached
			cachedCopy.Status = "SUSPENDED"
			return &cachedCopy, nil
		}
		return nil, err
	}
	var tracker BuildJobTracker
	if err := json.Unmarshal(body, &tracker); err != nil {
		return nil, err
	}
	tracker.Status = "RUNNING"
	if workerPod != "" {
		tracker.WorkerPodName = workerPod
	}
	if workerIP != "" && !slices.Contains(tracker.PodIPHistory, workerIP) {
		tracker.PodIPHistory = append(tracker.PodIPHistory, workerIP)
		tracker.CurrentPodIP = workerIP
	}
	tracker.EgressPolicyYAML = generateEgressPolicyYAML(b.atespace, jobName, tracker.Phase)

	b.mu.Lock()
	b.cachedTrackers[jobName] = &tracker
	b.suspendedActors[jobName] = false
	b.currentPhases[jobName] = tracker.Phase
	b.mu.Unlock()

	return &tracker, nil
}

func (b *ClusterAteBackend) FetchDeps(ctx context.Context, jobName string) (*BuildJobTracker, error) {
	if err := b.EnsureActor(ctx, jobName); err != nil {
		return nil, err
	}
	b.mu.Lock()
	b.suspendedActors[jobName] = false
	b.mu.Unlock()

	if err := b.applyEgressPolicy(ctx, jobName, PhaseDependencyFetch); err != nil {
		slog.Warn("Failed to apply Phase 1 EgressPolicy", slog.Any("err", err))
	}
	_, err := b.callActor(ctx, jobName, http.MethodPost, "/action/fetch-deps", []byte("{}"))
	if err != nil {
		return nil, err
	}
	return b.GetJobState(ctx, jobName, true)
}

func (b *ClusterAteBackend) CompileAndTest(ctx context.Context, jobName string) (*BuildJobTracker, error) {
	if err := b.EnsureActor(ctx, jobName); err != nil {
		return nil, err
	}
	b.mu.Lock()
	b.suspendedActors[jobName] = false
	b.mu.Unlock()

	if err := b.applyEgressPolicy(ctx, jobName, PhaseHermeticCompile); err != nil {
		slog.Warn("Failed to lock airgap in Phase 2", slog.Any("err", err))
	}
	_, err := b.callActor(ctx, jobName, http.MethodPost, "/action/compile-test", []byte("{}"))
	if err != nil {
		return nil, err
	}
	return b.GetJobState(ctx, jobName, true)
}

func (b *ClusterAteBackend) TestRogueEgress(ctx context.Context, jobName string) (*BuildJobTracker, error) {
	if err := b.EnsureActor(ctx, jobName); err != nil {
		return nil, err
	}
	b.mu.Lock()
	b.suspendedActors[jobName] = false
	b.mu.Unlock()

	// Ensure airgap is locked
	_ = b.applyEgressPolicy(ctx, jobName, PhaseHermeticCompile)
	_, err := b.callActor(ctx, jobName, http.MethodPost, "/action/test-rogue-egress", []byte("{}"))
	if err != nil {
		return nil, err
	}
	return b.GetJobState(ctx, jobName, true)
}

func (b *ClusterAteBackend) PublishArtifact(ctx context.Context, jobName string) (*BuildJobTracker, error) {
	if err := b.EnsureActor(ctx, jobName); err != nil {
		return nil, err
	}
	b.mu.Lock()
	b.suspendedActors[jobName] = false
	b.mu.Unlock()

	if err := b.applyEgressPolicy(ctx, jobName, PhaseArtifactPublish); err != nil {
		slog.Warn("Failed to apply Phase 3 EgressPolicy", slog.Any("err", err))
	}
	_, err := b.callActor(ctx, jobName, http.MethodPost, "/action/publish-artifact", []byte("{}"))
	if err != nil {
		return nil, err
	}
	return b.GetJobState(ctx, jobName, true)
}

func (b *ClusterAteBackend) AutoRunPipeline(ctx context.Context, jobName string) (*BuildJobTracker, error) {
	if _, err := b.FetchDeps(ctx, jobName); err != nil {
		return nil, err
	}
	if _, err := b.CompileAndTest(ctx, jobName); err != nil {
		return nil, err
	}
	if _, err := b.TestRogueEgress(ctx, jobName); err != nil {
		return nil, err
	}
	if _, err := b.PublishArtifact(ctx, jobName); err != nil {
		return nil, err
	}
	_ = b.SuspendActor(ctx, jobName)
	return b.GetJobState(ctx, jobName, false)
}

func (b *ClusterAteBackend) SuspendActor(ctx context.Context, jobName string) error {
	b.mu.Lock()
	b.suspendedActors[jobName] = true
	if tr, ok := b.cachedTrackers[jobName]; ok && tr != nil {
		tr.Status = "SUSPENDED"
		tr.CurrentPodIP = ""
		tr.WorkerPodName = ""
	}
	b.mu.Unlock()

	_, err := b.runKubectlAte(ctx, "suspend", "actor", jobName, "-a", b.atespace, "--mode=sync")
	return err
}

func (b *ClusterAteBackend) ResetJob(ctx context.Context, jobName string) (*BuildJobTracker, error) {
	_, _ = b.runKubectlAte(ctx, "delete", "actor", jobName, "-a", b.atespace)
	_, _ = b.runKubectlAte(ctx, "delete", "egress-policy", jobName, "-a", b.atespace)
	b.mu.Lock()
	delete(b.createdActors, jobName)
	delete(b.currentPhases, jobName)
	delete(b.cachedTrackers, jobName)
	b.suspendedActors[jobName] = false
	b.mu.Unlock()

	if err := b.EnsureActor(ctx, jobName); err != nil {
		return nil, err
	}
	return b.GetJobState(ctx, jobName, true)
}

func (b *ClusterAteBackend) ResetStage(ctx context.Context) (*BuildJobTracker, error) {
	for _, name := range defaultBuildJobs {
		_, _ = b.ResetJob(ctx, name)
	}
	// Keep 2nd and 3rd suspended initially
	_ = b.SuspendActor(ctx, defaultBuildJobs[1])
	_ = b.SuspendActor(ctx, defaultBuildJobs[2])
	return b.GetJobState(ctx, defaultBuildJobs[0], true)
}

func (b *ClusterAteBackend) GetEgressPolicyYAML(ctx context.Context, jobName string) (string, error) {
	b.mu.Lock()
	phase := b.currentPhases[jobName]
	b.mu.Unlock()
	if phase == "" {
		phase = PhaseIdle
	}
	// Try fetching from kubectl-ate if available
	out, err := b.runKubectlAte(ctx, "get", "egress-policy", jobName, "-a", b.atespace, "-o", "yaml")
	if err == nil && len(out) > 0 {
		return string(out), nil
	}
	return generateEgressPolicyYAML(b.atespace, jobName, phase), nil
}
