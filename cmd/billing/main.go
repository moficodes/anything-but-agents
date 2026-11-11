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

// Command billing provides both the 30-day sleeping subscription billing actor
// (--mode=actor) and the Payment Vault & Egress Credential Inspector Gateway
// (--mode=gateway or --mode=local).
package main

import (
	"flag"
	"log/slog"
	"net/http"
	"os"

	"github.com/moficodes/anything-but-agents/internal/presenterauth"
)

func main() {
	mode := flag.String("mode", "actor", "Operating mode: actor | gateway | local")
	addr := flag.String("addr", "", "HTTP listen address (defaults to :80 for actor, :8088 for gateway/local)")
	actorName := flag.String("actor-name", "sub-acme", "Subscription actor name (actor mode)")
	customerName := flag.String("customer-name", "Acme Corp", "Customer display name (actor mode)")
	planTier := flag.String("plan-tier", "Enterprise Tier ($5,000/mo)", "Subscription plan tier (actor mode)")
	monthlyFee := flag.Float64("monthly-fee", 5000.00, "Monthly subscription fee in USD (actor mode)")
	escrowAcct := flag.String("escrow-account", "escrow-acme-corp", "Escrow account identifier (actor mode)")
	vaultURL := flag.String("vault-url", "", "Upstream payment vault URL")
	echoURL := flag.String("echo-url", "", "Untrusted echo service URL for exfiltration testing")
	vaultToken := flag.String("vault-token", "", "Payment vault token or ate-secret:// URI placeholder")
	trustBundle := flag.String("trust-bundle", "", "Path to CA trust bundle for TLS egress termination")
	atespace := flag.String("atespace", "anything-but-agents", "Agent Substrate atespace")
	template := flag.String("template", "billing", "Agent Substrate ActorTemplate name")
	routerURL := flag.String("router-url", "https://atenet-router.ate-system.svc:443", "atenet-router URL for actor wakeup")
	ateEndpoint := flag.String("ate-endpoint", "", "Optional ate-api-server gRPC endpoint override")
	kubectlAteBin := flag.String("kubectl-ate-bin", "kubectl-ate", "Path to kubectl-ate binary")
	presenterToken := flag.String("presenter-token", os.Getenv("PRESENTER_TOKEN"), "Presenter token for UI & APIs")
	flag.Parse()

	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	listenAddr := *addr
	if listenAddr == "" {
		if *mode == "actor" {
			listenAddr = ":80"
		} else {
			listenAddr = ":8088"
		}
	}

	switch *mode {
	case "actor":
		actor := NewSubscriptionActor(
			*actorName,
			*customerName,
			*planTier,
			*monthlyFee,
			*escrowAcct,
			*vaultURL,
			*echoURL,
			*vaultToken,
			*trustBundle,
		)

		mux := http.NewServeMux()
		mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ok\n"))
		})
		mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ok\n"))
		})
		mux.HandleFunc("/state", func(w http.ResponseWriter, _ *http.Request) {
			writePrettyJSON(w, http.StatusOK, actor.GetState())
		})
		mux.HandleFunc("/settle", func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			st, rcpt, err := actor.SettleCycle(r.Context(), 0)
			if err != nil {
				http.Error(w, "settlement failed: "+err.Error(), http.StatusInternalServerError)
				return
			}
			writePrettyJSON(w, http.StatusOK, map[string]any{
				"state":   st,
				"receipt": rcpt,
			})
		})
		mux.HandleFunc("/inspect-memory", func(w http.ResponseWriter, _ *http.Request) {
			writePrettyJSON(w, http.StatusOK, actor.InspectMemory())
		})
		mux.HandleFunc("/test-exfiltrate", func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			var req struct {
				TargetURL string `json:"target_url"`
			}
			_ = decodeOptionalJSON(r, &req)
			res, err := actor.TestExfiltration(r.Context(), req.TargetURL)
			if err != nil {
				http.Error(w, "exfiltration test failed: "+err.Error(), http.StatusInternalServerError)
				return
			}
			writePrettyJSON(w, http.StatusOK, res)
		})

		slog.Info("Starting 30-Day Sleeping Subscription Billing Actor",
			slog.String("addr", listenAddr),
			slog.String("actor", *actorName),
			slog.Float64("monthly_fee", *monthlyFee))

		if err := http.ListenAndServe(listenAddr, mux); err != nil {
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
		slog.Info("Starting Sleeping Billing Engine Gateway (cluster-ate mode)",
			slog.String("addr", listenAddr),
			slog.String("atespace", *atespace),
			slog.String("router_url", *routerURL))
		if *presenterToken == "" {
			slog.Info("PRESENTER_TOKEN is unset; generated presenter key for Billing UI",
				slog.String("url", "/?key="+gw.PresenterKey()))
		}
		if err := http.ListenAndServe(listenAddr, gw.Mux()); err != nil {
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
		slog.Info("Starting Sleeping Billing Engine Gateway (local simulator mode)", slog.String("addr", listenAddr))
		if *presenterToken == "" {
			slog.Info("PRESENTER_TOKEN is unset; generated presenter key for Billing UI",
				slog.String("url", "/?key="+gw.PresenterKey()))
		}
		if err := http.ListenAndServe(listenAddr, gw.Mux()); err != nil {
			slog.Error("Local gateway server exited", slog.Any("err", err))
			os.Exit(1)
		}

	default:
		slog.Error("Unknown mode", slog.String("mode", *mode))
		os.Exit(1)
	}
}
