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
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

type SnapshotRecord struct {
	Version   int       `json:"version"`
	Reason    string    `json:"reason"`
	CreatedAt time.Time `json:"created_at"`
}

type ActorTrackerState struct {
	ActorName       string                 `json:"actor_name"`
	AttackerIP      string                 `json:"attacker_ip"`
	Label           string                 `json:"label"`
	Status          string                 `json:"status"` // RUNNING | SUSPENDED
	SnapshotVersion int                    `json:"snapshot_version"`
	Snapshots       []SnapshotRecord       `json:"snapshots"`
	IsClone         bool                   `json:"is_clone"`
	ClonedFrom      string                 `json:"cloned_from,omitempty"`
	LastShell       *AttackerShellResponse `json:"last_shell,omitempty"`
	LastForensics   *ForensicsResponse     `json:"last_forensics,omitempty"`
}

type HoneypotBackend interface {
	EnsureActor(ctx context.Context, actorName string) error
	ExecShell(ctx context.Context, actorName, cmd, dropFile, content string) (*AttackerShellResponse, *ForensicsResponse, error)
	GetForensics(ctx context.Context, actorName string) (*ForensicsResponse, error)
	SaveActor(ctx context.Context, actorName, reason string) (int, error)
	SuspendActor(ctx context.Context, actorName string) error
	RevertActor(ctx context.Context, actorName string, targetVersion int) (*ForensicsResponse, error)
	CloneActor(ctx context.Context, sourceActor, newActor, tagName string) (*ForensicsResponse, error)
	ListActors(ctx context.Context) ([]ActorTrackerState, error)
}

func defaultActorMetadata(actorName string) (ip, label string, isClone bool) {
	switch actorName {
	case "honeypot-203-0-113-5":
		return "203.0.113.5", "203.0.113.5 (APT-29 Intruder)", false
	case "honeypot-198-51-100-9":
		return "198.51.100.9", "198.51.100.9 (Recon Scanner)", false
	default:
		if strings.Contains(actorName, "forensic") || strings.Contains(actorName, "clone") {
			return "SOC-LAB", actorName + " [FORENSIC CLONE]", true
		}
		trimmed := strings.TrimPrefix(actorName, "honeypot-")
		ipGuess := strings.ReplaceAll(trimmed, "-", ".")
		return ipGuess, ipGuess + " (" + actorName + ")", false
	}
}

// --- LocalSimulatorBackend (--mode=local & unit tests) ---

type localDiskAndRAMSnapshot struct {
	version      int
	reason       string
	createdAt    time.Time
	filesOnDisk  map[string][]byte
	modesOnDisk  map[string]os.FileMode
	knownFiles   map[string]FileDeltaEntry
	forensicLog  []ForensicEvent
	canaryAlerts []CanaryAlert
	podIPHistory []string
	currentPodIP string
}

type localSimulatedActor struct {
	hp          *Honeypot
	tracker     ActorTrackerState
	checkpoints map[int]localDiskAndRAMSnapshot
	podIdx      int
}

type LocalSimulatorBackend struct {
	mu       sync.Mutex
	atespace string
	template string
	baseDir  string
	actors   map[string]*localSimulatedActor
	order    []string
	tags     map[string]localDiskAndRAMSnapshot
}

var simulatedWorkerPodIPs = []string{"10.44.0.9", "10.44.1.14", "10.44.2.21"}

func NewLocalSimulatorBackend(atespace, template string) *LocalSimulatorBackend {
	baseDir, _ := os.MkdirTemp("", "honeypot-local-sim-*")
	b := &LocalSimulatorBackend{
		atespace: atespace,
		template: template,
		baseDir:  baseDir,
		actors:   make(map[string]*localSimulatedActor),
		tags:     make(map[string]localDiskAndRAMSnapshot),
	}
	_ = b.EnsureActor(context.Background(), "honeypot-203-0-113-5")
	_ = b.EnsureActor(context.Background(), "honeypot-198-51-100-9")
	return b
}

func (b *LocalSimulatorBackend) captureSnapshotLocked(sim *localSimulatedActor, version int, reason string) localDiskAndRAMSnapshot {
	sim.hp.mu.Lock()
	defer sim.hp.mu.Unlock()

	filesOnDisk := make(map[string][]byte)
	modesOnDisk := make(map[string]os.FileMode)
	entries, _ := os.ReadDir(sim.hp.ScratchDir)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		full := filepath.Join(sim.hp.ScratchDir, entry.Name())
		if data, err := os.ReadFile(full); err == nil {
			filesOnDisk[entry.Name()] = data
		}
		if info, err := entry.Info(); err == nil {
			modesOnDisk[entry.Name()] = info.Mode().Perm()
		}
	}
	knownCopy := make(map[string]FileDeltaEntry, len(sim.hp.KnownFiles))
	for k, v := range sim.hp.KnownFiles {
		knownCopy[k] = v
	}
	return localDiskAndRAMSnapshot{
		version:      version,
		reason:       reason,
		createdAt:    time.Now().UTC(),
		filesOnDisk:  filesOnDisk,
		modesOnDisk:  modesOnDisk,
		knownFiles:   knownCopy,
		forensicLog:  slices.Clone(sim.hp.ForensicLog),
		canaryAlerts: slices.Clone(sim.hp.CanaryAlerts),
		podIPHistory: slices.Clone(sim.hp.PodIPHistory),
		currentPodIP: sim.hp.CurrentPodIP,
	}
}

func (b *LocalSimulatorBackend) restoreSnapshotLocked(sim *localSimulatedActor, snap localDiskAndRAMSnapshot) {
	sim.hp.mu.Lock()
	defer sim.hp.mu.Unlock()

	entries, _ := os.ReadDir(sim.hp.ScratchDir)
	for _, entry := range entries {
		_ = os.RemoveAll(filepath.Join(sim.hp.ScratchDir, entry.Name()))
	}
	for name, data := range snap.filesOnDisk {
		mode := snap.modesOnDisk[name]
		if mode == 0 {
			mode = 0o644
		}
		_ = os.WriteFile(filepath.Join(sim.hp.ScratchDir, name), data, mode)
	}
	sim.hp.KnownFiles = make(map[string]FileDeltaEntry, len(snap.knownFiles))
	for k, v := range snap.knownFiles {
		sim.hp.KnownFiles[k] = v
	}
	sim.hp.ForensicLog = slices.Clone(snap.forensicLog)
	sim.hp.CanaryAlerts = slices.Clone(snap.canaryAlerts)
	sim.hp.PodIPHistory = slices.Clone(snap.podIPHistory)
	sim.hp.CurrentPodIP = snap.currentPodIP
}

func (b *LocalSimulatorBackend) EnsureActor(_ context.Context, actorName string) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if _, exists := b.actors[actorName]; exists {
		return nil
	}
	scratch := filepath.Join(b.baseDir, actorName)
	hp, err := newHoneypot(scratch)
	if err != nil {
		return err
	}
	podIdx := len(b.order) % len(simulatedWorkerPodIPs)
	initialPodIP := simulatedWorkerPodIPs[podIdx]
	forensics := hp.SnapshotForensics(initialPodIP)
	ip, label, isClone := defaultActorMetadata(actorName)

	sim := &localSimulatedActor{
		hp:          hp,
		checkpoints: make(map[int]localDiskAndRAMSnapshot),
		podIdx:      podIdx,
		tracker: ActorTrackerState{
			ActorName:       actorName,
			AttackerIP:      ip,
			Label:           label,
			Status:          "RUNNING",
			SnapshotVersion: 1,
			Snapshots: []SnapshotRecord{
				{Version: 1, Reason: "Golden Snapshot Baseline", CreatedAt: time.Now().UTC()},
			},
			IsClone:       isClone,
			LastForensics: &forensics,
		},
	}
	sim.checkpoints[1] = b.captureSnapshotLocked(sim, 1, "Golden Snapshot Baseline")
	b.actors[actorName] = sim
	b.order = append(b.order, actorName)
	return nil
}

func (b *LocalSimulatorBackend) ExecShell(ctx context.Context, actorName, cmd, dropFile, content string) (*AttackerShellResponse, *ForensicsResponse, error) {
	if err := b.EnsureActor(ctx, actorName); err != nil {
		return nil, nil, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	sim := b.actors[actorName]
	if sim.tracker.Status == "SUSPENDED" {
		// Migrate to next worker pod IP on wakeup!
		sim.podIdx = (sim.podIdx + 1) % len(simulatedWorkerPodIPs)
		sim.tracker.Status = "RUNNING"
	}
	podIP := simulatedWorkerPodIPs[sim.podIdx]

	shellResp, err := sim.hp.InteractShell(dropFile, content, cmd, podIP)
	if err != nil {
		return nil, nil, err
	}
	forensicsResp := sim.hp.SnapshotForensics(podIP)
	sim.tracker.LastShell = &shellResp
	sim.tracker.LastForensics = &forensicsResp
	return &shellResp, &forensicsResp, nil
}

func (b *LocalSimulatorBackend) GetForensics(ctx context.Context, actorName string) (*ForensicsResponse, error) {
	if err := b.EnsureActor(ctx, actorName); err != nil {
		return nil, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	sim := b.actors[actorName]
	if sim.tracker.Status == "SUSPENDED" && sim.tracker.LastForensics != nil {
		return sim.tracker.LastForensics, nil
	}
	podIP := simulatedWorkerPodIPs[sim.podIdx]
	forensicsResp := sim.hp.SnapshotForensics(podIP)
	sim.tracker.LastForensics = &forensicsResp
	return &forensicsResp, nil
}

func (b *LocalSimulatorBackend) SaveActor(ctx context.Context, actorName, reason string) (int, error) {
	if err := b.EnsureActor(ctx, actorName); err != nil {
		return 0, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	sim := b.actors[actorName]
	nextVer := sim.tracker.SnapshotVersion + 1
	snap := b.captureSnapshotLocked(sim, nextVer, reason)
	sim.checkpoints[nextVer] = snap
	sim.tracker.SnapshotVersion = nextVer
	sim.tracker.Snapshots = append(sim.tracker.Snapshots, SnapshotRecord{
		Version:   nextVer,
		Reason:    reason,
		CreatedAt: snap.createdAt,
	})
	return nextVer, nil
}

func (b *LocalSimulatorBackend) SuspendActor(ctx context.Context, actorName string) error {
	if err := b.EnsureActor(ctx, actorName); err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	sim := b.actors[actorName]
	snap := b.captureSnapshotLocked(sim, sim.tracker.SnapshotVersion, "Suspended to GCS")
	sim.checkpoints[sim.tracker.SnapshotVersion] = snap
	sim.tracker.Status = "SUSPENDED"
	return nil
}

func (b *LocalSimulatorBackend) RevertActor(ctx context.Context, actorName string, targetVersion int) (*ForensicsResponse, error) {
	if err := b.EnsureActor(ctx, actorName); err != nil {
		return nil, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	sim := b.actors[actorName]
	if targetVersion <= 0 {
		targetVersion = sim.tracker.SnapshotVersion
	}
	snap, ok := sim.checkpoints[targetVersion]
	if !ok {
		return nil, fmt.Errorf("snapshot version %d not found for %s", targetVersion, actorName)
	}
	b.restoreSnapshotLocked(sim, snap)
	sim.tracker.SnapshotVersion = targetVersion
	sim.tracker.Status = "RUNNING"
	podIP := simulatedWorkerPodIPs[sim.podIdx]
	forensics := sim.hp.SnapshotForensics(podIP)
	sim.tracker.LastForensics = &forensics
	return &forensics, nil
}

func (b *LocalSimulatorBackend) CloneActor(ctx context.Context, sourceActor, newActor, tagName string) (*ForensicsResponse, error) {
	if err := b.EnsureActor(ctx, sourceActor); err != nil {
		return nil, err
	}
	b.mu.Lock()
	srcSim := b.actors[sourceActor]
	snap := b.captureSnapshotLocked(srcSim, srcSim.tracker.SnapshotVersion, "Forensic Tag "+tagName)
	b.tags[tagName] = snap
	b.mu.Unlock()

	if err := b.EnsureActor(ctx, newActor); err != nil {
		return nil, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	dstSim := b.actors[newActor]
	b.restoreSnapshotLocked(dstSim, snap)
	dstSim.tracker.IsClone = true
	dstSim.tracker.ClonedFrom = sourceActor
	dstSim.tracker.Label = fmt.Sprintf("%s [CLONE of %s]", newActor, sourceActor)
	dstSim.tracker.SnapshotVersion = 1
	dstSim.checkpoints[1] = snap
	podIP := simulatedWorkerPodIPs[dstSim.podIdx]
	forensics := dstSim.hp.SnapshotForensics(podIP)
	dstSim.tracker.LastForensics = &forensics
	return &forensics, nil
}

func (b *LocalSimulatorBackend) ListActors(_ context.Context) ([]ActorTrackerState, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	out := make([]ActorTrackerState, 0, len(b.order))
	for _, name := range b.order {
		if sim, ok := b.actors[name]; ok {
			trCopy := sim.tracker
			trCopy.Snapshots = slices.Clone(trCopy.Snapshots)
			out = append(out, trCopy)
		}
	}
	return out, nil
}

// --- ClusterAteBackend (--mode=gateway against real Agent Substrate cluster) ---

type ClusterAteBackend struct {
	mu            sync.Mutex
	atespace      string
	template      string
	routerURL     string
	ateEndpoint   string
	kubectlAteBin string
	httpClient    *http.Client
	trackers      map[string]*ActorTrackerState
	created       map[string]bool
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
	b := &ClusterAteBackend{
		atespace:      atespace,
		template:      template,
		routerURL:     normalizeRouterURL(routerURL),
		ateEndpoint:   ateEndpoint,
		kubectlAteBin: kubectlAteBin,
		httpClient:    newRouterHTTPClient(15 * time.Second),
		trackers:      make(map[string]*ActorTrackerState),
		created:       make(map[string]bool),
	}
	for _, name := range []string{"honeypot-203-0-113-5", "honeypot-198-51-100-9"} {
		ip, label, isClone := defaultActorMetadata(name)
		b.trackers[name] = &ActorTrackerState{
			ActorName:       name,
			AttackerIP:      ip,
			Label:           label,
			Status:          "SUSPENDED",
			SnapshotVersion: 1,
			Snapshots: []SnapshotRecord{
				{Version: 1, Reason: "Golden Snapshot Baseline", CreatedAt: time.Now().UTC()},
			},
			IsClone: isClone,
		}
		b.order = append(b.order, name)
	}
	return b
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

func (b *ClusterAteBackend) ensureTrackerLocked(actorName string) *ActorTrackerState {
	if tr, ok := b.trackers[actorName]; ok {
		return tr
	}
	ip, label, isClone := defaultActorMetadata(actorName)
	tr := &ActorTrackerState{
		ActorName:       actorName,
		AttackerIP:      ip,
		Label:           label,
		Status:          "RUNNING",
		SnapshotVersion: 1,
		Snapshots: []SnapshotRecord{
			{Version: 1, Reason: "Golden Snapshot Baseline", CreatedAt: time.Now().UTC()},
		},
		IsClone: isClone,
	}
	b.trackers[actorName] = tr
	b.order = append(b.order, actorName)
	return tr
}

func (b *ClusterAteBackend) EnsureActor(ctx context.Context, actorName string) error {
	b.mu.Lock()
	b.ensureTrackerLocked(actorName)
	alreadyCreated := b.created[actorName]
	b.mu.Unlock()
	if alreadyCreated {
		return nil
	}

	_, err := b.runKubectlAte(ctx, "create", "actor", actorName, "-a", b.atespace, "--template", b.template)
	if err != nil && !strings.Contains(strings.ToLower(err.Error()), "already exists") {
		return err
	}

	b.mu.Lock()
	b.created[actorName] = true
	b.mu.Unlock()
	return nil
}

func (b *ClusterAteBackend) inspectRunningActor(ctx context.Context, actorName string) (workerIP, workerPod, snapshotURI string) {
	raw, err := b.runKubectlAte(ctx, "get", "actors", actorName, "-a", b.atespace, "-o", "json")
	if err != nil {
		return "", "", ""
	}
	var parsed struct {
		Status struct {
			ExternalSnapshot struct {
				SnapshotURI string `json:"snapshotUri"`
			} `json:"externalSnapshot"`
			Snapshots []struct {
				DurableSnapshot struct {
					State  string `json:"state"`
					Object struct {
						SnapshotURI string `json:"snapshotUri"`
					} `json:"object"`
				} `json:"durableSnapshot"`
			} `json:"snapshots"`
			WorkerAssignment struct {
				WorkerPod    string   `json:"workerPod"`
				WorkerPodIP  string   `json:"workerPodIp"`
				WorkerPodIPs []string `json:"workerPodIps"`
			} `json:"workerAssignment"`
		} `json:"status"`
	}
	if json.Unmarshal([]byte(raw), &parsed) != nil {
		return "", "", ""
	}
	ip := parsed.Status.WorkerAssignment.WorkerPodIP
	if ip == "" && len(parsed.Status.WorkerAssignment.WorkerPodIPs) > 0 {
		ip = parsed.Status.WorkerAssignment.WorkerPodIPs[0]
	}
	uri := parsed.Status.ExternalSnapshot.SnapshotURI
	for _, s := range parsed.Status.Snapshots {
		if (s.DurableSnapshot.State == "" || s.DurableSnapshot.State == "SNAPSHOT_STORAGE_STATE_COMPLETED") && s.DurableSnapshot.Object.SnapshotURI != "" {
			uri = s.DurableSnapshot.Object.SnapshotURI
		}
	}
	return ip, parsed.Status.WorkerAssignment.WorkerPod, uri
}

func (b *ClusterAteBackend) reclaimWorkerSlots(ctx context.Context, keepActor string) {
	out, err := b.runKubectlAte(ctx, "get", "actors", "-a", b.atespace)
	if err != nil {
		return
	}
	var foreignActors []string
	var honeypotActors []string
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 || fields[0] == "ATESPACE" || fields[0] == "NAMESPACE" || strings.HasPrefix(fields[0], "Warning:") {
			continue
		}
		name := fields[1]
		state := fields[3]
		if name == keepActor {
			continue
		}
		if state != "ACTOR_STATE_RUNNING" && state != "ACTOR_STATE_PAUSED" {
			continue
		}
		if strings.HasPrefix(name, "honeypot-") {
			honeypotActors = append(honeypotActors, name)
		} else {
			foreignActors = append(foreignActors, name)
		}
	}
	for _, name := range foreignActors {
		_, _ = b.runKubectlAte(ctx, "suspend", "actor", name, "-a", b.atespace, "--mode=async")
	}
	if len(foreignActors) == 0 && len(honeypotActors) > 0 {
		victim := honeypotActors[0]
		_, _ = b.runKubectlAte(ctx, "suspend", "actor", victim, "-a", b.atespace, "--mode=async")
	}
}

func (b *ClusterAteBackend) callActorJSON(ctx context.Context, actorName, method, path, workerPodIP string, body any, out any) error {
	var rawBody []byte
	if body != nil {
		var err error
		rawBody, err = json.Marshal(body)
		if err != nil {
			return err
		}
	}
	for attempt := 0; attempt < 2; attempt++ {
		var bodyReader io.Reader
		if rawBody != nil {
			bodyReader = bytes.NewReader(rawBody)
		}
		req, err := http.NewRequestWithContext(ctx, method, b.routerURL+path, bodyReader)
		if err != nil {
			return err
		}
		req.Header.Set("ate-target-actor", b.atespace+"/"+actorName)
		if workerPodIP != "" {
			req.Header.Set("X-Worker-Pod-IP", workerPodIP)
		}
		if rawBody != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := b.httpClient.Do(req)
		if err != nil {
			if attempt == 0 {
				b.httpClient.CloseIdleConnections()
				continue
			}
			return err
		}
		rawResp, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusServiceUnavailable && attempt == 0 {
			b.reclaimWorkerSlots(ctx, actorName)
			time.Sleep(250 * time.Millisecond)
			continue
		}
		if resp.StatusCode >= 300 {
			return fmt.Errorf("actor %s %s returned %d: %s", actorName, path, resp.StatusCode, string(rawResp))
		}
		if out == nil || len(rawResp) == 0 {
			return nil
		}
		return json.Unmarshal(rawResp, out)
	}
	return fmt.Errorf("actor %s %s failed after retries", actorName, path)
}

func (b *ClusterAteBackend) ExecShell(ctx context.Context, actorName, cmd, dropFile, content string) (*AttackerShellResponse, *ForensicsResponse, error) {
	if err := b.EnsureActor(ctx, actorName); err != nil {
		return nil, nil, err
	}
	payload := map[string]string{
		"cmd":       cmd,
		"drop_file": dropFile,
		"content":   content,
	}
	var shellResp AttackerShellResponse
	if err := b.callActorJSON(ctx, actorName, http.MethodPost, "/shell", "", payload, &shellResp); err != nil {
		return nil, nil, err
	}
	workerIP, _, _ := b.inspectRunningActor(ctx, actorName)
	if workerIP != "" {
		shellResp.CurrentPodIP = workerIP
	}
	var forensicsResp ForensicsResponse
	if err := b.callActorJSON(ctx, actorName, http.MethodGet, "/forensics", workerIP, nil, &forensicsResp); err != nil {
		return nil, nil, err
	}
	b.mu.Lock()
	tr := b.ensureTrackerLocked(actorName)
	tr.Status = "RUNNING"
	tr.LastShell = &shellResp
	tr.LastForensics = &forensicsResp
	b.mu.Unlock()
	return &shellResp, &forensicsResp, nil
}

func (b *ClusterAteBackend) GetForensics(ctx context.Context, actorName string) (*ForensicsResponse, error) {
	if err := b.EnsureActor(ctx, actorName); err != nil {
		return nil, err
	}
	workerIP, _, _ := b.inspectRunningActor(ctx, actorName)
	var forensicsResp ForensicsResponse
	if err := b.callActorJSON(ctx, actorName, http.MethodGet, "/forensics", workerIP, nil, &forensicsResp); err != nil {
		return nil, err
	}
	if workerIP == "" {
		workerIP, _, _ = b.inspectRunningActor(ctx, actorName)
		if workerIP != "" && strings.HasPrefix(forensicsResp.CurrentWorkerPodIP, "169.254.") {
			_ = b.callActorJSON(ctx, actorName, http.MethodGet, "/forensics", workerIP, nil, &forensicsResp)
		}
	}
	b.mu.Lock()
	tr := b.ensureTrackerLocked(actorName)
	tr.Status = "RUNNING"
	tr.LastForensics = &forensicsResp
	b.mu.Unlock()
	return &forensicsResp, nil
}

func (b *ClusterAteBackend) SaveActor(ctx context.Context, actorName, reason string) (int, error) {
	b.mu.Lock()
	tr := b.ensureTrackerLocked(actorName)
	nextVer := tr.SnapshotVersion + 1
	b.mu.Unlock()

	// Save numbered checkpoint inside the actor's Go heap before suspending to GCS
	_ = b.callActorJSON(ctx, actorName, http.MethodPost, "/checkpoint/save", "", map[string]any{
		"version": nextVer,
		"reason":  reason,
	}, nil)

	// Commit SNAPSHOT_CONTENT_SCOPE_FULL (rootfs overlay + Go heap RAM) to GCS via suspend
	if _, err := b.runKubectlAte(ctx, "suspend", "actor", actorName, "-a", b.atespace, "--mode=sync"); err != nil {
		return 0, err
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	tr = b.ensureTrackerLocked(actorName)
	tr.SnapshotVersion = nextVer
	tr.Status = "SUSPENDED"
	tr.Snapshots = append(tr.Snapshots, SnapshotRecord{
		Version:   nextVer,
		Reason:    reason,
		CreatedAt: time.Now().UTC(),
	})
	return nextVer, nil
}

func (b *ClusterAteBackend) SuspendActor(ctx context.Context, actorName string) error {
	if _, err := b.runKubectlAte(ctx, "suspend", "actor", actorName, "-a", b.atespace, "--mode=sync"); err != nil {
		return err
	}
	b.mu.Lock()
	tr := b.ensureTrackerLocked(actorName)
	tr.Status = "SUSPENDED"
	b.mu.Unlock()
	return nil
}

func (b *ClusterAteBackend) RevertActor(ctx context.Context, actorName string, targetVersion int) (*ForensicsResponse, error) {
	// Revert running actor to its last committed GCS snapshot (if suspended, wake first so revert succeeds)
	if _, err := b.runKubectlAte(ctx, "revert", "actor", actorName, "-a", b.atespace); err != nil {
		var tmp ForensicsResponse
		_ = b.callActorJSON(ctx, actorName, http.MethodGet, "/forensics", "", nil, &tmp)
		_, _ = b.runKubectlAte(ctx, "revert", "actor", actorName, "-a", b.atespace)
	}

	workerIP, _, _ := b.inspectRunningActor(ctx, actorName)
	var forensicsResp ForensicsResponse
	if err := b.callActorJSON(ctx, actorName, http.MethodPost, "/checkpoint/restore", workerIP, map[string]any{
		"version": targetVersion,
	}, &forensicsResp); err != nil {
		return b.GetForensics(ctx, actorName)
	}
	if workerIP == "" {
		workerIP, _, _ = b.inspectRunningActor(ctx, actorName)
		if workerIP != "" && strings.HasPrefix(forensicsResp.CurrentWorkerPodIP, "169.254.") {
			_ = b.callActorJSON(ctx, actorName, http.MethodGet, "/forensics", workerIP, nil, &forensicsResp)
		}
	}

	b.mu.Lock()
	tr := b.ensureTrackerLocked(actorName)
	if targetVersion > 0 {
		tr.SnapshotVersion = targetVersion
	}
	tr.Status = "RUNNING"
	tr.LastForensics = &forensicsResp
	b.mu.Unlock()
	return &forensicsResp, nil
}

func (b *ClusterAteBackend) CloneActor(ctx context.Context, sourceActor, newActor, tagName string) (*ForensicsResponse, error) {
	_ = b.SuspendActor(ctx, sourceActor)
	_, _ = b.runKubectlAte(ctx, "delete", "tag", tagName, "-a", b.atespace)
	if _, err := b.runKubectlAte(ctx, "create", "tag", tagName, "-a", b.atespace, "--actor", sourceActor); err != nil {
		return nil, err
	}
	_, _ = b.runKubectlAte(ctx, "delete", "actor", newActor, "-a", b.atespace)
	if _, err := b.runKubectlAte(ctx, "create", "actor", newActor, "-a", b.atespace, "--template", b.template, "--tag", tagName); err != nil {
		return nil, err
	}
	b.mu.Lock()
	b.created[newActor] = true
	tr := b.ensureTrackerLocked(newActor)
	tr.IsClone = true
	tr.ClonedFrom = sourceActor
	tr.Label = fmt.Sprintf("%s [CLONE of %s]", newActor, sourceActor)
	b.mu.Unlock()
	return b.GetForensics(ctx, newActor)
}

func (b *ClusterAteBackend) ListActors(_ context.Context) ([]ActorTrackerState, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]ActorTrackerState, 0, len(b.order))
	for _, name := range b.order {
		if tr, ok := b.trackers[name]; ok {
			trCopy := *tr
			trCopy.Snapshots = slices.Clone(trCopy.Snapshots)
			out = append(out, trCopy)
		}
	}
	return out, nil
}
