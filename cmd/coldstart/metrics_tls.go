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
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math"
	"math/big"
	"net/http"
	"strings"
	"time"
)

// ExternalMetricValue represents a single metric sample in external.metrics.k8s.io/v1beta1.
type ExternalMetricValue struct {
	MetricName   string            `json:"metricName"`
	MetricLabels map[string]string `json:"metricLabels"`
	Timestamp    string            `json:"timestamp"`
	Value        string            `json:"value"`
}

// ExternalMetricValueList is the response returned to kube-controller-manager's HPA controller.
type ExternalMetricValueList struct {
	Kind       string                `json:"kind"`
	APIVersion string                `json:"apiVersion"`
	Metadata   map[string]string     `json:"metadata"`
	Items      []ExternalMetricValue `json:"items"`
}

// NewExternalMetricsMux builds an http.ServeMux implementing the Kubernetes
// external.metrics.k8s.io/v1beta1 aggregated API for `coldstart_lb_qps`.
func (g *GatewayServer) NewExternalMetricsMux() *http.ServeMux {
	mux := http.NewServeMux()

	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("/livez", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("/apis", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"kind":       "APIGroupList",
			"apiVersion": "v1",
			"groups": []map[string]any{
				{
					"name": "external.metrics.k8s.io",
					"versions": []map[string]any{
						{
							"groupVersion": "external.metrics.k8s.io/v1beta1",
							"version":      "v1beta1",
						},
					},
					"preferredVersion": map[string]any{
						"groupVersion": "external.metrics.k8s.io/v1beta1",
						"version":      "v1beta1",
					},
				},
			},
		})
	})
	mux.HandleFunc("/apis/external.metrics.k8s.io", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"kind":       "APIGroup",
			"apiVersion": "v1",
			"name":       "external.metrics.k8s.io",
			"versions": []map[string]any{
				{
					"groupVersion": "external.metrics.k8s.io/v1beta1",
					"version":      "v1beta1",
				},
			},
			"preferredVersion": map[string]any{
				"groupVersion": "external.metrics.k8s.io/v1beta1",
				"version":      "v1beta1",
			},
		})
	})
	mux.HandleFunc("/apis/external.metrics.k8s.io/v1beta1", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"kind":         "APIResourceList",
			"apiVersion":   "v1",
			"groupVersion": "external.metrics.k8s.io/v1beta1",
			"resources": []map[string]any{
				{
					"name":         "coldstart_lb_qps",
					"singularName": "",
					"namespaced":   true,
					"kind":         "ExternalMetricValueList",
					"verbs":        []string{"get"},
				},
			},
		})
	})

	// Matches /apis/external.metrics.k8s.io/v1beta1/namespaces/<ns>/coldstart_lb_qps
	mux.HandleFunc("/apis/external.metrics.k8s.io/v1beta1/namespaces/", func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/coldstart_lb_qps") {
			http.NotFound(w, r)
			return
		}
		selector := r.URL.Query().Get("labelSelector")
		k8sQPS, subQPS := g.CurrentLBQPS()
		nowStr := time.Now().UTC().Format(time.RFC3339)

		var items []ExternalMetricValue
		wantK8s := selector == "" || strings.Contains(selector, "backend=k8s")
		wantSub := selector == "" || strings.Contains(selector, "backend=substrate")

		if wantK8s {
			items = append(items, ExternalMetricValue{
				MetricName:   "coldstart_lb_qps",
				MetricLabels: map[string]string{"backend": "k8s"},
				Timestamp:    nowStr,
				Value:        fmt.Sprintf("%d", int64(math.Round(k8sQPS))),
			})
		}
		if wantSub {
			items = append(items, ExternalMetricValue{
				MetricName:   "coldstart_lb_qps",
				MetricLabels: map[string]string{"backend": "substrate"},
				Timestamp:    nowStr,
				Value:        fmt.Sprintf("%d", int64(math.Round(subQPS))),
			})
		}

		resp := ExternalMetricValueList{
			Kind:       "ExternalMetricValueList",
			APIVersion: "external.metrics.k8s.io/v1beta1",
			Metadata:   map[string]string{},
			Items:      items,
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})

	return mux
}

// generateSelfSignedCert creates an in-memory self-signed TLS certificate for the
// Kubernetes aggregated API server (external.metrics.k8s.io/v1beta1).
func generateSelfSignedCert() (tls.Certificate, error) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return tls.Certificate{}, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, err
	}
	tmpl := x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   "coldstart-gateway.anything-but-agents.svc",
			Organization: []string{"anything-but-agents"},
		},
		DNSNames: []string{
			"coldstart-gateway",
			"coldstart-gateway.anything-but-agents",
			"coldstart-gateway.anything-but-agents.svc",
			"coldstart-gateway.anything-but-agents.svc.cluster.local",
			"localhost",
		},
		NotBefore:             time.Now().Add(-1 * time.Hour),
		NotAfter:              time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	derBytes, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &priv.PublicKey, priv)
	if err != nil {
		return tls.Certificate{}, err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: derBytes})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(priv)})
	return tls.X509KeyPair(certPEM, keyPEM)
}
