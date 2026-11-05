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
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// SlideResponse returns slide data along with live Substrate wake metrics.
type SlideResponse struct {
	Slide         SlideData `json:"slide"`
	WakeLatencyMs int64     `json:"wake_latency_ms"`
	Source        string    `json:"source"` // "live" | "simulated" | "cache"
	WorkerPod     string    `json:"worker_pod,omitempty"`
	WorkerIP      string    `json:"worker_ip,omitempty"`
	PodIPHistory  []string  `json:"pod_ip_history"`
}

// SlidesBackend abstracts access to the 16 slide actors.
type SlidesBackend interface {
	GetSlide(ctx context.Context, id string, previousID string) (*SlideResponse, error)
	UpdateSlideNote(ctx context.Context, id string, note string) (*SlideResponse, error)
	RevertSlide(ctx context.Context, id string) (*SlideResponse, error)
	SuspendSlide(ctx context.Context, id string) error
	ListSlides(ctx context.Context) ([]SlideSummary, error)
}

// LocalSimulatorBackend provides an in-memory simulation of all 16 slide actors.
type LocalSimulatorBackend struct {
	mu           sync.Mutex
	actors       map[string]*SlideActor
	summaries    []SlideSummary
	workerPool   []string
	podHistories map[string][]string
}

// NewLocalSimulatorBackend creates a local simulator with all 16 slide actors.
func NewLocalSimulatorBackend() *LocalSimulatorBackend {
	defs := AllSlideDefs()
	actors := make(map[string]*SlideActor, len(defs))
	summaries := make([]SlideSummary, len(defs))
	histories := make(map[string][]string, len(defs))

	pool := []string{"10.108.1.12", "10.108.2.15", "10.108.3.18"}

	for i, def := range defs {
		actors[def.ID] = NewSlideActor(def.ID)
		summaries[i] = SlideSummary{
			ID:      def.ID,
			Index:   def.Index,
			Total:   def.Total,
			Layout:  def.Layout,
			Chapter: def.Chapter,
			Badge:   def.Badge,
			Title:   def.Title,
			Kind:    def.Kind,
		}
		histories[def.ID] = []string{pool[i%len(pool)]}
	}

	return &LocalSimulatorBackend{
		actors:       actors,
		summaries:    summaries,
		workerPool:   pool,
		podHistories: histories,
	}
}

func (s *LocalSimulatorBackend) ListSlides(_ context.Context) ([]SlideSummary, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]SlideSummary, len(s.summaries))
	copy(out, s.summaries)
	return out, nil
}

func (s *LocalSimulatorBackend) GetSlide(_ context.Context, id string, _ string) (*SlideResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	actor, ok := s.actors[id]
	if !ok {
		return nil, fmt.Errorf("slide actor %q not found", id)
	}

	// Simulate realistic 70-110ms Substrate wake-on-HTTP latency
	latency := int64(75 + rand.Intn(35))
	time.Sleep(time.Duration(latency) * time.Millisecond)

	slide := actor.GetSlide()

	// Simulate rotating across the 3 worker pool pods
	history := s.podHistories[id]
	curWorker := s.workerPool[rand.Intn(len(s.workerPool))]
	if len(history) == 0 || history[len(history)-1] != curWorker {
		history = append(history, curWorker)
		s.podHistories[id] = history
	}
	slide.CurrentPodIP = curWorker
	slide.PodIPHistory = append([]string{}, history...)

	return &SlideResponse{
		Slide:         slide,
		WakeLatencyMs: latency,
		Source:        "simulated",
		WorkerPod:     "ateworker-gvisor-sim",
		WorkerIP:      curWorker,
		PodIPHistory:  slide.PodIPHistory,
	}, nil
}

func (s *LocalSimulatorBackend) UpdateSlideNote(_ context.Context, id string, note string) (*SlideResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	actor, ok := s.actors[id]
	if !ok {
		return nil, fmt.Errorf("slide actor %q not found", id)
	}

	slide := actor.UpdateNote(note)
	history := s.podHistories[id]
	slide.PodIPHistory = append([]string{}, history...)

	return &SlideResponse{
		Slide:         slide,
		WakeLatencyMs: 12,
		Source:        "simulated",
		WorkerPod:     "ateworker-gvisor-sim",
		WorkerIP:      slide.CurrentPodIP,
		PodIPHistory:  slide.PodIPHistory,
	}, nil
}

func (s *LocalSimulatorBackend) RevertSlide(_ context.Context, id string) (*SlideResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	actor, ok := s.actors[id]
	if !ok {
		return nil, fmt.Errorf("slide actor %q not found", id)
	}

	slide := actor.ResetToInitial()
	history := s.podHistories[id]
	slide.PodIPHistory = append([]string{}, history...)

	return &SlideResponse{
		Slide:         slide,
		WakeLatencyMs: 45,
		Source:        "simulated",
		WorkerPod:     "ateworker-gvisor-sim",
		WorkerIP:      slide.CurrentPodIP,
		PodIPHistory:  slide.PodIPHistory,
	}, nil
}

func (s *LocalSimulatorBackend) SuspendSlide(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.actors[id]; !ok {
		return fmt.Errorf("slide actor %q not found", id)
	}
	return nil
}

// ClusterAteBackend connects to real Agent Substrate actors via atenet-router and kubectl-ate.
type ClusterAteBackend struct {
	mu            sync.Mutex
	atespace      string
	template      string
	routerURL     string
	ateEndpoint   string
	kubectlAteBin string
	httpClient    *http.Client
	podHistories  map[string][]string
}

// NewClusterAteBackend creates a backend connecting to the live Substrate cluster.
func NewClusterAteBackend(atespace, template, routerURL, ateEndpoint, kubectlAteBin string) *ClusterAteBackend {
	normURL := normalizeRouterURL(routerURL)
	return &ClusterAteBackend{
		atespace:      atespace,
		template:      template,
		routerURL:     normURL,
		ateEndpoint:   ateEndpoint,
		kubectlAteBin: kubectlAteBin,
		httpClient:    newRouterHTTPClient(30 * time.Second),
		podHistories:  make(map[string][]string),
	}
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
		Proxy:               http.ProxyFromEnvironment,
		DialContext:         (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 20,
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
		Transport: tr,
		Timeout:   timeout,
	}
}

func (c *ClusterAteBackend) runAte(ctx context.Context, args ...string) ([]byte, error) {
	cmdArgs := []string{}
	if c.ateEndpoint != "" {
		cmdArgs = append(cmdArgs, "--endpoint", c.ateEndpoint)
	}
	cmdArgs = append(cmdArgs, args...)

	bin := c.kubectlAteBin
	if bin == "" {
		bin = "kubectl-ate"
	}

	cmd := exec.CommandContext(ctx, bin, cmdArgs...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err != nil {
		return stdout.Bytes(), fmt.Errorf("kubectl-ate %v failed: %w (stderr: %s)", args, err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

func (c *ClusterAteBackend) suspendActorAsync(actorID string) {
	if actorID == "" {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_, err := c.runAte(ctx, "suspend", "actor", actorID, "-a", c.atespace, "--mode=async")
		if err != nil {
			slog.Warn("Failed to suspend previous slide actor", slog.String("actor", actorID), slog.Any("err", err))
		} else {
			slog.Info("Suspended previous slide actor to free worker slot", slog.String("actor", actorID))
		}
	}()
}

func (c *ClusterAteBackend) reclaimWorkerSlots(ctx context.Context, keepActor string) {
	raw, err := c.runAte(ctx, "get", "actors", "-a", c.atespace)
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 || fields[0] == "ATESPACE" || strings.HasPrefix(fields[0], "Warning:") {
			continue
		}
		name := fields[1]
		state := fields[3]
		if name != keepActor && strings.Contains(state, "RUNNING") {
			slog.Info("Reclaiming worker slot: suspending non-target actor", slog.String("actor", name))
			_, _ = c.runAte(ctx, "suspend", "actor", name, "-a", c.atespace, "--mode=async")
		}
	}
}

func (c *ClusterAteBackend) queryWorkerPodInfo(ctx context.Context, actorID string) (podName, podIP string) {
	raw, err := c.runAte(ctx, "get", "actors", "-a", c.atespace)
	if err != nil {
		return "", ""
	}
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 6 || fields[0] == "ATESPACE" || strings.HasPrefix(fields[0], "Warning:") {
			continue
		}
		if fields[1] == actorID {
			if fields[4] != "<none>" {
				podName = fields[4]
			}
			if strings.Contains(fields[5], ".") {
				podIP = fields[5]
			}
			return podName, podIP
		}
	}
	return "", ""
}

func (c *ClusterAteBackend) ListSlides(_ context.Context) ([]SlideSummary, error) {
	defs := AllSlideDefs()
	out := make([]SlideSummary, len(defs))
	for i, def := range defs {
		out[i] = SlideSummary{
			ID:      def.ID,
			Index:   def.Index,
			Total:   def.Total,
			Layout:  def.Layout,
			Chapter: def.Chapter,
			Badge:   def.Badge,
			Title:   def.Title,
			Kind:    def.Kind,
		}
	}
	return out, nil
}

func (c *ClusterAteBackend) ensureSlideActor(ctx context.Context, actorID string) error {
	if _, ok := GetSlideDef(actorID); !ok {
		return fmt.Errorf("slide actor %q not found", actorID)
	}
	slog.Info("Creating slide actor from template on demand", slog.String("actor", actorID), slog.String("template", c.template))
	_, err := c.runAte(ctx, "create", "actor", actorID, "-a", c.atespace, "--template", c.template)
	if err != nil {
		lower := strings.ToLower(err.Error())
		if strings.Contains(lower, "already exists") {
			return nil
		}
		if strings.Contains(lower, "no worker") || strings.Contains(lower, "capacity") {
			c.reclaimWorkerSlots(ctx, actorID)
			_, retryErr := c.runAte(ctx, "create", "actor", actorID, "-a", c.atespace, "--template", c.template)
			if retryErr == nil || strings.Contains(strings.ToLower(retryErr.Error()), "already exists") {
				return nil
			}
			return retryErr
		}
		return err
	}
	return nil
}

func (c *ClusterAteBackend) callActorHTTP(ctx context.Context, actorID, method, path string, payload any) (*SlideData, int64, error) {
	if _, ok := GetSlideDef(actorID); !ok {
		return nil, 0, fmt.Errorf("slide actor %q not found", actorID)
	}

	var rawPayload []byte
	if payload != nil {
		rawPayload, _ = json.Marshal(payload)
	}

	target := fmt.Sprintf("%s/%s", c.atespace, actorID)
	reqURL := fmt.Sprintf("%s%s", c.routerURL, path)

	for attempt := 0; attempt < 3; attempt++ {
		start := time.Now()
		var bodyReader io.Reader
		if rawPayload != nil {
			bodyReader = bytes.NewReader(rawPayload)
		}

		req, err := http.NewRequestWithContext(ctx, method, reqURL, bodyReader)
		if err != nil {
			return nil, 0, fmt.Errorf("failed to create request: %w", err)
		}
		req.Header.Set("ate-target-actor", target)
		if rawPayload != nil {
			req.Header.Set("Content-Type", "application/json")
		}

		resp, err := c.httpClient.Do(req)
		latency := time.Since(start).Milliseconds()
		if err != nil {
			if attempt == 0 {
				c.httpClient.CloseIdleConnections()
				continue
			}
			return nil, latency, fmt.Errorf("atenet-router call failed: %w", err)
		}

		rawResp, _ := io.ReadAll(io.LimitReader(resp.Body, 524288))
		_ = resp.Body.Close()

		if resp.StatusCode == http.StatusNotFound && attempt < 2 && strings.Contains(strings.ToLower(string(rawResp)), "not found") {
			if createErr := c.ensureSlideActor(ctx, actorID); createErr != nil {
				return nil, latency, fmt.Errorf("failed to create slide actor %s: %w", actorID, createErr)
			}
			continue
		}

		if resp.StatusCode == http.StatusServiceUnavailable && attempt < 2 && strings.Contains(string(rawResp), "no worker has room") {
			slog.Warn("atenet-router reported 503 no worker has room; reclaiming slots and retrying...", slog.String("actor", actorID))
			c.reclaimWorkerSlots(ctx, actorID)
			continue
		}

		if resp.StatusCode >= 300 {
			return nil, latency, fmt.Errorf("actor returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(rawResp)))
		}

		var slide SlideData
		if err := json.Unmarshal(rawResp, &slide); err != nil {
			return nil, latency, fmt.Errorf("failed to unmarshal slide response: %w (raw: %s)", err, strings.TrimSpace(string(rawResp)))
		}
		return &slide, latency, nil
	}

	return nil, 0, fmt.Errorf("actor %s call failed after retry", actorID)
}

func (c *ClusterAteBackend) GetSlide(ctx context.Context, id string, previousID string) (*SlideResponse, error) {
	if previousID != "" && previousID != id {
		c.suspendActorAsync(previousID)
	}

	slide, latency, err := c.callActorHTTP(ctx, id, http.MethodGet, "/api/slide?id="+url.QueryEscape(id), nil)
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	podName, podIP := c.queryWorkerPodInfo(ctx, id)
	if podIP != "" {
		slide.CurrentPodIP = podIP
		history := c.podHistories[id]
		if len(history) == 0 || history[len(history)-1] != podIP {
			history = append(history, podIP)
			c.podHistories[id] = history
		}
		slide.PodIPHistory = append([]string{}, history...)
	}

	return &SlideResponse{
		Slide:         *slide,
		WakeLatencyMs: latency,
		Source:        "live",
		WorkerPod:     podName,
		WorkerIP:      slide.CurrentPodIP,
		PodIPHistory:  slide.PodIPHistory,
	}, nil
}

func (c *ClusterAteBackend) UpdateSlideNote(ctx context.Context, id string, note string) (*SlideResponse, error) {
	payload := map[string]string{"notes": note}
	slide, latency, err := c.callActorHTTP(ctx, id, http.MethodPost, "/api/slide/note?id="+url.QueryEscape(id), payload)
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	podName, podIP := c.queryWorkerPodInfo(ctx, id)
	if podIP != "" {
		slide.CurrentPodIP = podIP
		history := c.podHistories[id]
		slide.PodIPHistory = append([]string{}, history...)
	}

	return &SlideResponse{
		Slide:         *slide,
		WakeLatencyMs: latency,
		Source:        "live",
		WorkerPod:     podName,
		WorkerIP:      slide.CurrentPodIP,
		PodIPHistory:  slide.PodIPHistory,
	}, nil
}

func (c *ClusterAteBackend) RevertSlide(ctx context.Context, id string) (*SlideResponse, error) {
	if _, ok := GetSlideDef(id); !ok {
		return nil, fmt.Errorf("slide actor %q not found", id)
	}
	slog.Info("Reverting slide actor memory snapshot", slog.String("actor", id))
	_, err := c.runAte(ctx, "revert", "actor", id, "-a", c.atespace)
	if err != nil {
		return nil, fmt.Errorf("kubectl ate revert actor failed: %w", err)
	}
	return c.GetSlide(ctx, id, "")
}

func (c *ClusterAteBackend) SuspendSlide(ctx context.Context, id string) error {
	if _, ok := GetSlideDef(id); !ok {
		return fmt.Errorf("slide actor %q not found", id)
	}
	_, err := c.runAte(ctx, "suspend", "actor", id, "-a", c.atespace, "--mode=async")
	return err
}

// StartLocalRouterProxy runs a loopback bridge on listenAddr (e.g. 127.0.0.1:8000)
// forwarding plain HTTP to atenet-router over mTLS.
func (c *ClusterAteBackend) StartLocalRouterProxy(listenAddr string) {
	if listenAddr == "" {
		return
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		target := r.Header.Get("ate-target-actor")
		targetURL := fmt.Sprintf("%s%s", c.routerURL, r.URL.RequestURI())
		proxyReq, err := http.NewRequestWithContext(r.Context(), r.Method, targetURL, r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		proxyReq.Header = r.Header.Clone()
		if target != "" {
			proxyReq.Header.Set("ate-target-actor", target)
		}

		resp, err := c.httpClient.Do(proxyReq)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()

		for k, vv := range resp.Header {
			for _, v := range vv {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	})

	server := &http.Server{
		Addr:    listenAddr,
		Handler: mux,
	}
	slog.Info("Starting local router HTTP-to-mTLS loopback proxy", slog.String("addr", listenAddr), slog.String("target", c.routerURL))
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		slog.Warn("Local router proxy exited", slog.Any("err", err))
	}
}
