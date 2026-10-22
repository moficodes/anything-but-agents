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

// Command dungeon2d provides both the per-player Agent Substrate actor server
// (--mode=actor) and the 2D browser game + Live Mission-Control Dashboard
// gateway (--mode=gateway or --mode=local).
package main

import (
	"flag"
	"log/slog"
	"net/http"
	"os"
)

func main() {
	addr := flag.String("addr", ":80", "HTTP listen address")
	mode := flag.String("mode", "actor", "Operating mode: actor | gateway | local")
	atespace := flag.String("atespace", "anything-but-agents", "Agent Substrate atespace")
	template := flag.String("template", "dungeon2d", "Agent Substrate ActorTemplate name")
	routerURL := flag.String("router-url", "http://localhost:8000", "atenet-router HTTP URL")
	ateEndpoint := flag.String("ate-endpoint", "", "Optional ate-api-server gRPC endpoint override for kubectl-ate")
	kubectlAteBin := flag.String("kubectl-ate-bin", "kubectl-ate", "Path to kubectl-ate binary")
	publicURL := flag.String("public-url", "", "Optional public base URL for QR code generation")
	presenterToken := flag.String("presenter-token", os.Getenv("PRESENTER_TOKEN"), "Presenter token for /dashboard (defaults to PRESENTER_TOKEN env var or random key)")
	flag.Parse()

	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	switch *mode {
	case "actor":
		session := NewActorSession()
		slog.Info("Starting dungeon2d per-player actor server", slog.String("addr", *addr))
		if err := http.ListenAndServe(*addr, NewActorMux(session)); err != nil {
			slog.Error("Actor server exited", slog.Any("err", err))
			os.Exit(1)
		}

	case "gateway":
		backend := NewClusterAteBackend(*atespace, *template, *routerURL, *ateEndpoint, *kubectlAteBin)
		go backend.StartLocalRouterProxy("127.0.0.1:8000")
		gw := NewGatewayServer(backend, *publicURL, *presenterToken)
		slog.Info("Starting dungeon2d Gateway & Live Dashboard (cluster-ate mode)",
			slog.String("addr", *addr),
			slog.String("atespace", *atespace),
			slog.String("template", *template),
			slog.String("router_url", *routerURL))
		if *presenterToken == "" {
			slog.Info("PRESENTER_TOKEN is unset; generated presenter key for /dashboard",
				slog.String("dashboard_url", "/dashboard?key="+gw.PresenterKey()))
		} else {
			slog.Info("Presenter key loaded from PRESENTER_TOKEN; open /dashboard?key=<PRESENTER_TOKEN> to unlock the dashboard")
		}
		if err := http.ListenAndServe(*addr, gw.Mux()); err != nil {
			slog.Error("Gateway server exited", slog.Any("err", err))
			os.Exit(1)
		}

	case "local":
		backend := NewLocalSimulatorBackend(*atespace, *template)
		gw := NewGatewayServer(backend, *publicURL, *presenterToken)
		slog.Info("Starting dungeon2d Gateway & Live Dashboard (local simulator mode)",
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
