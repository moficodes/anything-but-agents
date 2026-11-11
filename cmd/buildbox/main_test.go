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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestActorReadyAndHealth(t *testing.T) {
	bb := NewBuildbox("test-job", "", "")
	mux := ActorMux(bb)

	for _, path := range []string{"/readyz", "/healthz"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("%s returned %d, want 200", path, rec.Code)
		}
		if body := rec.Body.String(); body != "ok\n" {
			t.Fatalf("%s returned body %q, want %q", path, body, "ok\n")
		}
	}
}

func TestActor3PhaseHermeticPipeline(t *testing.T) {
	ctx := context.Background()

	// 1. Setup mock upstream server hosting package mirror and registry
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/api/mirror/pkgs/"):
			pkg := strings.TrimPrefix(r.URL.Path, "/api/mirror/pkgs/")
			if pkg == "backdoor-payload" {
				http.Error(w, "Forbidden", http.StatusForbidden)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"package":"` + pkg + `","version":"v1.0.0","size_bytes":1048576,"checksum":"sha256:abc","license":"Apache-2.0"}`))
		case r.URL.Path == "/api/registry/releases":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"status":"PUBLISHED","release_uri":"us-docker.pkg.dev/test/rel@sha256:123"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer mockServer.Close()

	bb := NewBuildbox("build-pr-104", mockServer.URL, mockServer.URL)

	// Step 0: Initial state check
	initialState := bb.State("")
	if initialState.Phase != PhaseIdle {
		t.Fatalf("expected initial phase %q, got %q", PhaseIdle, initialState.Phase)
	}
	if len(initialState.CacheEntries) != 0 {
		t.Fatalf("expected empty initial cache, got %d", len(initialState.CacheEntries))
	}

	// Step 1: Phase 1 — Fetch Dependencies
	err := bb.FetchDependencies(ctx, mockServer.URL)
	if err != nil {
		t.Fatalf("FetchDependencies failed: %v", err)
	}
	phase1State := bb.State("")
	if phase1State.Phase != PhaseDependencyFetch {
		t.Fatalf("expected phase %q, got %q", PhaseDependencyFetch, phase1State.Phase)
	}
	if len(phase1State.CacheEntries) != 3 {
		t.Fatalf("expected 3 cached packages, got %d", len(phase1State.CacheEntries))
	}
	if phase1State.CacheSizeBytes <= 0 {
		t.Fatalf("expected positive cache size, got %d", phase1State.CacheSizeBytes)
	}

	// Step 2: Phase 2 — Hermetic Compile and Test
	err = bb.CompileAndTest(ctx)
	if err != nil {
		t.Fatalf("CompileAndTest failed: %v", err)
	}
	phase2State := bb.State("")
	if phase2State.Phase != PhaseHermeticCompile {
		t.Fatalf("expected phase %q, got %q", PhaseHermeticCompile, phase2State.Phase)
	}
	if phase2State.BuildArtifact == nil {
		t.Fatal("expected non-nil BuildArtifact")
	}
	if !strings.HasPrefix(phase2State.BuildArtifact.BinaryDigest, "sha256:") {
		t.Fatalf("unexpected binary digest format: %s", phase2State.BuildArtifact.BinaryDigest)
	}
	if len(phase2State.BuildArtifact.SBOM) != 3 {
		t.Fatalf("expected 3 SBOM components, got %d", len(phase2State.BuildArtifact.SBOM))
	}
	if phase2State.CacheHitRatePct != 100.0 {
		t.Fatalf("expected 100%% warm cache hit rate, got %f", phase2State.CacheHitRatePct)
	}

	// Step 2b: Test Rogue Egress Attempts
	rogueSummary, err := bb.TestRogueEgress(ctx)
	if err != nil {
		t.Fatalf("TestRogueEgress error: %v", err)
	}
	if len(rogueSummary.Probes) != 3 {
		t.Fatalf("expected 3 rogue probes, got %d", len(rogueSummary.Probes))
	}
	if !rogueSummary.AllBlocked {
		t.Fatalf("expected all rogue probes to be blocked, but verdict was: %s", rogueSummary.Verdict)
	}
	if !strings.Contains(rogueSummary.Verdict, "100% Hermetic Airgap Verified") {
		t.Fatalf("unexpected verdict text: %s", rogueSummary.Verdict)
	}

	// Step 3: Phase 3 — Publish Artifact
	art, err := bb.PublishArtifact(ctx, mockServer.URL)
	if err != nil {
		t.Fatalf("PublishArtifact failed: %v", err)
	}
	if art.ReleaseURI == "" {
		t.Fatal("expected non-empty ReleaseURI")
	}
	phase3State := bb.State("")
	if phase3State.Phase != PhaseCompleted {
		t.Fatalf("expected phase %q, got %q", PhaseCompleted, phase3State.Phase)
	}

	// Verify Audit Log captured entries
	if len(phase3State.AuditLog) < 5 {
		t.Fatalf("expected at least 5 audit log entries, got %d", len(phase3State.AuditLog))
	}

	// Reset
	bb.Reset(true)
	resetState := bb.State("")
	if resetState.Phase != PhaseIdle {
		t.Fatalf("expected phase %q after reset, got %q", PhaseIdle, resetState.Phase)
	}
	if len(resetState.CacheEntries) != 0 {
		t.Fatalf("expected empty cache after reset, got %d", len(resetState.CacheEntries))
	}
}

func TestLocalSimulatorBackendFullLifecycle(t *testing.T) {
	ctx := context.Background()
	backend := NewLocalSimulatorBackend("anything-but-agents", "buildbox")

	// 1. List jobs
	jobs, err := backend.ListJobs(ctx)
	if err != nil {
		t.Fatalf("ListJobs failed: %v", err)
	}
	if len(jobs) != 3 {
		t.Fatalf("expected 3 default jobs, got %d", len(jobs))
	}
	if jobs[0].JobName != "build-pr-104" || jobs[0].Status != "RUNNING" {
		t.Fatalf("unexpected first job: %+v", jobs[0])
	}
	if jobs[1].JobName != "build-release-v2" || jobs[1].Status != "SUSPENDED" {
		t.Fatalf("unexpected second job: %+v", jobs[1])
	}

	// 2. Fetch dependencies
	st1, err := backend.FetchDeps(ctx, "build-pr-104")
	if err != nil {
		t.Fatalf("FetchDeps failed: %v", err)
	}
	if st1.Phase != PhaseDependencyFetch || len(st1.CacheEntries) != 3 {
		t.Fatalf("unexpected state after fetch: %+v", st1)
	}

	// 3. Compile and test
	st2, err := backend.CompileAndTest(ctx, "build-pr-104")
	if err != nil {
		t.Fatalf("CompileAndTest failed: %v", err)
	}
	if st2.Phase != PhaseHermeticCompile || st2.BuildArtifact == nil {
		t.Fatalf("unexpected state after compile: %+v", st2)
	}

	// 4. Test rogue egress
	st3, err := backend.TestRogueEgress(ctx, "build-pr-104")
	if err != nil {
		t.Fatalf("TestRogueEgress failed: %v", err)
	}
	if st3.LastRogueTest == nil || !st3.LastRogueTest.AllBlocked {
		t.Fatalf("expected all rogue tests blocked, got: %+v", st3.LastRogueTest)
	}

	// 5. Publish artifact
	st4, err := backend.PublishArtifact(ctx, "build-pr-104")
	if err != nil {
		t.Fatalf("PublishArtifact failed: %v", err)
	}
	if st4.Phase != PhaseCompleted || st4.BuildArtifact.ReleaseURI == "" {
		t.Fatalf("unexpected state after publish: %+v", st4)
	}

	// 6. Suspend actor
	err = backend.SuspendActor(ctx, "build-pr-104")
	if err != nil {
		t.Fatalf("SuspendActor failed: %v", err)
	}
	stSuspended, _ := backend.GetJobState(ctx, "build-pr-104", false)
	if stSuspended.Status != "SUSPENDED" {
		t.Fatalf("expected SUSPENDED, got %s", stSuspended.Status)
	}

	// 7. Wake suspended actor -> worker pod migration
	stWoken, err := backend.GetJobState(ctx, "build-pr-104", true)
	if err != nil {
		t.Fatalf("GetJobState with wake failed: %v", err)
	}
	if stWoken.Status != "RUNNING" {
		t.Fatalf("expected RUNNING after wake, got %s", stWoken.Status)
	}
	if len(stWoken.PodIPHistory) < 2 {
		t.Fatalf("expected pod migration history >= 2, got %d", len(stWoken.PodIPHistory))
	}

	// 8. Egress policy YAML verification
	policyYAML, err := backend.GetEgressPolicyYAML(ctx, "build-pr-104")
	if err != nil {
		t.Fatalf("GetEgressPolicyYAML failed: %v", err)
	}
	if !strings.Contains(policyYAML, "anything-but-agents") {
		t.Fatalf("unexpected policy YAML: %s", policyYAML)
	}

	// 9. AutoRunPipeline on secondary job
	stAuto, err := backend.AutoRunPipeline(ctx, "build-release-v2")
	if err != nil {
		t.Fatalf("AutoRunPipeline failed: %v", err)
	}
	if stAuto.Status != "SUSPENDED" || stAuto.Phase != PhaseCompleted {
		t.Fatalf("unexpected autorun result: %+v", stAuto)
	}

	// 10. Reset stage
	stReset, err := backend.ResetStage(ctx)
	if err != nil {
		t.Fatalf("ResetStage failed: %v", err)
	}
	if stReset.JobName != "build-pr-104" || stReset.Phase != PhaseIdle {
		t.Fatalf("unexpected reset state: %+v", stReset)
	}
}

func TestGatewayServerAPIRoutesAndAuth(t *testing.T) {
	backend := NewLocalSimulatorBackend("anything-but-agents", "buildbox")
	const testToken = "test-presenter-key-12345"
	gw := NewGatewayServer(backend, testToken)
	mux := gw.Mux()

	// 1. Unauthenticated /api/state should be rejected
	reqUnauth := httptest.NewRequest(http.MethodGet, "/api/state?job=build-pr-104", nil)
	recUnauth := httptest.NewRecorder()
	mux.ServeHTTP(recUnauth, reqUnauth)
	if recUnauth.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 Unauthorized, got %d", recUnauth.Code)
	}

	// 2. Authenticated /api/state with X-Presenter-Key
	reqAuth := httptest.NewRequest(http.MethodGet, "/api/state?job=build-pr-104", nil)
	reqAuth.Header.Set(PresenterHeaderName, testToken)
	recAuth := httptest.NewRecorder()
	mux.ServeHTTP(recAuth, reqAuth)
	if recAuth.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", recAuth.Code)
	}
	var state BuildJobTracker
	if err := json.Unmarshal(recAuth.Body.Bytes(), &state); err != nil {
		t.Fatalf("failed to decode state JSON: %v", err)
	}
	if state.JobName != "build-pr-104" {
		t.Fatalf("expected job build-pr-104, got %s", state.JobName)
	}

	// 3. Authenticated /api/jobs
	reqJobs := httptest.NewRequest(http.MethodGet, "/api/jobs", nil)
	reqJobs.Header.Set(PresenterHeaderName, testToken)
	recJobs := httptest.NewRecorder()
	mux.ServeHTTP(recJobs, reqJobs)
	if recJobs.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", recJobs.Code)
	}

	// 4. Test Hosted Mock Package Mirror
	reqPkg := httptest.NewRequest(http.MethodGet, "/api/mirror/pkgs/core", nil)
	recPkg := httptest.NewRecorder()
	mux.ServeHTTP(recPkg, reqPkg)
	if recPkg.Code != http.StatusOK {
		t.Fatalf("mirror core pkg returned %d, want 200", recPkg.Code)
	}
	if !strings.Contains(recPkg.Body.String(), "pkg/core") {
		t.Fatalf("unexpected mirror response: %s", recPkg.Body.String())
	}

	// Rogue package attempt should be 403 Forbidden
	reqRoguePkg := httptest.NewRequest(http.MethodGet, "/api/mirror/pkgs/backdoor-payload", nil)
	recRoguePkg := httptest.NewRecorder()
	mux.ServeHTTP(recRoguePkg, reqRoguePkg)
	if recRoguePkg.Code != http.StatusForbidden {
		t.Fatalf("rogue mirror pkg returned %d, want 403", recRoguePkg.Code)
	}

	// 5. Test Hosted Mock Artifact Registry
	releasePayload := `{"job_name":"build-pr-104","binary_digest":"sha256:abc123","git_commit":"commit1","signature":"sig1","airgap_verified":true}`
	reqRelease := httptest.NewRequest(http.MethodPost, "/api/registry/releases", strings.NewReader(releasePayload))
	recRelease := httptest.NewRecorder()
	mux.ServeHTTP(recRelease, reqRelease)
	if recRelease.Code != http.StatusCreated {
		t.Fatalf("registry release returned %d, want 201", recRelease.Code)
	}
	if !strings.Contains(recRelease.Body.String(), "PUBLISHED") {
		t.Fatalf("unexpected release response: %s", recRelease.Body.String())
	}

	// 6. Test /api/build/fetch-deps, /api/build/compile-test, /api/build/test-rogue, /api/build/publish, /api/build/suspend
	actions := []string{"fetch-deps", "compile-test", "test-rogue", "publish", "suspend"}
	for _, action := range actions {
		reqAct := httptest.NewRequest(http.MethodPost, "/api/build/"+action, strings.NewReader(`{"job_name":"build-pr-104"}`))
		reqAct.Header.Set(PresenterHeaderName, testToken)
		reqAct.Header.Set("Content-Type", "application/json")
		recAct := httptest.NewRecorder()
		mux.ServeHTTP(recAct, reqAct)
		if recAct.Code != http.StatusOK {
			t.Fatalf("action %s failed with %d: %s", action, recAct.Code, recAct.Body.String())
		}
	}

	// Verify that polling /api/state with wake=0 preserves SUSPENDED state
	reqPoll := httptest.NewRequest(http.MethodGet, "/api/state?job=build-pr-104&wake=0", nil)
	reqPoll.Header.Set(PresenterHeaderName, testToken)
	recPoll := httptest.NewRecorder()
	mux.ServeHTTP(recPoll, reqPoll)
	if recPoll.Code != http.StatusOK {
		t.Fatalf("expected 200 on poll, got %d", recPoll.Code)
	}
	var polledState BuildJobTracker
	if err := json.Unmarshal(recPoll.Body.Bytes(), &polledState); err != nil {
		t.Fatalf("unmarshal polled state: %v", err)
	}
	if polledState.Status != "SUSPENDED" {
		t.Fatalf("expected status SUSPENDED after suspend action, got %s", polledState.Status)
	}

	// 7. Test /dashboard and / UI routes
	for _, uiPath := range []string{"/", "/dashboard"} {
		// a. Unauthenticated returns 401 login gate
		reqUnauth := httptest.NewRequest(http.MethodGet, uiPath, nil)
		recUnauth := httptest.NewRecorder()
		mux.ServeHTTP(recUnauth, reqUnauth)
		if recUnauth.Code != http.StatusUnauthorized {
			t.Fatalf("expected 401 on unauthenticated %s, got %d", uiPath, recUnauth.Code)
		}

		// b. Unlocking with ?key= redirects (303 See Other) and sets session cookies
		reqUnlock := httptest.NewRequest(http.MethodGet, uiPath+"?key="+testToken, nil)
		recUnlock := httptest.NewRecorder()
		mux.ServeHTTP(recUnlock, reqUnlock)
		if recUnlock.Code != http.StatusSeeOther {
			t.Fatalf("expected 303 redirect on %s?key=..., got %d", uiPath, recUnlock.Code)
		}
		cookies := recUnlock.Result().Cookies()
		if len(cookies) == 0 {
			t.Fatalf("expected session cookies set on unlock of %s", uiPath)
		}

		// c. Authenticated request with cookie serves 200 HTML
		reqAuth := httptest.NewRequest(http.MethodGet, uiPath, nil)
		for _, c := range cookies {
			reqAuth.AddCookie(c)
		}
		recAuth := httptest.NewRecorder()
		mux.ServeHTTP(recAuth, reqAuth)
		if recAuth.Code != http.StatusOK {
			t.Fatalf("UI route %s with cookie returned %d, want 200", uiPath, recAuth.Code)
		}
		body := recAuth.Body.String()
		if !strings.Contains(body, "The 3-Stage Hermetic CI/CD Build Pipeline") {
			t.Fatalf("UI route %s missing title in HTML", uiPath)
		}
		if !strings.Contains(body, "theme-toggle") {
			t.Fatalf("UI route %s missing theme-toggle in HTML", uiPath)
		}
		if !strings.Contains(body, "pipelineStepper") {
			t.Fatalf("UI route %s missing pipelineStepper in HTML", uiPath)
		}
		if !strings.Contains(body, "stepperProgressBar") {
			t.Fatalf("UI route %s missing stepperProgressBar in HTML", uiPath)
		}
	}
}

func TestEgressPolicyYAMLGeneration(t *testing.T) {
	p1 := generateEgressPolicyYAML("anything-but-agents", "build-pr-104", PhaseDependencyFetch)
	if !strings.Contains(p1, "buildbox-gateway.anything-but-agents.svc.cluster.local") {
		t.Fatalf("Phase 1 YAML missing mirror host: %s", p1)
	}

	p2 := generateEgressPolicyYAML("anything-but-agents", "build-pr-104", PhaseHermeticCompile)
	if !strings.Contains(p2, "rules: []") || !strings.Contains(p2, "Total Airgap") {
		t.Fatalf("Phase 2 YAML missing airgap 0-rules: %s", p2)
	}

	p3 := generateEgressPolicyYAML("anything-but-agents", "build-pr-104", PhaseArtifactPublish)
	if !strings.Contains(p3, "buildbox-gateway.anything-but-agents.svc.cluster.local") {
		t.Fatalf("Phase 3 YAML missing registry host: %s", p3)
	}
}

func TestActorMuxPostEndpoints(t *testing.T) {
	bb := NewBuildbox("build-pr-104", "", "")
	mux := ActorMux(bb)

	// Fetch dependencies with invalid/unreachable URL will record failure and return 502
	reqFetch := httptest.NewRequest(http.MethodPost, "/action/fetch-deps", strings.NewReader(`{"mirror_url":"http://127.0.0.1:54321"}`))
	recFetch := httptest.NewRecorder()
	mux.ServeHTTP(recFetch, reqFetch)
	if recFetch.Code != http.StatusBadGateway {
		t.Fatalf("expected 502 Bad Gateway for unreachable mirror, got %d", recFetch.Code)
	}

	// Compile with empty cache returns 400
	reqCompile := httptest.NewRequest(http.MethodPost, "/action/compile-test", strings.NewReader(`{}`))
	recCompile := httptest.NewRecorder()
	mux.ServeHTTP(recCompile, reqCompile)
	if recCompile.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request for compile with empty cache, got %d", recCompile.Code)
	}

	// Test rogue egress returns 200 with summary
	reqRogue := httptest.NewRequest(http.MethodPost, "/action/test-rogue-egress", strings.NewReader(`{}`))
	recRogue := httptest.NewRecorder()
	mux.ServeHTTP(recRogue, reqRogue)
	if recRogue.Code != http.StatusOK {
		t.Fatalf("expected 200 OK for rogue test, got %d", recRogue.Code)
	}
}

func TestDecodeOptionalJSON(t *testing.T) {
	reqEmpty := httptest.NewRequest(http.MethodPost, "/", nil)
	var dest struct {
		Foo string `json:"foo"`
	}
	if err := decodeOptionalJSON(reqEmpty, &dest); err != nil {
		t.Fatalf("unexpected error on empty body: %v", err)
	}

	reqWithBody := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader([]byte(`{"foo":"bar"}`)))
	if err := decodeOptionalJSON(reqWithBody, &dest); err != nil {
		t.Fatalf("unexpected error on valid body: %v", err)
	}
	if dest.Foo != "bar" {
		t.Fatalf("expected foo=bar, got %s", dest.Foo)
	}
}
