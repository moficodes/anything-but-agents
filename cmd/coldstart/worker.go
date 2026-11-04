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
	"cmp"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"net"
	"net/http"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// WorkerConfig configures a WarmHeapEngine instance.
type WorkerConfig struct {
	SieveLimit       int
	CorpusDocs       int
	MaxConcurrent    int
	MinQueryDuration time.Duration
}

// SearchDoc represents an indexed cloud-native package/symbol document in RAM.
type SearchDoc struct {
	ID        uint32
	Package   string
	Symbol    string
	Synopsis  string
	Tokens    []string
	Authority float32
}

// SearchHit represents a single ranked search result returned by a query.
type SearchHit struct {
	DocID    uint32  `json:"doc_id"`
	Package  string  `json:"package"`
	Symbol   string  `json:"symbol"`
	Synopsis string  `json:"synopsis"`
	Score    float64 `json:"score"`
}

// WorkerStatusResponse is returned by GET /statusz at any point in the pod/actor lifecycle.
type WorkerStatusResponse struct {
	Ready                 bool    `json:"ready"`
	InitStage             string  `json:"init_stage"`
	InitStep              int     `json:"init_step"`
	InitTotalSteps        int     `json:"init_total_steps"`
	ColdBootDurationMs    int64   `json:"cold_boot_duration_ms"`
	GoldenHeapFingerprint string  `json:"golden_heap_fingerprint"`
	IndexDigest           string  `json:"index_digest"`
	DocsIndexed           int     `json:"docs_indexed"`
	TokensIndexed         int     `json:"tokens_indexed"`
	TrigramsIndexed       int     `json:"trigrams_indexed"`
	HeapAllocMB           float64 `json:"heap_alloc_mb"`
	InFlight              int64   `json:"in_flight"`
	MaxConcurrent         int     `json:"max_concurrent"`
	CapacityQPS           float64 `json:"capacity_qps"`
	TotalQueries          int64   `json:"total_queries"`
	TotalShed             int64   `json:"total_shed"`
	CurrentPodIP          string  `json:"current_pod_ip"`
}

// WarmHeapEngine holds the precomputed 50,000-document inverted token & trigram
// search index, BM25 weights, and prime sieve in Go heap RAM.
type WarmHeapEngine struct {
	ready               atomic.Bool
	initStep            atomic.Int32
	initStage           atomic.Value // string
	inFlight            atomic.Int64
	totalShed           atomic.Int64
	ColdBootStartedAt   time.Time
	ColdBootCompletedAt time.Time
	ColdBootDuration    time.Duration

	GoldenHeapFingerprint string
	IndexDigest           string
	GoldenHeapAllocMB     float64

	PrimesUpToN  int
	PrimeCount   int
	LargestPrime int
	Primes       []int32

	CorpusDocs       int
	MaxConcurrent    int
	MinQueryDuration time.Duration
	sem              chan struct{}

	// Read-only after RunExpensiveInitialization completes (shared across Golden Snapshot clones)
	docs         []SearchDoc
	tokenIndex   map[string][]uint32
	trigramIndex map[uint32][]uint32
	idfTable     map[string]float32
	avgDocLen    float32

	mu            sync.Mutex
	QueryCount    int      `json:"query_count"`
	CustomOverlay []string `json:"custom_overlay"`
	PodIPHistory  []string `json:"pod_ip_history"`
}

func newWarmHeapEngine(sieveLimit int) *WarmHeapEngine {
	corpusDocs := 50000
	if sieveLimit <= 1000 {
		// Fast path for unit tests using small sieve limits.
		corpusDocs = 500
	}
	return NewWarmHeapEngineWithConfig(WorkerConfig{
		SieveLimit:       sieveLimit,
		CorpusDocs:       corpusDocs,
		MaxConcurrent:    4,
		MinQueryDuration: 0,
	})
}

func NewWarmHeapEngineWithConfig(cfg WorkerConfig) *WarmHeapEngine {
	if cfg.SieveLimit <= 0 {
		cfg.SieveLimit = 1000000
	}
	if cfg.CorpusDocs <= 0 {
		cfg.CorpusDocs = 50000
	}
	if cfg.MaxConcurrent <= 0 {
		cfg.MaxConcurrent = 4
	}
	e := &WarmHeapEngine{
		ColdBootStartedAt: time.Now().UTC(),
		PrimesUpToN:       cfg.SieveLimit,
		CorpusDocs:        cfg.CorpusDocs,
		MaxConcurrent:     cfg.MaxConcurrent,
		MinQueryDuration:  cfg.MinQueryDuration,
		sem:               make(chan struct{}, cfg.MaxConcurrent),
	}
	e.initStage.Store("booting: waiting to start index build")
	return e
}

// CloneFromGoldenSnapshot creates an instant copy-on-write clone of an already-initialized
// WarmHeapEngine (used by --mode=local to simulate Substrate Golden Snapshot restoration).
func (e *WarmHeapEngine) CloneFromGoldenSnapshot() *WarmHeapEngine {
	e.mu.Lock()
	defer e.mu.Unlock()

	clone := &WarmHeapEngine{
		ColdBootStartedAt:     e.ColdBootStartedAt,
		ColdBootCompletedAt:   e.ColdBootCompletedAt,
		ColdBootDuration:      e.ColdBootDuration,
		GoldenHeapFingerprint: e.GoldenHeapFingerprint,
		IndexDigest:           e.IndexDigest,
		GoldenHeapAllocMB:     e.GoldenHeapAllocMB,
		PrimesUpToN:           e.PrimesUpToN,
		PrimeCount:            e.PrimeCount,
		LargestPrime:          e.LargestPrime,
		Primes:                e.Primes,
		CorpusDocs:            e.CorpusDocs,
		MaxConcurrent:         e.MaxConcurrent,
		MinQueryDuration:      e.MinQueryDuration,
		sem:                   make(chan struct{}, e.MaxConcurrent),
		docs:                  e.docs,
		tokenIndex:            e.tokenIndex,
		trigramIndex:          e.trigramIndex,
		idfTable:              e.idfTable,
		avgDocLen:             e.avgDocLen,
	}
	clone.initStep.Store(6)
	clone.initStage.Store("ready: restored from Golden Snapshot")
	clone.ready.Store(true)
	return clone
}

var (
	corpusDomains = []string{
		"k8s.io/autoscaler/v2",
		"k8s.io/apiserver/pkg/endpoints",
		"ate.dev/substrate/runtime",
		"ate.dev/substrate/snapshot",
		"ate.dev/atenet/router",
		"gvisor.dev/sentry/kernel",
		"gvisor.dev/sentry/mm",
		"storage.googleapis.com/gcs/blob",
		"envoyproxy.io/extproc/filter",
		"raft.consensus.io/quorum",
		"sqlite.org/durable/wal",
		"prometheus.io/adapter/external",
	}
	corpusPrefixes = []string{
		"Horizontal", "Snapshot", "Golden", "Adaptive", "Distributed", "Sandbox",
		"Memory", "Trigram", "Inverted", "Consistent", "Ephemeral", "Durable",
		"Concurrent", "Streaming", "Checkpoint", "Restore",
	}
	corpusNouns = []string{
		"PodAutoscaler", "HeapRestorer", "ActorRouter", "PostingList", "LeaseManager",
		"PageTable", "OverlayFS", "MetricsAdapter", "WalReplayer", "WorkerPool",
		"TokenBucket", "QuorumVoter", "CorpusIndexer", "BundleVerifier",
	}
	corpusVerbs = []string{
		"reconciles", "hydrates", "checkpoints", "intersects", "schedules",
		"multiplexes", "validates", "throttles", "broadcasts", "indexes",
	}
	corpusTopics = []string{
		"kubernetes horizontal pod autoscaler scale target",
		"gvisor sandbox memory snapshot restore",
		"golden heap fingerprint copy on write clone",
		"atenet envoy extproc request parking router",
		"durable directory write ahead log replay",
		"raft leader lease split brain wall clock",
		"external metrics api load balancer qps",
		"trigram inverted posting list bm25 ranking",
	}
)

func (e *WarmHeapEngine) RunExpensiveInitialization(delay time.Duration) {
	const totalSteps = 6
	stepTarget := delay / time.Duration(totalSteps)

	runStep := func(step int, desc string, fn func()) {
		stepStart := time.Now()
		e.initStep.Store(int32(step))
		e.initStage.Store(desc)
		slog.Info("Cold-start search engine warming heap...",
			slog.Int("step", step),
			slog.Int("total_steps", totalSteps),
			slog.String("stage", desc),
		)
		fn()
		if stepTarget > 0 {
			if rem := stepTarget - time.Since(stepStart); rem > 0 {
				time.Sleep(rem)
			}
		}
	}

	// Stage 1: Generate deterministic 50,000-document package & symbol corpus
	runStep(1, "Stage 1/6: Generating cloud-native package & symbol corpus", func() {
		e.docs = make([]SearchDoc, e.CorpusDocs)
		var totalTokens int
		for i := 0; i < e.CorpusDocs; i++ {
			dom := corpusDomains[i%len(corpusDomains)]
			pref := corpusPrefixes[(i*7+3)%len(corpusPrefixes)]
			noun := corpusNouns[(i*13+5)%len(corpusNouns)]
			verb := corpusVerbs[(i*17+11)%len(corpusVerbs)]
			topic := corpusTopics[(i*23+7)%len(corpusTopics)]

			pkg := fmt.Sprintf("%s/shard%03d", dom, i%128)
			sym := fmt.Sprintf("%s%sV%d", pref, noun, (i%9)+1)
			syn := fmt.Sprintf("%s %s %s for %s (doc #%d)", sym, verb, noun, topic, i)

			rawText := strings.ToLower(pkg + " " + sym + " " + syn)
			toks := tokenizeText(rawText)
			totalTokens += len(toks)
			e.docs[i] = SearchDoc{
				ID:        uint32(i),
				Package:   pkg,
				Symbol:    sym,
				Synopsis:  syn,
				Tokens:    toks,
				Authority: 1.0 + float32((i*31)%100)/100.0,
			}
		}
		if e.CorpusDocs > 0 {
			e.avgDocLen = float32(totalTokens) / float32(e.CorpusDocs)
		}
	})

	// Stage 2: Build inverted token posting lists
	runStep(2, "Stage 2/6: Building inverted token posting lists", func() {
		e.tokenIndex = make(map[string][]uint32, 4096)
		seenTok := make(map[string]struct{}, 32)
		for i := range e.docs {
			docID := e.docs[i].ID
			clear(seenTok)
			for _, tk := range e.docs[i].Tokens {
				if _, ok := seenTok[tk]; !ok {
					seenTok[tk] = struct{}{}
					e.tokenIndex[tk] = append(e.tokenIndex[tk], docID)
				}
			}
		}
	})

	// Stage 3: Build inverted 3-gram (trigram) substring index in RAM
	runStep(3, "Stage 3/6: Building inverted 3-gram substring index", func() {
		e.trigramIndex = make(map[uint32][]uint32, 8192)
		seen := make(map[uint32]uint32, 128)
		for i := range e.docs {
			docID := e.docs[i].ID
			text := strings.ToLower(e.docs[i].Symbol + " " + e.docs[i].Synopsis)
			clear(seen)
			for j := 0; j+3 <= len(text); j++ {
				tri := (uint32(text[j]) << 16) | (uint32(text[j+1]) << 8) | uint32(text[j+2])
				if _, exists := seen[tri]; !exists {
					seen[tri] = docID
					e.trigramIndex[tri] = append(e.trigramIndex[tri], docID)
				}
			}
		}
	})

	// Stage 4: Precompute BM25 Inverse Document Frequency (IDF) weights
	runStep(4, "Stage 4/6: Precomputing BM25 IDF weights & authority norms", func() {
		e.idfTable = make(map[string]float32, len(e.tokenIndex))
		n := float64(len(e.docs))
		for tk, postings := range e.tokenIndex {
			df := float64(len(postings))
			idf := math.Log(1.0 + (n-df+0.5)/(df+0.5))
			e.idfTable[tk] = float32(idf)
		}
	})

	// Stage 5: Compute prime lookup table up to PrimesUpToN in RAM
	runStep(5, "Stage 5/6: Computing prime lookup table in RAM", func() {
		primes, count, largest := sieveOfEratosthenesSlice(e.PrimesUpToN)
		e.Primes = primes
		e.PrimeCount = count
		e.LargestPrime = largest
	})

	// Stage 6: Seal Golden Snapshot heap & compute SHA-256 index digest
	runStep(6, "Stage 6/6: Sealing Golden Snapshot heap & index digest", func() {
		runtime.GC()
		h := sha256.New()
		fmt.Fprintf(h, "docs=%d;tokens=%d;trigrams=%d;primes=%d;largest=%d",
			len(e.docs), len(e.tokenIndex), len(e.trigramIndex), e.PrimeCount, e.LargestPrime)
		e.IndexDigest = hex.EncodeToString(h.Sum(nil))[:16]

		if e.GoldenHeapFingerprint == "" {
			var raw [8]byte
			_, _ = rand.Read(raw[:])
			e.GoldenHeapFingerprint = hex.EncodeToString(raw[:])
		}
		e.GoldenHeapAllocMB = currentHeapAllocMB()
	})

	e.ColdBootCompletedAt = time.Now().UTC()
	e.ColdBootDuration = e.ColdBootCompletedAt.Sub(e.ColdBootStartedAt)
	e.initStage.Store("ready: search index warm in RAM")
	e.ready.Store(true)

	slog.Info("Golden heap initialized; /readyz now returning 200 OK",
		slog.String("golden_heap_fingerprint", e.GoldenHeapFingerprint),
		slog.String("index_digest", e.IndexDigest),
		slog.Duration("cold_boot_duration", e.ColdBootDuration),
		slog.Int("docs_indexed", len(e.docs)),
		slog.Int("trigrams_indexed", len(e.trigramIndex)),
		slog.Int("prime_count", e.PrimeCount),
		slog.Float64("heap_alloc_mb", e.GoldenHeapAllocMB),
	)
}

func sieveOfEratosthenes(n int) (count int, largest int) {
	_, count, largest = sieveOfEratosthenesSlice(n)
	return count, largest
}

func sieveOfEratosthenesSlice(n int) (primes []int32, count int, largest int) {
	if n < 2 {
		return nil, 0, 0
	}
	composite := make([]bool, n+1)
	for p := 2; p*p <= n; p++ {
		if !composite[p] {
			for i := p * p; i <= n; i += p {
				composite[i] = true
			}
		}
	}
	primes = make([]int32, 0, n/10)
	for p := 2; p <= n; p++ {
		if !composite[p] {
			primes = append(primes, int32(p))
			count++
			largest = p
		}
	}
	return primes, count, largest
}

func tokenizeText(s string) []string {
	var tokens []string
	start := -1
	for i := 0; i < len(s); i++ {
		c := s[i]
		isAlphaNum := (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')
		if isAlphaNum {
			if start < 0 {
				start = i
			}
		} else if start >= 0 {
			if i-start >= 2 {
				tokens = append(tokens, s[start:i])
			}
			start = -1
		}
	}
	if start >= 0 && len(s)-start >= 2 {
		tokens = append(tokens, s[start:])
	}
	return tokens
}

func currentHeapAllocMB() float64 {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	mb := float64(ms.HeapAlloc) / (1024.0 * 1024.0)
	return math.Round(mb*100) / 100
}

var defaultSearchQueries = []string{
	"kubernetes horizontal pod autoscaler scale",
	"gvisor memory snapshot restore golden",
	"atenet router request parking actor",
	"durable wal storage replay checkpoint",
	"external metrics load balancer qps",
}

// SearchExecutionResult holds the detailed metrics and hits of an in-memory search query.
type SearchExecutionResult struct {
	Query                string      `json:"query"`
	Hits                 []SearchHit `json:"hits"`
	DocsScanned          int         `json:"docs_scanned"`
	PostingIntersections int         `json:"posting_intersections"`
	ComputeDurationMs    float64     `json:"compute_duration_ms"`
}

// RunSearch executes a real trigram + token + BM25 search over the resident heap index.
func (e *WarmHeapEngine) RunSearch(query string, customOverlay []string) SearchExecutionResult {
	start := time.Now()
	q := strings.TrimSpace(query)
	if q == "" {
		q = defaultSearchQueries[0]
	}
	lowerQ := strings.ToLower(q)
	qTokens := tokenizeText(lowerQ)

	// Collect candidate documents via token posting lists and trigram posting lists.
	candidateScores := make(map[uint32]float64, 512)
	intersections := 0

	for _, tk := range qTokens {
		postings := e.tokenIndex[tk]
		idf := float64(e.idfTable[tk])
		if idf == 0 {
			idf = 1.5
		}
		for _, docID := range postings {
			intersections++
			doc := &e.docs[docID]
			var tfCount int
			for _, dt := range doc.Tokens {
				if dt == tk {
					tfCount++
				}
			}
			tf := float64(tfCount)
			if tf == 0 {
				tf = 1.0
			}
			docLen := float64(len(doc.Tokens))
			avgLen := float64(e.avgDocLen)
			if avgLen <= 0 {
				avgLen = 10
			}
			// Standard BM25 term score (k1=1.2, b=0.75)
			bm25 := idf * ((tf * 2.2) / (tf + 1.2*(1.0-0.75+0.75*(docLen/avgLen))))
			candidateScores[docID] += bm25 * float64(doc.Authority)
		}
	}

	// Trigram substring boost
	maxTrigrams := 12
	triCount := 0
	for i := 0; i+3 <= len(lowerQ) && triCount < maxTrigrams; i++ {
		if lowerQ[i] == ' ' || lowerQ[i+1] == ' ' || lowerQ[i+2] == ' ' {
			continue
		}
		tri := (uint32(lowerQ[i]) << 16) | (uint32(lowerQ[i+1]) << 8) | uint32(lowerQ[i+2])
		triCount++
		postings := e.trigramIndex[tri]
		// Scan up to 1500 postings per trigram so each query exercises real memory bandwidth
		limit := len(postings)
		if limit > 1500 {
			limit = 1500
		}
		for _, docID := range postings[:limit] {
			intersections++
			candidateScores[docID] += 0.35
		}
	}

	type scoredDoc struct {
		docID uint32
		score float64
	}
	scored := make([]scoredDoc, 0, len(candidateScores))
	for docID, sc := range candidateScores {
		scored = append(scored, scoredDoc{docID: docID, score: sc})
	}
	slices.SortFunc(scored, func(a, b scoredDoc) int {
		if c := cmp.Compare(b.score, a.score); c != 0 {
			return c
		}
		return cmp.Compare(a.docID, b.docID)
	})

	var hits []SearchHit
	// Include any matching custom overlay symbols added to this specific actor clone
	for idx, sym := range customOverlay {
		if strings.Contains(strings.ToLower(sym), lowerQ) || len(hits) == 0 {
			hits = append(hits, SearchHit{
				DocID:    uint32(1000000 + idx),
				Package:  "actor.overlay/local",
				Symbol:   sym,
				Synopsis: fmt.Sprintf("Per-actor copy-on-write overlay symbol '%s' isolated in RAM", sym),
				Score:    99.9,
			})
			if len(hits) >= 2 {
				break
			}
		}
	}

	for _, item := range scored {
		if len(hits) >= 5 {
			break
		}
		doc := &e.docs[item.docID]
		hits = append(hits, SearchHit{
			DocID:    doc.ID,
			Package:  doc.Package,
			Symbol:   doc.Symbol,
			Synopsis: doc.Synopsis,
			Score:    math.Round(item.score*100) / 100,
		})
	}

	return SearchExecutionResult{
		Query:                q,
		Hits:                 hits,
		DocsScanned:          len(candidateScores),
		PostingIntersections: intersections,
		ComputeDurationMs:    math.Round(float64(time.Since(start).Microseconds())/10.0) / 100.0,
	}
}

func (e *WarmHeapEngine) HandleQuery(overlaySymbol string, podIP string) map[string]any {
	return e.HandleSearchQuery("", overlaySymbol, podIP)
}

func (e *WarmHeapEngine) HandleSearchQuery(query string, overlaySymbol string, podIP string) map[string]any {
	e.mu.Lock()
	e.QueryCount++
	qNum := e.QueryCount
	if overlaySymbol != "" && !slices.Contains(e.CustomOverlay, overlaySymbol) {
		e.CustomOverlay = append(e.CustomOverlay, overlaySymbol)
	}
	if podIP != "" && (len(e.PodIPHistory) == 0 || e.PodIPHistory[len(e.PodIPHistory)-1] != podIP) {
		e.PodIPHistory = append(e.PodIPHistory, podIP)
	}
	overlayCopy := slices.Clone(e.CustomOverlay)
	podHistoryCopy := slices.Clone(e.PodIPHistory)
	e.mu.Unlock()

	if query == "" {
		query = defaultSearchQueries[(qNum-1)%len(defaultSearchQueries)]
	}
	searchRes := e.RunSearch(query, overlayCopy)

	heapMB := currentHeapAllocMB()
	if heapMB < e.GoldenHeapAllocMB {
		heapMB = e.GoldenHeapAllocMB
	}

	return map[string]any{
		"status":                    "INSTANT_CLONE_FROM_GOLDEN_SNAPSHOT",
		"golden_heap_fingerprint":   e.GoldenHeapFingerprint,
		"index_digest":              e.IndexDigest,
		"cold_boot_duration_ms":     e.ColdBootDuration.Milliseconds(),
		"cold_boot_completed_at":    e.ColdBootCompletedAt.Format(time.RFC3339Nano),
		"served_at":                 time.Now().UTC().Format(time.RFC3339Nano),
		"time_since_golden_boot_s":  fmt.Sprintf("%.1fs", time.Now().Round(0).Sub(e.ColdBootCompletedAt.Round(0)).Seconds()),
		"docs_indexed":              len(e.docs),
		"tokens_indexed":            len(e.tokenIndex),
		"trigrams_indexed":          len(e.trigramIndex),
		"heap_alloc_mb":             heapMB,
		"precomputed_primes_limit":  e.PrimesUpToN,
		"precomputed_prime_count":   e.PrimeCount,
		"precomputed_largest_prime": e.LargestPrime,
		"search_query":              searchRes.Query,
		"search_docs_scanned":       searchRes.DocsScanned,
		"posting_intersections":     searchRes.PostingIntersections,
		"search_compute_ms":         searchRes.ComputeDurationMs,
		"search_hits":               searchRes.Hits,
		"actor_query_count":         qNum,
		"actor_custom_overlay":      overlayCopy,
		"current_pod_ip":            podIP,
		"pod_ip_history":            podHistoryCopy,
	}
}

func (e *WarmHeapEngine) StatusSnapshot(podIP string) WorkerStatusResponse {
	e.mu.Lock()
	totalQueries := int64(e.QueryCount)
	e.mu.Unlock()

	stage, _ := e.initStage.Load().(string)
	capQPS := 20.0
	if e.MinQueryDuration > 0 {
		capQPS = float64(e.MaxConcurrent) / e.MinQueryDuration.Seconds()
	}
	bootMs := e.ColdBootDuration.Milliseconds()
	if !e.ready.Load() {
		bootMs = time.Since(e.ColdBootStartedAt).Milliseconds()
	}
	heapMB := currentHeapAllocMB()
	if e.ready.Load() && heapMB < e.GoldenHeapAllocMB {
		heapMB = e.GoldenHeapAllocMB
	}

	return WorkerStatusResponse{
		Ready:                 e.ready.Load(),
		InitStage:             stage,
		InitStep:              int(e.initStep.Load()),
		InitTotalSteps:        6,
		ColdBootDurationMs:    bootMs,
		GoldenHeapFingerprint: e.GoldenHeapFingerprint,
		IndexDigest:           e.IndexDigest,
		DocsIndexed:           len(e.docs),
		TokensIndexed:         len(e.tokenIndex),
		TrigramsIndexed:       len(e.trigramIndex),
		HeapAllocMB:           heapMB,
		InFlight:              e.inFlight.Load(),
		MaxConcurrent:         e.MaxConcurrent,
		CapacityQPS:           capQPS,
		TotalQueries:          totalQueries,
		TotalShed:             e.totalShed.Load(),
		CurrentPodIP:          podIP,
	}
}

func newMux(engine *WarmHeapEngine) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if !engine.ready.Load() {
			stage, _ := engine.initStage.Load().(string)
			http.Error(w, fmt.Sprintf("still running expensive cold-start initialization (%s)...\n", stage), http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("/statusz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(engine.StatusSnapshot(currentPodIP()))
	})
	handleSearch := func(w http.ResponseWriter, r *http.Request) {
		if !engine.ready.Load() {
			stage, _ := engine.initStage.Load().(string)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status":        "COLD_BOOTING_NOT_READY",
				"init_stage":    stage,
				"init_step":     engine.initStep.Load(),
				"current_pod_ip": currentPodIP(),
			})
			return
		}

		// Enforce per-replica concurrency ceiling; allow a tiny 25ms buffer to absorb
		// network tick jitter at 100% capacity, then shed excess load with 429 Too Many Requests.
		select {
		case engine.sem <- struct{}{}:
		default:
			jitterTimer := time.NewTimer(25 * time.Millisecond)
			select {
			case engine.sem <- struct{}{}:
				jitterTimer.Stop()
			case <-jitterTimer.C:
				engine.totalShed.Add(1)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusTooManyRequests)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"status":         "OVERLOADED_SHED_LOAD",
					"reason":         fmt.Sprintf("replica saturated at max_concurrent=%d in-flight search workers", engine.MaxConcurrent),
					"in_flight":      engine.inFlight.Load(),
					"max_concurrent": engine.MaxConcurrent,
					"current_pod_ip": currentPodIP(),
				})
				return
			}
		}
		engine.inFlight.Add(1)
		defer func() {
			engine.inFlight.Add(-1)
			<-engine.sem
		}()

		reqStart := time.Now()
		query := r.URL.Query().Get("q")
		symbol := r.URL.Query().Get("add_symbol")
		resp := engine.HandleSearchQuery(query, symbol, currentPodIP())

		if engine.MinQueryDuration > 0 {
			targetDur := engine.MinQueryDuration
			if targetDur >= 180*time.Millisecond {
				// Leave 20ms headroom for HTTP/network round-trip so 4 concurrent slots
				// achieve a clean 20 QPS per Ready replica over cluster networking.
				targetDur -= 20 * time.Millisecond
			}
			if rem := targetDur - time.Since(reqStart); rem > 0 {
				time.Sleep(rem)
			}
		}

		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(resp)
	}
	mux.HandleFunc("/search", handleSearch)
	mux.HandleFunc("/", handleSearch)
	return mux
}

func currentPodIP() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return "unknown"
	}
	for _, addr := range addrs {
		if ipnet, ok := addr.(*net.IPNet); ok && !ipnet.IP.IsLoopback() && ipnet.IP.To4() != nil {
			return ipnet.IP.String()
		}
	}
	return "127.0.0.1"
}
