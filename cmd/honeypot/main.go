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

// Command honeypot provides both the per-attacker gVisor deception sandbox
// (--mode=actor) and the Split-Screen "Truman Show" SOC Director's Booth UI
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
	scratchDir := flag.String("scratch-dir", "/tmp/honeypot", "Rootfs directory for attacker-dropped files (actor mode)")
	atespace := flag.String("atespace", "anything-but-agents", "Agent Substrate atespace")
	template := flag.String("template", "honeypot", "Agent Substrate ActorTemplate name")
	routerURL := flag.String("router-url", "http://localhost:8000", "atenet-router HTTP URL")
	ateEndpoint := flag.String("ate-endpoint", "", "Optional ate-api-server gRPC endpoint override for kubectl-ate")
	kubectlAteBin := flag.String("kubectl-ate-bin", "kubectl-ate", "Path to kubectl-ate binary")
	flag.Parse()

	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	switch *mode {
	case "actor":
		hp, err := newHoneypot(*scratchDir)
		if err != nil {
			slog.Error("Failed to initialize honeypot", slog.Any("err", err))
			os.Exit(1)
		}
		slog.Info("Starting Truman Show Honeypot actor server", slog.String("addr", *addr), slog.String("scratch_dir", *scratchDir))
		if err := http.ListenAndServe(*addr, newMux(hp)); err != nil {
			slog.Error("Actor server exited", slog.Any("err", err))
			os.Exit(1)
		}

	case "gateway":
		backend := NewClusterAteBackend(*atespace, *template, *routerURL, *ateEndpoint, *kubectlAteBin)
		gw := NewGatewayServer(backend)
		slog.Info("Starting Honeypot Split-Screen SOC Gateway (cluster-ate mode)",
			slog.String("addr", *addr),
			slog.String("atespace", *atespace),
			slog.String("router_url", *routerURL))
		if err := http.ListenAndServe(*addr, gw.Mux()); err != nil {
			slog.Error("Gateway server exited", slog.Any("err", err))
			os.Exit(1)
		}

	case "local":
		backend := NewLocalSimulatorBackend(*atespace, *template)
		gw := NewGatewayServer(backend)
		slog.Info("Starting Honeypot Split-Screen SOC Gateway (local simulator mode)", slog.String("addr", *addr))
		if err := http.ListenAndServe(*addr, gw.Mux()); err != nil {
			slog.Error("Local gateway server exited", slog.Any("err", err))
			os.Exit(1)
		}

	default:
		slog.Error("Unknown --mode (expected actor, gateway, or local)", slog.String("mode", *mode))
		os.Exit(1)
	}
}
