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

// Command buildbox provides both the stateful gVisor hermetic CI/CD actor
// (--mode=actor) and the 3-Stage Hermetic Pipeline & EgressPolicy Gateway
// (--mode=gateway or --mode=local).
package main

import (
	"flag"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"

	"github.com/moficodes/anything-but-agents/internal/presenterauth"
)

func main() {
	addr := flag.String("addr", ":80", "HTTP listen address")
	mode := flag.String("mode", "actor", "Operating mode: actor | gateway | local")
	atespace := flag.String("atespace", "anything-but-agents", "Agent Substrate atespace")
	template := flag.String("template", "buildbox", "Agent Substrate ActorTemplate name")
	routerURL := flag.String("router-url", "http://localhost:8000", "atenet-router HTTP URL")
	ateEndpoint := flag.String("ate-endpoint", "", "Optional ate-api-server gRPC endpoint override for kubectl-ate")
	kubectlAteBin := flag.String("kubectl-ate-bin", "kubectl-ate", "Path to kubectl-ate binary")
	presenterToken := flag.String("presenter-token", os.Getenv("PRESENTER_TOKEN"), "Presenter token for UI & APIs (defaults to PRESENTER_TOKEN env var)")
	jobName := flag.String("job-name", os.Getenv("BUILD_JOB_NAME"), "Build job name for actor mode (e.g. build-pr-104)")
	mirrorURL := flag.String("mirror-url", os.Getenv("BUILDBOX_MIRROR_URL"), "Package mirror upstream URL")
	registryURL := flag.String("registry-url", os.Getenv("BUILDBOX_REGISTRY_URL"), "Artifact registry upstream URL")
	flag.Parse()

	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	switch *mode {
	case "actor":
		bb := NewBuildbox(*jobName, *mirrorURL, *registryURL)
		slog.Info("Starting Buildbox Hermetic CI/CD actor server",
			slog.String("addr", *addr),
			slog.String("job_name", bb.jobName),
			slog.String("mirror_url", bb.mirrorURL),
			slog.String("registry_url", bb.registryURL))
		if err := http.ListenAndServe(*addr, ActorMux(bb)); err != nil {
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
		slog.Info("Starting Buildbox Hermetic CI/CD Gateway (cluster-ate mode)",
			slog.String("addr", *addr),
			slog.String("atespace", *atespace),
			slog.String("router_url", *routerURL))
		if *presenterToken == "" {
			slog.Info("PRESENTER_TOKEN is unset; generated presenter key for Buildbox UI",
				slog.String("url", "/?key="+gw.PresenterKey()))
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

		// Set self as upstream package mirror and artifact registry in local mode
		hostPort := *addr
		if strings.HasPrefix(hostPort, ":") {
			hostPort = "localhost" + hostPort
		}
		localUpstream := "http://" + hostPort
		backend.SetUpstreamURLs(localUpstream, localUpstream)

		gw := NewGatewayServer(backend, token)
		slog.Info("Starting Buildbox Hermetic CI/CD Gateway (local simulator mode)",
			slog.String("addr", *addr),
			slog.String("upstream", localUpstream))
		if *presenterToken == "" {
			slog.Info("PRESENTER_TOKEN is unset; generated presenter key for Buildbox UI",
				slog.String("url", "/?key="+gw.PresenterKey()))
		}
		if err := http.ListenAndServe(*addr, gw.Mux()); err != nil {
			slog.Error("Local gateway server exited", slog.Any("err", err))
			os.Exit(1)
		}

	default:
		slog.Error("Unknown mode", slog.String("mode", *mode))
		os.Exit(1)
	}
}

func getOutboundIP() string {
	conn, err := net.Dial("udp", "8.8.8.8:80")
	if err != nil {
		return "127.0.0.1"
	}
	defer conn.Close()
	localAddr := conn.LocalAddr().(*net.UDPAddr)
	return localAddr.IP.String()
}
