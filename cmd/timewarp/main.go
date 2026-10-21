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

// Command timewarp is an intentional ANTI-PATTERN demo ("Don't Try This at
// Home") for the "Running Anything But Agents on Agent Substrate" talk.
//
// It supports three operating modes (--mode=actor|gateway|local):
//   - actor: runs inside the timewarp ActorTemplate (--addr=:80), tracking Raft
//     role/term, a 3s leader lease, a distributed lock (owner + fencing token),
//     a 100ms heartbeat delta loop, a naive rate-limiter token bucket, and an
//     optional Wakeup Clock-Jump Guardrail.
//   - gateway: coordinates 3 real Agent Substrate actors (raft-node-1..3) via
//     kubectl-ate and atenet-router, and serves the interactive Chronos
//     Split-Brain & Dual-Clock Lab Web UI (/ and /dashboard).
//   - local: runs the Gateway and Web UI backed by an in-memory 3-node cluster
//     simulator for local development and testing without a cluster.
package main

import (
	"flag"
	"log/slog"
	"net/http"
	"os"
	"time"
)

func main() {
	addr := flag.String("addr", ":80", "HTTP listen address")
	mode := flag.String("mode", "actor", "Operating mode: actor | gateway | local")
	atespace := flag.String("atespace", "anything-but-agents", "Agent Substrate atespace")
	template := flag.String("template", "timewarp", "Agent Substrate ActorTemplate name")
	routerURL := flag.String("router-url", "http://localhost:8000", "atenet-router HTTP URL")
	ateEndpoint := flag.String("ate-endpoint", "", "Optional ate-api-server gRPC endpoint override for kubectl-ate")
	kubectlAteBin := flag.String("kubectl-ate-bin", "kubectl-ate", "Path to kubectl-ate binary")
	flag.Parse()

	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	switch *mode {
	case "actor":
		// Pass zero time so the leader lease clock starts on the actor's first HTTP
		// request rather than during ActorTemplate Golden Snapshot creation.
		node := newNaiveClusterNode(time.Time{})

		go func() {
			ticker := time.NewTicker(100 * time.Millisecond)
			defer ticker.Stop()
			for now := range ticker.C {
				node.ObserveClock(now, "")
			}
		}()

		slog.Info("Starting Time-Warp Anti-Pattern actor server", slog.String("addr", *addr))
		if err := http.ListenAndServe(*addr, newMux(node)); err != nil {
			slog.Error("Actor server exited", slog.Any("err", err))
			os.Exit(1)
		}

	case "gateway":
		backend := NewClusterAteBackend(*atespace, *template, *routerURL, *ateEndpoint, *kubectlAteBin)
		gw := NewGatewayServer(backend)
		slog.Info("Starting Time-Warp Split-Brain & Dual-Clock Lab Gateway (cluster-ate mode)",
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
		slog.Info("Starting Time-Warp Split-Brain & Dual-Clock Lab Gateway (local simulator mode)", slog.String("addr", *addr))
		if err := http.ListenAndServe(*addr, gw.Mux()); err != nil {
			slog.Error("Local gateway server exited", slog.Any("err", err))
			os.Exit(1)
		}

	default:
		slog.Error("Unknown --mode (expected actor, gateway, or local)", slog.String("mode", *mode))
		os.Exit(1)
	}
}
