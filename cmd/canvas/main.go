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

// Command canvas is a collaborative architecture canvas & document room server
// demonstrating OpenFGA AccessPolicy fine-grained per-actor relationship authorization
// enforced before actor wakeup on Agent Substrate.
//
// Modes:
//   - actor: Runs a single in-memory document room (zero auth middleware, pure heap state).
//   - gateway: Connects to cluster Substrate components (atenet-router, kubectl-ate) and OpenFGA perimeter evaluator.
//   - local: Runs a zero-dependency local simulator mode for tests, offline demos, and verification.
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
	docID := flag.String("doc-id", DocMergerAcquisition, "Document ID for actor mode")
	atespace := flag.String("atespace", "anything-but-agents", "Agent Substrate atespace")
	template := flag.String("template", "canvas", "Agent Substrate ActorTemplate name")
	routerURL := flag.String("router-url", "http://localhost:8000", "atenet-router HTTP URL")
	ateEndpoint := flag.String("ate-endpoint", "", "Optional ate-api-server gRPC endpoint override for kubectl-ate")
	kubectlAteBin := flag.String("kubectl-ate-bin", "kubectl-ate", "Path to kubectl-ate binary")
	presenterToken := flag.String("presenter-token", os.Getenv("PRESENTER_TOKEN"), "Optional presenter token for /dashboard")
	flag.Parse()

	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	switch *mode {
	case "actor":
		actor := NewCanvasActor(*docID)
		slog.Info("Starting Canvas Document Actor server (zero-auth in-memory mode)",
			slog.String("addr", *addr),
			slog.String("doc_id", *docID),
			slog.String("classification", actor.classification))
		if err := http.ListenAndServe(*addr, newActorMux(actor)); err != nil {
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
		slog.Info("Starting Canvas Document Rooms Gateway (cluster-ate mode)",
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
		slog.Info("Starting Canvas Document Rooms Gateway (local simulator mode)",
			slog.String("addr", *addr),
			slog.String("atespace", *atespace))
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
