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

// Command coldstart provides both:
//  1. A production-realistic "Cold-Start Monster" In-Memory Package & Code Search
//     Engine (--mode=worker, default) that builds a 50,000-document inverted
//     token & trigram search index (~65 MB of Go heap) across 6 stages (~12s)
//     before /readyz flips from 503 to 200 OK, and enforces a bounded 20 QPS
//     per-replica concurrency ceiling.
//  2. A Dual-Backend HPA & Production QPS Comparison Gateway (--mode=gateway
//     or --mode=local) that drives identical traffic spikes against a Standard
//     Kubernetes Deployment+HPA and an Agent Substrate Golden Snapshot Actor
//     Pool+HPA, serving `coldstart_lb_qps` on external.metrics.k8s.io/v1beta1.
package main

import (
	"context"
	"crypto/tls"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"time"
)

func main() {
	addr := flag.String("addr", ":80", "HTTP listen address")
	mode := flag.String("mode", "worker", "Operating mode: worker | gateway | local")
	initDelay := flag.Duration("init-delay", 12*time.Second, "Heavy cold-start index build duration before /readyz reports 200 OK")
	sieveLimit := flag.Int("sieve-limit", 1000000, "Upper bound for prime lookup sieve precomputed in Golden Snapshot RAM")
	corpusDocs := flag.Int("corpus-docs", 50000, "Number of package & symbol documents indexed in RAM during cold boot")
	maxConcurrent := flag.Int("max-concurrent", 4, "Maximum concurrent search queries per replica before shedding load with 429")
	minQueryDuration := flag.Duration("min-query-duration", 200*time.Millisecond, "Minimum per-query execution time (4 concurrent / 200ms = 20 QPS ceiling per replica)")

	// Gateway / HPA flags
	metricsAddr := flag.String("metrics-addr", ":6443", "HTTPS listen address for Kubernetes external.metrics.k8s.io/v1beta1 (empty to disable)")
	namespace := flag.String("namespace", "anything-but-agents", "Kubernetes namespace for HPA and Deployment resources")
	atespace := flag.String("atespace", "anything-but-agents", "Agent Substrate atespace")
	template := flag.String("template", "coldstart", "Agent Substrate ActorTemplate name")
	k8sServiceURL := flag.String("k8s-service-url", "http://coldstart-k8s.anything-but-agents.svc.cluster.local:80", "Standard K8s Service URL for coldstart-k8s")
	routerURL := flag.String("router-url", "http://atenet-router.ate-system.svc.cluster.local:80", "Agent Substrate atenet-router HTTP URL")
	ateEndpoint := flag.String("ate-endpoint", "", "Optional ate-api-server gRPC endpoint override for kubectl-ate")
	kubectlAteBin := flag.String("kubectl-ate-bin", "kubectl-ate", "Path to kubectl-ate binary")
	flag.Parse()

	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	switch *mode {
	case "worker", "actor":
		engine := NewWarmHeapEngineWithConfig(WorkerConfig{
			SieveLimit:       *sieveLimit,
			CorpusDocs:       *corpusDocs,
			MaxConcurrent:    *maxConcurrent,
			MinQueryDuration: *minQueryDuration,
		})

		go func() {
			slog.Info("Starting coldstart worker HTTP listener (returning 503 on /readyz until 50,000-doc search index finishes building)",
				slog.String("addr", *addr),
				slog.Int("corpus_docs", *corpusDocs),
				slog.Int("max_concurrent", *maxConcurrent),
				slog.Duration("min_query_duration", *minQueryDuration),
			)
			if err := http.ListenAndServe(*addr, newMux(engine)); err != nil {
				slog.Error("Worker server exited", slog.Any("err", err))
				os.Exit(1)
			}
		}()

		engine.RunExpensiveInitialization(*initDelay)
		select {}

	case "gateway", "local":
		gw := NewGatewayServer(GatewayConfig{
			Mode:                   *mode,
			Namespace:              *namespace,
			Atespace:               *atespace,
			Template:               *template,
			K8sServiceURL:          *k8sServiceURL,
			RouterURL:              *routerURL,
			AteEndpoint:            *ateEndpoint,
			KubectlAteBin:          *kubectlAteBin,
			K8sColdBootDelay:       *initDelay,
			SubstrateWakeDelay:     95 * time.Millisecond,
			WorkerMinQueryDuration: *minQueryDuration,
			WorkerMaxConcurrent:    *maxConcurrent,
			CorpusDocs:             *corpusDocs,
			SieveLimit:             *sieveLimit,
		})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		gw.StartBackgroundLoops(ctx)

		if *mode == "gateway" && *metricsAddr != "" {
			go func() {
				cert, err := generateSelfSignedCert()
				if err != nil {
					slog.Error("Failed to generate self-signed TLS cert for external metrics server", slog.Any("err", err))
					return
				}
				srv := &http.Server{
					Addr:    *metricsAddr,
					Handler: gw.NewExternalMetricsMux(),
					TLSConfig: &tls.Config{
						Certificates: []tls.Certificate{cert},
					},
				}
				slog.Info("Starting Kubernetes external.metrics.k8s.io/v1beta1 HTTPS server", slog.String("metrics_addr", *metricsAddr))
				if err := srv.ListenAndServeTLS("", ""); err != nil {
					slog.Error("External metrics HTTPS server exited", slog.Any("err", err))
				}
			}()
		}

		slog.Info("Starting coldstart HPA & QPS Comparison Gateway",
			slog.String("mode", *mode),
			slog.String("addr", *addr),
			slog.String("namespace", *namespace),
			slog.String("atespace", *atespace),
		)
		if err := http.ListenAndServe(*addr, gw.Mux()); err != nil {
			slog.Error("Gateway HTTP server exited", slog.Any("err", err))
			os.Exit(1)
		}

	default:
		slog.Error("Unknown --mode (expected worker, gateway, or local)", slog.String("mode", *mode))
		os.Exit(1)
	}
}
