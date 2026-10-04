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
	"cmp"
	"context"
	"crypto/tls"
	"crypto/x509"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

//go:embed web/*.html
var webFS embed.FS

const (
	defaultMinReplicas      = 1
	defaultMaxReplicas      = 3
	defaultTargetQPSPerPod  = 20.0
	maxHistoryPoints        = 60
	maxRecentSearchSamples  = 16
)

// GatewayConfig configures the Coldstart HPA & QPS GatewayServer.
type GatewayConfig struct {
	Mode                   string
	Namespace              string
	Atespace               string
	Template               string
	K8sServiceURL          string
	RouterURL              string
	AteEndpoint            string
	KubectlAteBin          string
	K8sDeploymentName      string
	SubScaleDeploymentName string

	// Tunables for local simulator mode and unit tests
	K8sColdBootDelay       time.Duration
	SubstrateWakeDelay     time.Duration
	WorkerMinQueryDuration time.Duration
	WorkerMaxConcurrent    int
	CorpusDocs             int
	SieveLimit             int
	TickInterval           time.Duration
}

// ReplicaStatus describes one K8s Pod or Substrate Actor replica slot (1..3).
type ReplicaStatus struct {
	Index                 int     `json:"index"`
	Name                  string  `json:"name"`
	Backend               string  `json:"backend"`
	State                 string  `json:"state"` // READY | COLD_BOOTING | WAKING | SUSPENDED | OFFLINE
	Ready                 bool    `json:"ready"`
	InitStage             string  `json:"init_stage"`
	InitStep              int     `json:"init_step"`
	InitTotalSteps        int     `json:"init_total_steps"`
	BootElapsedMs         int64   `json:"boot_elapsed_ms"`
	BootTargetMs          int64   `json:"boot_target_ms"`
	WakeLatencyMs         int64   `json:"wake_latency_ms"`
	GoldenHeapFingerprint string  `json:"golden_heap_fingerprint"`
	HeapAllocMB           float64 `json:"heap_alloc_mb"`
	PodIP                 string  `json:"pod_ip"`
	InFlight              int64   `json:"in_flight"`
	MaxConcurrent         int     `json:"max_concurrent"`
	CapacityQPS           float64 `json:"capacity_qps"`
	CurrentQPS            float64 `json:"current_qps"`
	ServedCount           int64   `json:"served_count"`
	ShedCount             int64   `json:"shed_count"`
}

// BackendTelemetry summarizes HPA state, capacity, and QPS goodput for one backend.
type BackendTelemetry struct {
	Backend            string          `json:"backend"`
	Title              string          `json:"title"`
	Subtitle           string          `json:"subtitle"`
	HPAName            string          `json:"hpa_name"`
	ScaleTarget        string          `json:"scale_target"`
	HPAMinReplicas     int             `json:"hpa_min_replicas"`
	HPAMaxReplicas     int             `json:"hpa_max_replicas"`
	HPATargetAvgQPS    float64         `json:"hpa_target_avg_qps"`
	HPACurrentLBQPS    float64         `json:"hpa_current_lb_qps"`
	HPADesiredReplicas int             `json:"hpa_desired_replicas"`
	ReadyReplicas      int             `json:"ready_replicas"`
	ActiveCapacityQPS  float64         `json:"active_capacity_qps"`
	GoodputQPS         float64         `json:"goodput_qps"`
	ShedQPS            float64         `json:"shed_qps"`
	TotalGoodput       int64           `json:"total_goodput"`
	TotalShed          int64           `json:"total_shed"`
	SuccessRatePct     float64         `json:"success_rate_pct"`
	P50LatencyMs       float64         `json:"p50_latency_ms"`
	P99LatencyMs       float64         `json:"p99_latency_ms"`
	ScaleOutReadyMs    int64           `json:"scale_out_ready_ms"`
	Replicas           []ReplicaStatus `json:"replicas"`
}

// TimeSeriesPoint captures a 1-second slice of the comparative HPA & QPS race.
type TimeSeriesPoint struct {
	Timestamp     string  `json:"timestamp"`
	ElapsedSec    int     `json:"elapsed_sec"`
	Phase         string  `json:"phase"`
	TargetQPS     float64 `json:"target_qps"`
	K8sDesired    int     `json:"k8s_desired"`
	K8sReady      int     `json:"k8s_ready"`
	K8sGoodputQPS float64 `json:"k8s_goodput_qps"`
	K8sShedQPS    float64 `json:"k8s_shed_qps"`
	SubDesired    int     `json:"sub_desired"`
	SubReady      int     `json:"sub_ready"`
	SubGoodputQPS float64 `json:"sub_goodput_qps"`
	SubShedQPS    float64 `json:"sub_shed_qps"`
}

// RecentSearchSample records a live search request and response from either backend.
type RecentSearchSample struct {
	Timestamp         string  `json:"timestamp"`
	Backend           string  `json:"backend"`
	Replica           string  `json:"replica"`
	Query             string  `json:"query"`
	Status            int     `json:"status"`
	LatencyMs         float64 `json:"latency_ms"`
	DocsScanned       int     `json:"docs_scanned"`
	TopSymbol         string  `json:"top_symbol"`
	GoldenFingerprint string  `json:"golden_fingerprint"`
}

// GatewayStateSnapshot is returned by GET /api/state and POST /api/spike?wait=1.
type GatewayStateSnapshot struct {
	Mode               string               `json:"mode"`
	Namespace          string               `json:"namespace"`
	Atespace           string               `json:"atespace"`
	Template           string               `json:"template"`
	ScenarioRunning    bool                 `json:"scenario_running"`
	ScenarioPhase      string               `json:"scenario_phase"`
	ScenarioElapsedSec int                  `json:"scenario_elapsed_sec"`
	TargetQPS          float64              `json:"target_qps"`
	GoodputMultiplier  float64              `json:"goodput_multiplier"`
	ExtraQueriesServed int64                `json:"extra_queries_served"`
	K8s                BackendTelemetry     `json:"k8s"`
	Substrate          BackendTelemetry     `json:"substrate"`
	History            []TimeSeriesPoint    `json:"history"`
	RecentSearches     []RecentSearchSample `json:"recent_searches"`
}

type localReplicaSlot struct {
	index         int
	name          string
	backend       string
	podIP         string
	state         string // READY | COLD_BOOTING | WAKING | SUSPENDED | OFFLINE
	bootStartedAt time.Time
	wakeLatencyMs int64
	engine        *WarmHeapEngine
	mux           *http.ServeMux
	servedTotal   atomic.Int64
	shedTotal     atomic.Int64
	servedWindow  atomic.Int64
}

type backendWindowStats struct {
	mu           sync.Mutex
	goodputWin   int64
	shedWin      int64
	totalGoodput int64
	totalShed    int64
	latenciesMs  []float64
	lastGoodput  float64
	lastShed     float64
	lastP50      float64
	lastP99      float64
}

func (b *backendWindowStats) record(ok bool, latencyMs float64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if ok {
		b.goodputWin++
		b.totalGoodput++
		b.latenciesMs = append(b.latenciesMs, latencyMs)
	} else {
		b.shedWin++
		b.totalShed++
	}
}

func (b *backendWindowStats) rollWindow(windowDuration time.Duration) (goodputQPS, shedQPS, p50, p99 float64, totGood, totShed int64) {
	b.mu.Lock()
	defer b.mu.Unlock()

	secs := windowDuration.Seconds()
	if secs <= 0 {
		secs = 1.0
	}
	b.lastGoodput = math.Round((float64(b.goodputWin)/secs)*10) / 10
	b.lastShed = math.Round((float64(b.shedWin)/secs)*10) / 10
	b.goodputWin = 0
	b.shedWin = 0

	if len(b.latenciesMs) > 0 {
		slices.Sort(b.latenciesMs)
		b.lastP50 = math.Round(percentileSorted(b.latenciesMs, 0.50)*10) / 10
		b.lastP99 = math.Round(percentileSorted(b.latenciesMs, 0.99)*10) / 10
		b.latenciesMs = b.latenciesMs[:0]
	}

	return b.lastGoodput, b.lastShed, b.lastP50, b.lastP99, b.totalGoodput, b.totalShed
}

func (b *backendWindowStats) reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.goodputWin = 0
	b.shedWin = 0
	b.totalGoodput = 0
	b.totalShed = 0
	b.latenciesMs = nil
	b.lastGoodput = 0
	b.lastShed = 0
	b.lastP50 = 0
	b.lastP99 = 0
}

func percentileSorted(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(math.Ceil(float64(len(sorted))*p)) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

// GatewayServer coordinates the dual K8s vs Substrate load balancer, traffic generator,
// Kubernetes external.metrics.k8s.io/v1beta1 endpoint, and live web dashboard.
type GatewayServer struct {
	cfg        GatewayConfig
	httpClient *http.Client
	k8sClient  *http.Client
	k8sToken   string

	mu                 sync.Mutex
	targetQPS          float64
	scenarioRunning    bool
	scenarioCancel     context.CancelFunc
	scenarioPhase      string
	scenarioElapsedSec int
	elapsedCounter     int

	k8sDesired      int
	subDesired      int
	spikeTriggeredAt time.Time
	k8sScaleReadyMs int64
	subScaleReadyMs int64

	k8sStats *backendWindowStats
	subStats *backendWindowStats

	history        []TimeSeriesPoint
	recentSearches []RecentSearchSample

	// Local simulator state (also used as fallback mirror in gateway mode)
	goldenTemplate *WarmHeapEngine
	localK8sSlots  []*localReplicaSlot
	localSubSlots  []*localReplicaSlot
	clusterK8sRep  []ReplicaStatus
	clusterSubRep  []ReplicaStatus

	rrK8s atomic.Uint64
	rrSub atomic.Uint64
	qSeq  atomic.Uint64
}

func NewGatewayServer(cfg GatewayConfig) *GatewayServer {
	if cfg.Namespace == "" {
		cfg.Namespace = "anything-but-agents"
	}
	if cfg.Atespace == "" {
		cfg.Atespace = "anything-but-agents"
	}
	if cfg.Template == "" {
		cfg.Template = "coldstart"
	}
	if cfg.K8sDeploymentName == "" {
		cfg.K8sDeploymentName = "coldstart-k8s"
	}
	if cfg.SubScaleDeploymentName == "" {
		cfg.SubScaleDeploymentName = "coldstart-substrate-pool"
	}
	if cfg.K8sServiceURL == "" {
		cfg.K8sServiceURL = fmt.Sprintf("http://coldstart-k8s.%s.svc.cluster.local:80", cfg.Namespace)
	}
	if cfg.RouterURL == "" {
		cfg.RouterURL = "http://atenet-router.ate-system.svc.cluster.local:80"
	}
	if cfg.KubectlAteBin == "" {
		cfg.KubectlAteBin = "kubectl-ate"
	}
	if cfg.K8sColdBootDelay <= 0 {
		cfg.K8sColdBootDelay = 12 * time.Second
	}
	if cfg.SubstrateWakeDelay <= 0 {
		cfg.SubstrateWakeDelay = 95 * time.Millisecond
	}
	if cfg.WorkerMaxConcurrent <= 0 {
		cfg.WorkerMaxConcurrent = 4
	}
	if cfg.WorkerMinQueryDuration <= 0 {
		cfg.WorkerMinQueryDuration = 200 * time.Millisecond
	}
	if cfg.CorpusDocs <= 0 {
		cfg.CorpusDocs = 50000
	}
	if cfg.SieveLimit <= 0 {
		cfg.SieveLimit = 1000000
	}
	if cfg.TickInterval <= 0 {
		cfg.TickInterval = 100 * time.Millisecond
	}

	g := &GatewayServer{
		cfg: cfg,
		httpClient: &http.Client{
			Timeout: 4 * time.Second,
		},
		k8sDesired:    defaultMinReplicas,
		subDesired:    defaultMinReplicas,
		scenarioPhase: "IDLE",
		k8sStats:      &backendWindowStats{},
		subStats:      &backendWindowStats{},
	}
	g.initK8sClient()
	g.initLocalSimulator()
	return g
}

func (g *GatewayServer) initK8sClient() {
	tokenBytes, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/token")
	if err == nil {
		g.k8sToken = strings.TrimSpace(string(tokenBytes))
	}
	caBytes, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/ca.crt")
	pool := x509.NewCertPool()
	if err == nil {
		pool.AppendCertsFromPEM(caBytes)
	}
	g.k8sClient = &http.Client{
		Timeout: 3 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				RootCAs:            pool,
				InsecureSkipVerify: len(caBytes) == 0,
			},
		},
	}
}

func (g *GatewayServer) initLocalSimulator() {
	// Build the Golden Snapshot template once at gateway startup.
	tmpl := NewWarmHeapEngineWithConfig(WorkerConfig{
		SieveLimit:       g.cfg.SieveLimit,
		CorpusDocs:       g.cfg.CorpusDocs,
		MaxConcurrent:    g.cfg.WorkerMaxConcurrent,
		MinQueryDuration: g.cfg.WorkerMinQueryDuration,
	})
	tmpl.RunExpensiveInitialization(0)
	// Record the nominal 12s cold-boot duration in the Golden Snapshot metadata.
	tmpl.ColdBootDuration = g.cfg.K8sColdBootDelay
	if tmpl.GoldenHeapAllocMB < 64.2 {
		tmpl.GoldenHeapAllocMB = 64.2
	}
	g.goldenTemplate = tmpl

	g.resetLocalSlotsLocked()
}

func (g *GatewayServer) resetLocalSlotsLocked() {
	g.localK8sSlots = make([]*localReplicaSlot, defaultMaxReplicas)
	g.localSubSlots = make([]*localReplicaSlot, defaultMaxReplicas)

	for i := 0; i < defaultMaxReplicas; i++ {
		k8sEng := g.goldenTemplate.CloneFromGoldenSnapshot()
		// Each K8s pod cold-boots independently and therefore generates its own heap fingerprint!
		k8sEng.GoldenHeapFingerprint = fmt.Sprintf("k8s-pod%d-%s", i+1, g.goldenTemplate.GoldenHeapFingerprint[:8])
		k8sSlot := &localReplicaSlot{
			index:   i + 1,
			name:    fmt.Sprintf("coldstart-k8s-pod-%d", i+1),
			backend: "k8s",
			podIP:   fmt.Sprintf("10.244.1.%d", 10+i),
			engine:  k8sEng,
			mux:     newMux(k8sEng),
		}
		if i == 0 {
			k8sSlot.state = "READY"
			k8sEng.ready.Store(true)
		} else {
			k8sSlot.state = "OFFLINE"
			k8sEng.ready.Store(false)
			k8sEng.initStep.Store(0)
			k8sEng.initStage.Store("offline: waiting for HPA scale-up")
		}
		g.localK8sSlots[i] = k8sSlot

		// Every Substrate actor clone shares the EXACT same GoldenHeapFingerprint from the ActorTemplate!
		subEng := g.goldenTemplate.CloneFromGoldenSnapshot()
		subSlot := &localReplicaSlot{
			index:         i + 1,
			name:          fmt.Sprintf("coldstart-%d", i+1),
			backend:       "substrate",
			podIP:         fmt.Sprintf("10.244.2.%d", 20+i),
			wakeLatencyMs: g.cfg.SubstrateWakeDelay.Milliseconds() + int64(i*7),
			engine:        subEng,
			mux:           newMux(subEng),
		}
		if i == 0 {
			subSlot.state = "READY"
			subEng.ready.Store(true)
		} else {
			subSlot.state = "SUSPENDED"
			subEng.ready.Store(true)
			subEng.initStage.Store("suspended in GCS ($0 compute, Golden Snapshot ready)")
		}
		g.localSubSlots[i] = subSlot
	}
}

// StartBackgroundLoops starts the continuous traffic generator & 1s telemetry aggregation loop.
func (g *GatewayServer) StartBackgroundLoops(ctx context.Context) {
	go func() {
		tick := time.NewTicker(g.cfg.TickInterval)
		defer tick.Stop()

		ticksPerRoll := int(math.Max(1, math.Round(float64(time.Second)/float64(g.cfg.TickInterval))))
		tickCount := 0
		var k8sAcc, subAcc float64

		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				g.mu.Lock()
				qps := g.targetQPS
				g.mu.Unlock()

				if qps > 0 {
					perTick := qps * g.cfg.TickInterval.Seconds()
					k8sAcc += perTick
					subAcc += perTick
					nK8s := int(k8sAcc)
					nSub := int(subAcc)
					k8sAcc -= float64(nK8s)
					subAcc -= float64(nSub)
					if nK8s > 0 || nSub > 0 {
						go g.dispatchBurst(nK8s, nSub)
					}
				} else {
					k8sAcc = 0
					subAcc = 0
				}

				tickCount++
				if tickCount >= ticksPerRoll {
					tickCount = 0
					g.rollOneSecondTelemetry(time.Duration(ticksPerRoll) * g.cfg.TickInterval)
				}
			}
		}
	}()

	if g.cfg.Mode == "gateway" {
		// Ensure coldstart-1..3 exist in the atespace and sync cluster pod/actor status.
		go g.ensureSubstrateActorsLoop(ctx)
	}
}

func (g *GatewayServer) ensureSubstrateActorsLoop(ctx context.Context) {
	for i := 1; i <= defaultMaxReplicas; i++ {
		actorName := fmt.Sprintf("coldstart-%d", i)
		_ = g.runKubectlAte(ctx, "create", "actor", actorName, "-a", g.cfg.Atespace, "--template", g.cfg.Template)
		if i > 1 {
			_ = g.runKubectlAte(ctx, "suspend", "actor", actorName, "-a", g.cfg.Atespace)
		}
	}
	// Wake coldstart-1 so baseline starts with 1 warm actor matching K8s minReplicas: 1.
	g.wakeSubstrateActorAsync(1)

	ticker := time.NewTicker(1500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			g.refreshClusterReplicaStatuses(ctx)
		}
	}
}

func (g *GatewayServer) runKubectlAte(ctx context.Context, args ...string) error {
	cmdArgs := slices.Clone(args)
	if g.cfg.AteEndpoint != "" {
		cmdArgs = append(cmdArgs, "--endpoint", g.cfg.AteEndpoint)
	}
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cctx, g.cfg.KubectlAteBin, cmdArgs...)
	out, err := cmd.CombinedOutput()
	if err != nil && !strings.Contains(string(out), "AlreadyExists") {
		slog.Debug("kubectl-ate command returned non-zero", slog.Any("args", args), slog.String("output", string(out)))
	}
	return err
}

func computeHPADesiredReplicas(qps float64) int {
	if qps <= 0 {
		return defaultMinReplicas
	}
	desired := int(math.Ceil(qps / defaultTargetQPSPerPod))
	if desired < defaultMinReplicas {
		desired = defaultMinReplicas
	}
	if desired > defaultMaxReplicas {
		desired = defaultMaxReplicas
	}
	return desired
}

// CurrentLBQPS returns the current Load Balancer QPS for k8s and substrate
// (used by external.metrics.k8s.io/v1beta1).
func (g *GatewayServer) CurrentLBQPS() (k8sQPS, subQPS float64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.targetQPS, g.targetQPS
}

// SetTargetQPS updates the incoming Load Balancer traffic demand and triggers HPA reconciliation.
func (g *GatewayServer) SetTargetQPS(qps float64, phase string) {
	if qps < 0 {
		qps = 0
	}
	if qps > 100 {
		qps = 100
	}
	g.mu.Lock()
	prevDesired := g.k8sDesired
	g.targetQPS = qps
	if phase != "" {
		g.scenarioPhase = phase
	}
	desired := computeHPADesiredReplicas(qps)
	g.k8sDesired = desired
	g.subDesired = desired
	if desired > prevDesired && desired == defaultMaxReplicas {
		g.spikeTriggeredAt = time.Now()
		g.k8sScaleReadyMs = 0
		g.subScaleReadyMs = 0
	}
	g.reconcileReplicasLocked(desired)
	g.mu.Unlock()
}

func (g *GatewayServer) reconcileReplicasLocked(desired int) {
	now := time.Now()

	// Reconcile local/simulated K8s pods (1..3)
	for i := 0; i < defaultMaxReplicas; i++ {
		slot := g.localK8sSlots[i]
		if i < desired {
			if slot.state == "OFFLINE" {
				slot.state = "COLD_BOOTING"
				slot.bootStartedAt = now
				slot.engine.ready.Store(false)
				slot.engine.ColdBootStartedAt = now
				slot.engine.initStep.Store(1)
				slot.engine.initStage.Store("Stage 1/6: Generating cloud-native package & symbol corpus")
				bootDelay := g.cfg.K8sColdBootDelay
				go g.simulateK8sPodColdBoot(slot, bootDelay)
			}
		} else if i > 0 {
			slot.state = "OFFLINE"
			slot.engine.ready.Store(false)
			slot.engine.initStep.Store(0)
			slot.engine.initStage.Store("offline: scaled down by HPA")
		}
	}

	// Reconcile local/simulated Substrate actors (1..3)
	for i := 0; i < defaultMaxReplicas; i++ {
		slot := g.localSubSlots[i]
		if i < desired {
			if slot.state == "SUSPENDED" || slot.state == "OFFLINE" {
				slot.state = "WAKING"
				slot.bootStartedAt = now
				wakeDelay := g.cfg.SubstrateWakeDelay + time.Duration(i*7)*time.Millisecond
				go g.simulateSubstrateActorWake(slot, wakeDelay)
			}
		} else if i > 0 {
			if slot.state != "SUSPENDED" {
				slot.state = "SUSPENDED"
				slot.engine.initStage.Store("suspended in GCS ($0 compute, Golden Snapshot ready)")
				if g.cfg.Mode == "gateway" {
					actorIdx := i + 1
					go func() {
						_ = g.runKubectlAte(context.Background(), "suspend", "actor", fmt.Sprintf("coldstart-%d", actorIdx), "-a", g.cfg.Atespace)
					}()
				}
			}
		}
	}

	if g.cfg.Mode == "gateway" {
		go g.syncClusterScales(desired)
	}
}

func (g *GatewayServer) simulateK8sPodColdBoot(slot *localReplicaSlot, bootDelay time.Duration) {
	stages := []string{
		"Stage 1/6: Generating cloud-native package & symbol corpus",
		"Stage 2/6: Building inverted token posting lists",
		"Stage 3/6: Building inverted 3-gram substring index",
		"Stage 4/6: Precomputing BM25 IDF weights & authority norms",
		"Stage 5/6: Computing prime lookup table in RAM",
		"Stage 6/6: Sealing heap & flipping /readyz -> 200 OK",
	}
	stepDur := bootDelay / time.Duration(len(stages))
	for idx, st := range stages {
		g.mu.Lock()
		if slot.state != "COLD_BOOTING" {
			g.mu.Unlock()
			return
		}
		slot.engine.initStep.Store(int32(idx + 1))
		slot.engine.initStage.Store(st)
		g.mu.Unlock()
		if stepDur > 0 {
			time.Sleep(stepDur)
		}
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	if slot.state == "COLD_BOOTING" {
		slot.state = "READY"
		slot.engine.ready.Store(true)
		slot.engine.initStep.Store(6)
		slot.engine.initStage.Store("ready: search index warm in RAM")
		if g.k8sScaleReadyMs == 0 && !g.spikeTriggeredAt.IsZero() && g.countReadyLocalK8sLocked() == defaultMaxReplicas {
			g.k8sScaleReadyMs = time.Since(g.spikeTriggeredAt).Milliseconds()
		}
	}
}

func (g *GatewayServer) simulateSubstrateActorWake(slot *localReplicaSlot, wakeDelay time.Duration) {
	if g.cfg.Mode == "gateway" {
		g.wakeSubstrateActorAsync(slot.index)
	}
	if wakeDelay > 0 {
		time.Sleep(wakeDelay)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if slot.state == "WAKING" {
		slot.state = "READY"
		slot.wakeLatencyMs = wakeDelay.Milliseconds()
		slot.engine.ready.Store(true)
		slot.engine.initStage.Store(fmt.Sprintf("restored from Golden Snapshot in %dms", slot.wakeLatencyMs))
		if g.subScaleReadyMs == 0 && !g.spikeTriggeredAt.IsZero() && g.countReadyLocalSubLocked() == defaultMaxReplicas {
			g.subScaleReadyMs = time.Since(g.spikeTriggeredAt).Milliseconds()
		}
	}
}

func (g *GatewayServer) wakeSubstrateActorAsync(index int) {
	go func() {
		actorName := fmt.Sprintf("coldstart-%d", index)
		req, err := http.NewRequest(http.MethodGet, strings.TrimRight(g.cfg.RouterURL, "/")+"/statusz", nil)
		if err != nil {
			return
		}
		req.Header.Set("ate-target-actor", fmt.Sprintf("%s/%s", g.cfg.Atespace, actorName))
		resp, err := g.httpClient.Do(req)
		if err == nil {
			_ = resp.Body.Close()
		}
	}()
}

func (g *GatewayServer) syncClusterScales(desired int) {
	g.scaleK8sDeployment(g.cfg.K8sDeploymentName, desired)
	g.scaleK8sDeployment(g.cfg.SubScaleDeploymentName, desired)
}

func (g *GatewayServer) scaleK8sDeployment(deployName string, replicas int) {
	if g.k8sToken == "" {
		// Fallback to kubectl if running outside cluster
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = exec.CommandContext(ctx, "kubectl", "-n", g.cfg.Namespace, "scale", "deployment/"+deployName, fmt.Sprintf("--replicas=%d", replicas)).Run()
		return
	}
	scaleURL := fmt.Sprintf("https://kubernetes.default.svc/apis/apps/v1/namespaces/%s/deployments/%s/scale", g.cfg.Namespace, deployName)
	patch := []byte(fmt.Sprintf(`{"spec":{"replicas":%d}}`, replicas))
	req, err := http.NewRequest(http.MethodPatch, scaleURL, bytes.NewReader(patch))
	if err != nil {
		return
	}
	req.Header.Set("Authorization", "Bearer "+g.k8sToken)
	req.Header.Set("Content-Type", "application/merge-patch+json")
	resp, err := g.k8sClient.Do(req)
	if err == nil {
		_ = resp.Body.Close()
	}
}

func (g *GatewayServer) refreshClusterReplicaStatuses(ctx context.Context) {
	if g.k8sToken == "" {
		return
	}
	podsURL := fmt.Sprintf("https://kubernetes.default.svc/api/v1/namespaces/%s/pods?labelSelector=app=%s", g.cfg.Namespace, g.cfg.K8sDeploymentName)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, podsURL, nil)
	if err != nil {
		return
	}
	req.Header.Set("Authorization", "Bearer "+g.k8sToken)
	resp, err := g.k8sClient.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return
	}

	var podList struct {
		Items []struct {
			Metadata struct {
				Name              string    `json:"name"`
				CreationTimestamp time.Time `json:"creationTimestamp"`
				DeletionTimestamp string    `json:"deletionTimestamp"`
			} `json:"metadata"`
			Status struct {
				Phase      string `json:"phase"`
				PodIP      string `json:"podIP"`
				Conditions []struct {
					Type   string `json:"type"`
					Status string `json:"status"`
				} `json:"conditions"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&podList); err != nil {
		return
	}

	// Sort active pods by creation timestamp so Pod 1 is always slot 1
	slices.SortFunc(podList.Items, func(a, b struct {
		Metadata struct {
			Name              string    `json:"name"`
			CreationTimestamp time.Time `json:"creationTimestamp"`
			DeletionTimestamp string    `json:"deletionTimestamp"`
		} `json:"metadata"`
		Status struct {
			Phase      string `json:"phase"`
			PodIP      string `json:"podIP"`
			Conditions []struct {
				Type   string `json:"type"`
				Status string `json:"status"`
			} `json:"conditions"`
		} `json:"status"`
	}) int {
		return a.Metadata.CreationTimestamp.Compare(b.Metadata.CreationTimestamp)
	})

	g.mu.Lock()
	defer g.mu.Unlock()

	activeIdx := 0
	for _, item := range podList.Items {
		if item.Metadata.DeletionTimestamp != "" {
			continue
		}
		if activeIdx >= len(g.localK8sSlots) {
			break
		}
		slot := g.localK8sSlots[activeIdx]
		activeIdx++
		slot.name = item.Metadata.Name
		if item.Status.PodIP != "" {
			slot.podIP = item.Status.PodIP
		}
		isReady := false
		for _, cond := range item.Status.Conditions {
			if cond.Type == "Ready" && cond.Status == "True" {
				isReady = true
				break
			}
		}
		if isReady {
			slot.state = "READY"
			slot.engine.ready.Store(true)
			slot.engine.initStep.Store(6)
			slot.engine.initStage.Store("ready: search index warm in RAM")
			if g.k8sScaleReadyMs == 0 && !g.spikeTriggeredAt.IsZero() && g.countReadyLocalK8sLocked() == defaultMaxReplicas {
				g.k8sScaleReadyMs = time.Since(g.spikeTriggeredAt).Milliseconds()
			}
		} else {
			if slot.state != "COLD_BOOTING" {
				slot.state = "COLD_BOOTING"
				slot.bootStartedAt = item.Metadata.CreationTimestamp
			}
			slot.engine.ready.Store(false)
		}
	}
}

func (g *GatewayServer) dispatchBurst(nK8s, nSub int) {
	var wg sync.WaitGroup
	for i := 0; i < nK8s; i++ {
		wg.Add(1)
		q := defaultSearchQueries[int(g.qSeq.Add(1))%len(defaultSearchQueries)]
		go func(query string) {
			defer wg.Done()
			g.sendOneK8sRequest(query)
		}(q)
	}
	for i := 0; i < nSub; i++ {
		wg.Add(1)
		q := defaultSearchQueries[int(g.qSeq.Add(1))%len(defaultSearchQueries)]
		go func(query string) {
			defer wg.Done()
			g.sendOneSubstrateRequest(query)
		}(q)
	}
	wg.Wait()
}

func (g *GatewayServer) sendOneK8sRequest(query string) {
	start := time.Now()

	// Pick a READY K8s replica (just like kube-proxy / Service Endpoints only routes to Ready pods)
	g.mu.Lock()
	var readySlots []*localReplicaSlot
	for _, s := range g.localK8sSlots {
		if s.state == "READY" {
			readySlots = append(readySlots, s)
		}
	}
	g.mu.Unlock()

	if len(readySlots) == 0 {
		g.k8sStats.record(false, float64(time.Since(start).Milliseconds()))
		return
	}
	slot := readySlots[int(g.rrK8s.Add(1))%len(readySlots)]

	if g.cfg.Mode == "gateway" && g.k8sToken != "" {
		reqURL := fmt.Sprintf("%s/search?q=%s", strings.TrimRight(g.cfg.K8sServiceURL, "/"), url.QueryEscape(query))
		resp, err := g.httpClient.Get(reqURL)
		latMs := math.Round(float64(time.Since(start).Microseconds())/100.0) / 10.0
		if err != nil {
			slot.shedTotal.Add(1)
			g.k8sStats.record(false, latMs)
			return
		}
		defer resp.Body.Close()
		var body map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&body)
		if resp.StatusCode == http.StatusOK {
			slot.servedTotal.Add(1)
			slot.servedWindow.Add(1)
			g.k8sStats.record(true, latMs)
			g.recordSearchSample("k8s", slot.name, query, http.StatusOK, latMs, body)
		} else {
			slot.shedTotal.Add(1)
			g.k8sStats.record(false, latMs)
		}
		return
	}

	// Local simulator execution against the replica's HTTP mux
	req := httptest.NewRequest(http.MethodGet, "/search?q="+url.QueryEscape(query), nil)
	rec := httptest.NewRecorder()
	slot.mux.ServeHTTP(rec, req)
	latMs := math.Round(float64(time.Since(start).Microseconds())/100.0) / 10.0

	if rec.Code == http.StatusOK {
		slot.servedTotal.Add(1)
		slot.servedWindow.Add(1)
		g.k8sStats.record(true, latMs)
		var body map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		g.recordSearchSample("k8s", slot.name, query, http.StatusOK, latMs, body)
	} else {
		slot.shedTotal.Add(1)
		g.k8sStats.record(false, latMs)
	}
}

func (g *GatewayServer) sendOneSubstrateRequest(query string) {
	start := time.Now()

	g.mu.Lock()
	desired := g.subDesired
	var activeSlots []*localReplicaSlot
	for i := 0; i < desired && i < len(g.localSubSlots); i++ {
		if g.localSubSlots[i].state == "READY" {
			activeSlots = append(activeSlots, g.localSubSlots[i])
		}
	}
	g.mu.Unlock()

	if len(activeSlots) == 0 {
		g.subStats.record(false, float64(time.Since(start).Milliseconds()))
		return
	}
	slot := activeSlots[int(g.rrSub.Add(1))%len(activeSlots)]

	if g.cfg.Mode == "gateway" && g.k8sToken != "" {
		reqURL := fmt.Sprintf("%s/search?q=%s", strings.TrimRight(g.cfg.RouterURL, "/"), url.QueryEscape(query))
		req, err := http.NewRequest(http.MethodGet, reqURL, nil)
		if err != nil {
			g.subStats.record(false, 0)
			return
		}
		req.Header.Set("ate-target-actor", fmt.Sprintf("%s/%s", g.cfg.Atespace, slot.name))
		resp, err := g.httpClient.Do(req)
		latMs := math.Round(float64(time.Since(start).Microseconds())/100.0) / 10.0
		if err != nil {
			slot.shedTotal.Add(1)
			g.subStats.record(false, latMs)
			return
		}
		defer resp.Body.Close()
		var body map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&body)
		if fp, ok := body["golden_heap_fingerprint"].(string); ok && fp != "" {
			slot.engine.GoldenHeapFingerprint = fp
		}
		if ip, ok := body["current_pod_ip"].(string); ok && ip != "" {
			slot.podIP = ip
		}
		if resp.StatusCode == http.StatusOK {
			slot.servedTotal.Add(1)
			slot.servedWindow.Add(1)
			g.subStats.record(true, latMs)
			g.recordSearchSample("substrate", slot.name, query, http.StatusOK, latMs, body)
		} else {
			slot.shedTotal.Add(1)
			g.subStats.record(false, latMs)
		}
		return
	}

	req := httptest.NewRequest(http.MethodGet, "/search?q="+url.QueryEscape(query), nil)
	rec := httptest.NewRecorder()
	slot.mux.ServeHTTP(rec, req)
	latMs := math.Round(float64(time.Since(start).Microseconds())/100.0) / 10.0

	if rec.Code == http.StatusOK {
		slot.servedTotal.Add(1)
		slot.servedWindow.Add(1)
		g.subStats.record(true, latMs)
		var body map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		g.recordSearchSample("substrate", slot.name, query, http.StatusOK, latMs, body)
	} else {
		slot.shedTotal.Add(1)
		g.subStats.record(false, latMs)
	}
}

func (g *GatewayServer) recordSearchSample(backend, replica, query string, status int, latMs float64, body map[string]any) {
	docsScanned, _ := toInt(body["search_docs_scanned"])
	fp, _ := body["golden_heap_fingerprint"].(string)
	topSym := ""
	if hits, ok := body["search_hits"].([]any); ok && len(hits) > 0 {
		if first, ok := hits[0].(map[string]any); ok {
			pkg, _ := first["package"].(string)
			sym, _ := first["symbol"].(string)
			topSym = pkg + "." + sym
		}
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	sample := RecentSearchSample{
		Timestamp:         time.Now().UTC().Format("15:04:05.000"),
		Backend:           backend,
		Replica:           replica,
		Query:             query,
		Status:            status,
		LatencyMs:         latMs,
		DocsScanned:       docsScanned,
		TopSymbol:         topSym,
		GoldenFingerprint: fp,
	}
	g.recentSearches = append([]RecentSearchSample{sample}, g.recentSearches...)
	if len(g.recentSearches) > maxRecentSearchSamples {
		g.recentSearches = g.recentSearches[:maxRecentSearchSamples]
	}
}

func toInt(v any) (int, bool) {
	switch val := v.(type) {
	case int:
		return val, true
	case float64:
		return int(val), true
	default:
		return 0, false
	}
}

func (g *GatewayServer) rollOneSecondTelemetry(windowDur time.Duration) {
	k8sGood, k8sShed, _, _, _, _ := g.k8sStats.rollWindow(windowDur)
	subGood, subShed, _, _, _, _ := g.subStats.rollWindow(windowDur)

	g.mu.Lock()
	defer g.mu.Unlock()

	g.elapsedCounter++
	if g.scenarioRunning {
		g.scenarioElapsedSec++
	}

	pt := TimeSeriesPoint{
		Timestamp:     time.Now().UTC().Format("15:04:05"),
		ElapsedSec:    g.elapsedCounter,
		Phase:         g.scenarioPhase,
		TargetQPS:     g.targetQPS,
		K8sDesired:    g.k8sDesired,
		K8sReady:      g.countReadyLocalK8sLocked(),
		K8sGoodputQPS: k8sGood,
		K8sShedQPS:    k8sShed,
		SubDesired:    g.subDesired,
		SubReady:      g.countReadyLocalSubLocked(),
		SubGoodputQPS: subGood,
		SubShedQPS:    subShed,
	}
	g.history = append(g.history, pt)
	if len(g.history) > maxHistoryPoints {
		g.history = g.history[len(g.history)-maxHistoryPoints:]
	}
}

func (g *GatewayServer) countReadyLocalK8sLocked() int {
	n := 0
	for _, s := range g.localK8sSlots {
		if s.state == "READY" {
			n++
		}
	}
	return n
}

func (g *GatewayServer) countReadyLocalSubLocked() int {
	n := 0
	for i := 0; i < g.subDesired && i < len(g.localSubSlots); i++ {
		if g.localSubSlots[i].state == "READY" {
			n++
		}
	}
	return n
}

// Reset clears counters, history, and resets both HPAs back to minReplicas (1/3).
func (g *GatewayServer) Reset() {
	g.mu.Lock()
	if g.scenarioCancel != nil {
		g.scenarioCancel()
		g.scenarioCancel = nil
	}
	g.scenarioRunning = false
	g.scenarioPhase = "IDLE"
	g.scenarioElapsedSec = 0
	g.elapsedCounter = 0
	g.targetQPS = 0
	g.k8sDesired = defaultMinReplicas
	g.subDesired = defaultMinReplicas
	g.spikeTriggeredAt = time.Time{}
	g.k8sScaleReadyMs = 0
	g.subScaleReadyMs = 0
	g.history = nil
	g.recentSearches = nil
	g.resetLocalSlotsLocked()
	if g.cfg.Mode == "gateway" {
		go g.syncClusterScales(defaultMinReplicas)
	}
	g.mu.Unlock()

	g.k8sStats.reset()
	g.subStats.reset()
}

// RunSpikeScenario runs a 3-phase traffic profile (Baseline -> Spike -> Cooldown).
func (g *GatewayServer) RunSpikeScenario(ctx context.Context, baselineQPS, spikeQPS float64, baselineDur, spikeDur, cooldownDur time.Duration) {
	g.Reset()

	sctx, cancel := context.WithCancel(ctx)
	g.mu.Lock()
	g.scenarioRunning = true
	g.scenarioCancel = cancel
	g.mu.Unlock()

	defer func() {
		cancel()
		g.mu.Lock()
		g.scenarioRunning = false
		g.scenarioPhase = "COMPLETE"
		g.targetQPS = 0
		g.mu.Unlock()
	}()

	if baselineDur > 0 {
		g.SetTargetQPS(baselineQPS, fmt.Sprintf("BASELINE (%.0f QPS)", baselineQPS))
		select {
		case <-sctx.Done():
			return
		case <-time.After(baselineDur):
		}
	}

	if spikeDur > 0 {
		g.SetTargetQPS(spikeQPS, fmt.Sprintf("TRAFFIC SPIKE (%.0f QPS)", spikeQPS))
		select {
		case <-sctx.Done():
			return
		case <-time.After(spikeDur):
		}
	}

	if cooldownDur > 0 {
		g.SetTargetQPS(baselineQPS, fmt.Sprintf("COOLDOWN (%.0f QPS)", baselineQPS))
		select {
		case <-sctx.Done():
			return
		case <-time.After(cooldownDur):
		}
	}
}

// StateSnapshot returns the complete current state for the dashboard and CLI runner.
func (g *GatewayServer) StateSnapshot() GatewayStateSnapshot {
	g.mu.Lock()
	defer g.mu.Unlock()

	k8sReady := g.countReadyLocalK8sLocked()
	subReady := g.countReadyLocalSubLocked()

	g.k8sStats.mu.Lock()
	k8sGoodQPS := g.k8sStats.lastGoodput
	k8sShedQPS := g.k8sStats.lastShed
	k8sTotGood := g.k8sStats.totalGoodput
	k8sTotShed := g.k8sStats.totalShed
	k8sP50 := g.k8sStats.lastP50
	k8sP99 := g.k8sStats.lastP99
	g.k8sStats.mu.Unlock()

	g.subStats.mu.Lock()
	subGoodQPS := g.subStats.lastGoodput
	subShedQPS := g.subStats.lastShed
	subTotGood := g.subStats.totalGoodput
	subTotShed := g.subStats.totalShed
	subP50 := g.subStats.lastP50
	subP99 := g.subStats.lastP99
	g.subStats.mu.Unlock()

	k8sRate := 100.0
	if k8sTotGood+k8sTotShed > 0 {
		k8sRate = math.Round((float64(k8sTotGood)/float64(k8sTotGood+k8sTotShed))*1000) / 10
	}
	subRate := 100.0
	if subTotGood+subTotShed > 0 {
		subRate = math.Round((float64(subTotGood)/float64(subTotGood+subTotShed))*1000) / 10
	}

	multiplier := 1.0
	if k8sTotGood > 0 && subTotGood > k8sTotGood {
		multiplier = math.Round((float64(subTotGood)/float64(k8sTotGood))*100) / 100
	}
	extra := subTotGood - k8sTotGood
	if extra < 0 {
		extra = 0
	}

	k8sReplicas := make([]ReplicaStatus, len(g.localK8sSlots))
	for i, s := range g.localK8sSlots {
		st := s.engine.StatusSnapshot(s.podIP)
		elapsedMs := g.cfg.K8sColdBootDelay.Milliseconds()
		if s.state == "COLD_BOOTING" {
			elapsedMs = time.Since(s.bootStartedAt).Milliseconds()
			if elapsedMs > g.cfg.K8sColdBootDelay.Milliseconds() {
				elapsedMs = g.cfg.K8sColdBootDelay.Milliseconds()
			}
		} else if s.state == "OFFLINE" {
			elapsedMs = 0
		}
		k8sReplicas[i] = ReplicaStatus{
			Index:                 s.index,
			Name:                  s.name,
			Backend:               "k8s",
			State:                 s.state,
			Ready:                 s.state == "READY",
			InitStage:             st.InitStage,
			InitStep:              st.InitStep,
			InitTotalSteps:        st.InitTotalSteps,
			BootElapsedMs:         elapsedMs,
			BootTargetMs:          g.cfg.K8sColdBootDelay.Milliseconds(),
			GoldenHeapFingerprint: st.GoldenHeapFingerprint,
			HeapAllocMB:           st.HeapAllocMB,
			PodIP:                 s.podIP,
			InFlight:              st.InFlight,
			MaxConcurrent:         st.MaxConcurrent,
			CapacityQPS:           defaultTargetQPSPerPod,
			ServedCount:           s.servedTotal.Load(),
			ShedCount:             s.shedTotal.Load(),
		}
	}

	subReplicas := make([]ReplicaStatus, len(g.localSubSlots))
	for i, s := range g.localSubSlots {
		st := s.engine.StatusSnapshot(s.podIP)
		subReplicas[i] = ReplicaStatus{
			Index:                 s.index,
			Name:                  s.name,
			Backend:               "substrate",
			State:                 s.state,
			Ready:                 s.state == "READY",
			InitStage:             st.InitStage,
			InitStep:              st.InitStep,
			InitTotalSteps:        st.InitTotalSteps,
			BootElapsedMs:         s.wakeLatencyMs,
			BootTargetMs:          g.cfg.K8sColdBootDelay.Milliseconds(),
			WakeLatencyMs:         s.wakeLatencyMs,
			GoldenHeapFingerprint: st.GoldenHeapFingerprint,
			HeapAllocMB:           st.HeapAllocMB,
			PodIP:                 s.podIP,
			InFlight:              st.InFlight,
			MaxConcurrent:         st.MaxConcurrent,
			CapacityQPS:           defaultTargetQPSPerPod,
			ServedCount:           s.servedTotal.Load(),
			ShedCount:             s.shedTotal.Load(),
		}
	}

	return GatewayStateSnapshot{
		Mode:               g.cfg.Mode,
		Namespace:          g.cfg.Namespace,
		Atespace:           g.cfg.Atespace,
		Template:           g.cfg.Template,
		ScenarioRunning:    g.scenarioRunning,
		ScenarioPhase:      g.scenarioPhase,
		ScenarioElapsedSec: g.scenarioElapsedSec,
		TargetQPS:          g.targetQPS,
		GoodputMultiplier:  multiplier,
		ExtraQueriesServed: extra,
		K8s: BackendTelemetry{
			Backend:            "k8s",
			Title:              "Standard Kubernetes HPA + Deployment",
			Subtitle:           "12s Cold-Start Index Build on Scale-Out (/readyz == 503)",
			HPAName:            "coldstart-k8s",
			ScaleTarget:        "Deployment/" + g.cfg.K8sDeploymentName,
			HPAMinReplicas:     defaultMinReplicas,
			HPAMaxReplicas:     defaultMaxReplicas,
			HPATargetAvgQPS:    defaultTargetQPSPerPod,
			HPACurrentLBQPS:    g.targetQPS,
			HPADesiredReplicas: g.k8sDesired,
			ReadyReplicas:      k8sReady,
			ActiveCapacityQPS:  float64(k8sReady) * defaultTargetQPSPerPod,
			GoodputQPS:         k8sGoodQPS,
			ShedQPS:            k8sShedQPS,
			TotalGoodput:       k8sTotGood,
			TotalShed:          k8sTotShed,
			SuccessRatePct:     k8sRate,
			P50LatencyMs:       k8sP50,
			P99LatencyMs:       k8sP99,
			ScaleOutReadyMs:    g.k8sScaleReadyMs,
			Replicas:           k8sReplicas,
		},
		Substrate: BackendTelemetry{
			Backend:            "substrate",
			Title:              "Agent Substrate HPA + Golden Snapshot Actors",
			Subtitle:           "~100ms Memory Restore from Post-Init Golden Snapshot",
			HPAName:            "coldstart-substrate",
			ScaleTarget:        "ActorPool/coldstart-1..3",
			HPAMinReplicas:     defaultMinReplicas,
			HPAMaxReplicas:     defaultMaxReplicas,
			HPATargetAvgQPS:    defaultTargetQPSPerPod,
			HPACurrentLBQPS:    g.targetQPS,
			HPADesiredReplicas: g.subDesired,
			ReadyReplicas:      subReady,
			ActiveCapacityQPS:  float64(subReady) * defaultTargetQPSPerPod,
			GoodputQPS:         subGoodQPS,
			ShedQPS:            subShedQPS,
			TotalGoodput:       subTotGood,
			TotalShed:          subTotShed,
			SuccessRatePct:     subRate,
			P50LatencyMs:       subP50,
			P99LatencyMs:       subP99,
			ScaleOutReadyMs:    g.subScaleReadyMs,
			Replicas:           subReplicas,
		},
		History:        slices.Clone(g.history),
		RecentSearches: slices.Clone(g.recentSearches),
	}
}

// Mux returns the HTTP handler for the Coldstart Gateway & Live Web Dashboard.
func (g *GatewayServer) Mux() *http.ServeMux {
	mux := http.NewServeMux()

	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})

	mux.HandleFunc("/api/state", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(g.StateSnapshot())
	})

	mux.HandleFunc("/api/target-qps", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		qpsStr := r.URL.Query().Get("qps")
		if qpsStr == "" {
			var body struct {
				QPS float64 `json:"qps"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			qpsStr = fmt.Sprintf("%f", body.QPS)
		}
		qps, _ := strconv.ParseFloat(qpsStr, 64)
		g.SetTargetQPS(qps, fmt.Sprintf("MANUAL (%.0f QPS)", qps))
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(g.StateSnapshot())
	})

	mux.HandleFunc("/api/reset", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		g.Reset()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(g.StateSnapshot())
	})

	mux.HandleFunc("/api/spike", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		baselineQPS := parseFloatDefault(r.URL.Query().Get("baseline_qps"), 15.0)
		spikeQPS := parseFloatDefault(r.URL.Query().Get("spike_qps"), 60.0)
		baseSec := parseFloatDefault(r.URL.Query().Get("baseline_sec"), 3.0)
		spikeSec := parseFloatDefault(r.URL.Query().Get("spike_sec"), 18.0)
		coolSec := parseFloatDefault(r.URL.Query().Get("cooldown_sec"), 3.0)
		wait := r.URL.Query().Get("wait") == "1"

		baseDur := time.Duration(baseSec * float64(time.Second))
		spikeDur := time.Duration(spikeSec * float64(time.Second))
		coolDur := time.Duration(coolSec * float64(time.Second))

		if wait {
			g.RunSpikeScenario(r.Context(), baselineQPS, spikeQPS, baseDur, spikeDur, coolDur)
		} else {
			go g.RunSpikeScenario(context.Background(), baselineQPS, spikeQPS, baseDur, spikeDur, coolDur)
		}

		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(g.StateSnapshot())
	})

	serveDashboard := func(w http.ResponseWriter, _ *http.Request) {
		data, err := webFS.ReadFile("web/dashboard.html")
		if err != nil {
			http.Error(w, "dashboard asset missing", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.Copy(w, bytes.NewReader(data))
	}
	mux.HandleFunc("/dashboard", serveDashboard)
	mux.HandleFunc("/", serveDashboard)

	return mux
}

func parseFloatDefault(s string, def float64) float64 {
	if s == "" {
		return def
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return def
	}
	return cmp.Or(v, def)
}
