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

// Command tenantdb is a single-tenant embedded database server ("One DB
// Process Per Customer") backed by a Substrate DurableDir volume (/data) in
// --mode=actor, and a Multi-Tenant Storage & Memory Tiering Control Plane in
// --mode=gateway or --mode=local.
package main

import (
	"flag"
	"log/slog"
	"net/http"
	"os"

	"github.com/moficodes/anything-but-agents/internal/presenterauth"
)

func main() {
	addr := flag.String("addr", ":80", "HTTP listen address")
	mode := flag.String("mode", "actor", "Operating mode: actor | gateway | local")
	dataDir := flag.String("data-dir", "/data", "Path to DurableDir mount (actor mode)")
	bufferPoolMB := flag.Int("buffer-pool-mb", 16, "Hot RAM buffer pool size in MB per tenant (actor mode)")
	atespace := flag.String("atespace", "anything-but-agents", "Agent Substrate atespace")
	template := flag.String("template", "tenantdb", "Agent Substrate ActorTemplate name")
	routerURL := flag.String("router-url", "http://localhost:8000", "atenet-router HTTP URL")
	ateEndpoint := flag.String("ate-endpoint", "", "Optional ate-api-server gRPC endpoint override for kubectl-ate")
	kubectlAteBin := flag.String("kubectl-ate-bin", "kubectl-ate", "Path to kubectl-ate binary")
	presenterToken := flag.String("presenter-token", os.Getenv("PRESENTER_TOKEN"), "Optional presenter token for /dashboard")
	flag.Parse()

	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	switch *mode {
	case "actor":
		db, err := openTenantDBWithConfig(*dataDir, *bufferPoolMB*1024*1024)
		if err != nil {
			slog.Error("Failed to open tenant DB", slog.Any("err", err))
			os.Exit(1)
		}
		slog.Info("Starting Per-Tenant DB actor server",
			slog.String("addr", *addr),
			slog.String("data_dir", *dataDir),
			slog.Int("buffer_pool_mb", *bufferPoolMB))
		if err := http.ListenAndServe(*addr, newMux(db)); err != nil {
			slog.Error("Actor server exited", slog.Any("err", err))
			os.Exit(1)
		}

	case "gateway":
		token := *presenterToken
		if token == "" {
			token = presenterauth.RandomToken(24)
		}
		backend := NewClusterAteBackend(*atespace, *template, *routerURL, *ateEndpoint, *kubectlAteBin)
		gw := NewGatewayServer(backend, token)
		slog.Info("Starting TenantDB Storage & Memory Tiering Gateway (cluster-ate mode)",
			slog.String("addr", *addr),
			slog.String("atespace", *atespace),
			slog.String("router_url", *routerURL))
		if *presenterToken == "" {
			slog.Info("PRESENTER_TOKEN is unset; generated presenter key for /dashboard",
				slog.String("dashboard_url", "/dashboard?key="+gw.PresenterKey()))
		}
		if err := http.ListenAndServe(*addr, gw.Mux()); err != nil {
			slog.Error("Gateway server exited", slog.Any("err", err))
			os.Exit(1)
		}

	case "local":
		token := *presenterToken
		if token == "" {
			token = presenterauth.RandomToken(24)
		}
		backend := NewLocalSimulatorBackend(*atespace, *template)
		gw := NewGatewayServer(backend, token)
		slog.Info("Starting TenantDB Storage & Memory Tiering Gateway (local simulator mode)",
			slog.String("addr", *addr))
		if *presenterToken == "" {
			slog.Info("PRESENTER_TOKEN is unset; generated presenter key for /dashboard",
				slog.String("dashboard_url", "/dashboard?key="+gw.PresenterKey()))
		}
		if err := http.ListenAndServe(*addr, gw.Mux()); err != nil {
			slog.Error("Local gateway server exited", slog.Any("err", err))
			os.Exit(1)
		}

	default:
		slog.Error("Unknown --mode (expected actor, gateway, or local)", slog.String("mode", *mode))
		os.Exit(1)
	}
}
