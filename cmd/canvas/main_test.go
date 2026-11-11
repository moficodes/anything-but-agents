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
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestActorSceneGraphPersistence(t *testing.T) {
	actor := NewCanvasActor(DocMergerAcquisition)

	initialScene := actor.BuildSceneGraph()
	if initialScene.DocID != DocMergerAcquisition {
		t.Fatalf("expected docID %s, got %s", DocMergerAcquisition, initialScene.DocID)
	}
	if len(initialScene.Nodes) != 4 {
		t.Fatalf("expected 4 initial nodes in M&A catalog, got %d", len(initialScene.Nodes))
	}
	if initialScene.Revision != 1 {
		t.Fatalf("expected initial revision 1, got %d", initialScene.Revision)
	}

	// 1. Add Node
	newNode := CanvasNode{
		ID:       "node-audit",
		Type:     "service",
		Label:    "Security Audit Service",
		SubLabel: "Pre-Closing Verification",
		X:        500,
		Y:        250,
		Width:    220,
		Height:   80,
		Color:    "#10b981",
	}
	sceneAfterAdd, err := actor.Mutate(CanvasOp{
		Op:        "add_node",
		Principal: PrincipalAlice,
		Node:      &newNode,
	})
	if err != nil {
		t.Fatalf("failed to add node: %v", err)
	}
	if sceneAfterAdd.Revision != 2 {
		t.Fatalf("expected revision 2, got %d", sceneAfterAdd.Revision)
	}
	if len(sceneAfterAdd.Nodes) != 5 {
		t.Fatalf("expected 5 nodes after addition, got %d", len(sceneAfterAdd.Nodes))
	}

	// 2. Move Node
	sceneAfterMove, err := actor.Mutate(CanvasOp{
		Op:        "move_node",
		Principal: PrincipalAlice,
		Move: &MoveNodePayload{
			ID: "node-audit",
			X:  600,
			Y:  350,
		},
	})
	if err != nil {
		t.Fatalf("failed to move node: %v", err)
	}
	if sceneAfterMove.Revision != 3 {
		t.Fatalf("expected revision 3, got %d", sceneAfterMove.Revision)
	}

	foundMoved := false
	for _, n := range sceneAfterMove.Nodes {
		if n.ID == "node-audit" {
			foundMoved = true
			if n.X != 600 || n.Y != 350 {
				t.Fatalf("expected coordinates (600, 350), got (%d, %d)", n.X, n.Y)
			}
		}
	}
	if !foundMoved {
		t.Fatalf("node-audit not found after move")
	}

	// 3. Add Sticky Note
	newNote := CanvasNote{
		ID:     "note-audit-approved",
		Text:   "Security audit completed by external compliance team.",
		X:      500,
		Y:      450,
		Width:  210,
		Height: 100,
		Color:  "#bbf7d0",
	}
	sceneAfterNote, err := actor.Mutate(CanvasOp{
		Op:        "add_note",
		Principal: PrincipalAlice,
		Note:      &newNote,
	})
	if err != nil {
		t.Fatalf("failed to add sticky note: %v", err)
	}
	if len(sceneAfterNote.Notes) != 2 {
		t.Fatalf("expected 2 sticky notes, got %d", len(sceneAfterNote.Notes))
	}

	// 4. Connect Edge
	newEdge := CanvasEdge{
		ID:     "edge-audit-escrow",
		From:   "node-audit",
		To:     "node-escrow",
		Label:  "Compliance Signoff",
		Style:  "solid",
	}
	sceneAfterEdge, err := actor.Mutate(CanvasOp{
		Op:        "add_edge",
		Principal: PrincipalAlice,
		Edge:      &newEdge,
	})
	if err != nil {
		t.Fatalf("failed to add edge: %v", err)
	}
	if len(sceneAfterEdge.Edges) != 4 {
		t.Fatalf("expected 4 edges, got %d", len(sceneAfterEdge.Edges))
	}

	// 5. Delete Node (should cascade-remove edge-audit-escrow)
	sceneAfterDelete, err := actor.Mutate(CanvasOp{
		Op:        "delete",
		Principal: PrincipalAlice,
		Delete: &DeletePayload{
			TargetType: "node",
			ID:         "node-audit",
		},
	})
	if err != nil {
		t.Fatalf("failed to delete node: %v", err)
	}
	if len(sceneAfterDelete.Nodes) != 4 {
		t.Fatalf("expected 4 nodes after delete, got %d", len(sceneAfterDelete.Nodes))
	}
	for _, e := range sceneAfterDelete.Edges {
		if e.ID == "edge-audit-escrow" {
			t.Fatalf("cascading edge deletion failed: edge-audit-escrow still present")
		}
	}
}

func TestOpenFGAEvaluation(t *testing.T) {
	eng := NewOpenFGAEngine("anything-but-agents")

	// 1. Alice is owner of doc-merger-acquisition
	allowed, role, _ := eng.Check(PrincipalAlice, PermCanGet, DocMergerAcquisition)
	if !allowed || role != RoleOwner {
		t.Fatalf("expected Alice to have can_get as owner, got allowed=%v, role=%s", allowed, role)
	}
	allowed, _, _ = eng.Check(PrincipalAlice, PermCanUpdate, DocMergerAcquisition)
	if !allowed {
		t.Fatalf("expected Alice to have can_update")
	}
	allowed, _, _ = eng.Check(PrincipalAlice, PermCanManagePolicy, DocMergerAcquisition)
	if !allowed {
		t.Fatalf("expected Alice to have can_manage_policy")
	}

	// 2. Bob has no role on doc-merger-acquisition initially
	allowed, role, _ = eng.Check(PrincipalBob, PermCanGet, DocMergerAcquisition)
	if allowed {
		t.Fatalf("expected Bob to be denied can_get on doc-merger-acquisition initially")
	}
	if role != "" {
		t.Fatalf("expected Bob role to be empty, got %s", role)
	}

	// 3. Bob is editor on doc-q4-roadmap
	allowed, role, _ = eng.Check(PrincipalBob, PermCanGet, DocQ4Roadmap)
	if !allowed || role != RoleEditor {
		t.Fatalf("expected Bob to have can_get as editor on roadmap, got allowed=%v, role=%s", allowed, role)
	}
	allowed, _, _ = eng.Check(PrincipalBob, PermCanUpdate, DocQ4Roadmap)
	if !allowed {
		t.Fatalf("expected Bob to have can_update on roadmap")
	}
	allowed, _, _ = eng.Check(PrincipalBob, PermCanManagePolicy, DocQ4Roadmap)
	if allowed {
		t.Fatalf("editor Bob should NOT have can_manage_policy (owner required)")
	}

	// 4. Charlie is viewer on doc-incident-postmortem
	allowed, role, _ = eng.Check(PrincipalCharlie, PermCanGet, DocIncidentPostmortem)
	if !allowed || role != RoleViewer {
		t.Fatalf("expected Charlie to have can_get as viewer, got allowed=%v, role=%s", allowed, role)
	}
	allowed, _, _ = eng.Check(PrincipalCharlie, PermCanUpdate, DocIncidentPostmortem)
	if allowed {
		t.Fatalf("viewer Charlie should NOT have can_update (read-only)")
	}
}

func TestDeniedBeforeWakeup(t *testing.T) {
	backend := NewLocalSimulatorBackend("anything-but-agents", "canvas")
	ctx := context.Background()

	// doc-merger-acquisition starts in SUSPENDED state
	// 1. Charlie attempts to access doc-merger-acquisition
	resp, err := backend.GetCanvasState(ctx, PrincipalCharlie, DocMergerAcquisition)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !resp.IsDeniedBeforeWakeup {
		t.Fatalf("expected IsDeniedBeforeWakeup to be true")
	}
	if resp.ActorState != ActorStateSuspended {
		t.Fatalf("expected actor to remain SUSPENDED in GCS, got %s", resp.ActorState)
	}
	if resp.Scene != nil {
		t.Fatalf("expected scene to be nil when denied")
	}

	metrics := backend.GetMetrics()
	if metrics.BlockedWhileAsleep != 1 {
		t.Fatalf("expected BlockedWhileAsleep to be 1, got %d", metrics.BlockedWhileAsleep)
	}
	if metrics.TotalBlockedUnauthorized != 1 {
		t.Fatalf("expected TotalBlockedUnauthorized to be 1, got %d", metrics.TotalBlockedUnauthorized)
	}
	if metrics.ComputeSavedSeconds <= 0 {
		t.Fatalf("expected ComputeSavedSeconds > 0, got %f", metrics.ComputeSavedSeconds)
	}

	// 2. Bob also attempts to access doc-merger-acquisition (before sharing)
	resp2, err := backend.GetCanvasState(ctx, PrincipalBob, DocMergerAcquisition)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !resp2.IsDeniedBeforeWakeup {
		t.Fatalf("expected Bob to also be denied before wakeup")
	}
	if resp2.ActorState != ActorStateSuspended {
		t.Fatalf("expected actor to remain SUSPENDED, got %s", resp2.ActorState)
	}

	metrics = backend.GetMetrics()
	if metrics.BlockedWhileAsleep != 2 {
		t.Fatalf("expected BlockedWhileAsleep to be 2, got %d", metrics.BlockedWhileAsleep)
	}
}

func TestDynamicGrantAndRevoke(t *testing.T) {
	backend := NewLocalSimulatorBackend("anything-but-agents", "canvas")
	ctx := context.Background()

	// 1. Verify Bob cannot access doc-merger-acquisition
	resp, _ := backend.GetCanvasState(ctx, PrincipalBob, DocMergerAcquisition)
	if !resp.IsDeniedBeforeWakeup {
		t.Fatalf("Bob should not have access initially")
	}

	// 2. Charlie (non-owner) tries to grant Bob -> should fail
	_, err := backend.GrantRole(ctx, PrincipalCharlie, PrincipalBob, RoleEditor, DocMergerAcquisition)
	if err == nil {
		t.Fatalf("non-owner Charlie should not be able to grant roles")
	}

	// 3. Alice (owner) grants Bob 'editor'
	entry, err := backend.GrantRole(ctx, PrincipalAlice, PrincipalBob, RoleEditor, DocMergerAcquisition)
	if err != nil {
		t.Fatalf("owner Alice should be able to grant editor: %v", err)
	}
	if entry.Decision != "POLICY_UPDATE" {
		t.Fatalf("expected decision POLICY_UPDATE, got %s", entry.Decision)
	}

	// 4. Now Bob accesses doc-merger-acquisition -> should be ALLOWED and wake actor
	respAllowed, err := backend.GetCanvasState(ctx, PrincipalBob, DocMergerAcquisition)
	if err != nil {
		t.Fatalf("Bob should now be allowed: %v", err)
	}
	if respAllowed.IsDeniedBeforeWakeup {
		t.Fatalf("Bob should no longer be denied")
	}
	if respAllowed.ActorState != ActorStateRunning {
		t.Fatalf("expected actor to be woken to RUNNING, got %s", respAllowed.ActorState)
	}
	if !respAllowed.CanEdit {
		t.Fatalf("Bob as editor should have CanEdit=true")
	}

	// 5. Bob mutates canvas by adding a node
	mutateResp, err := backend.MutateCanvas(ctx, PrincipalBob, DocMergerAcquisition, CanvasOp{
		Op: "add_node",
		Node: &CanvasNode{
			ID:    "node-bob-arch",
			Type:  "service",
			Label: "Bob's Cache Layer",
			X:     300,
			Y:     300,
		},
	})
	if err != nil {
		t.Fatalf("Bob should be able to mutate canvas: %v", err)
	}
	if mutateResp.Scene.Revision < 2 {
		t.Fatalf("expected revision to increase after Bob's mutation")
	}

	// 6. Alice revokes Bob's role
	revokeEntry, err := backend.RevokeRole(ctx, PrincipalAlice, PrincipalBob, DocMergerAcquisition)
	if err != nil {
		t.Fatalf("Alice should be able to revoke Bob's role: %v", err)
	}
	if revokeEntry.Decision != "POLICY_UPDATE" {
		t.Fatalf("expected decision POLICY_UPDATE on revoke, got %s", revokeEntry.Decision)
	}

	// 7. Bob immediately blocked on next request
	respAfterRevoke, _ := backend.GetCanvasState(ctx, PrincipalBob, DocMergerAcquisition)
	if !respAfterRevoke.IsDeniedBeforeWakeup {
		t.Fatalf("Bob should be denied immediately after role revoke")
	}
}

func TestViewerReadOnlyEnforcement(t *testing.T) {
	backend := NewLocalSimulatorBackend("anything-but-agents", "canvas")
	ctx := context.Background()

	// Charlie is viewer on doc-incident-postmortem
	resp, err := backend.GetCanvasState(ctx, PrincipalCharlie, DocIncidentPostmortem)
	if err != nil {
		t.Fatalf("viewer Charlie should be able to read: %v", err)
	}
	if resp.IsDeniedBeforeWakeup {
		t.Fatalf("Charlie should not be denied read access")
	}
	if resp.CanEdit {
		t.Fatalf("viewer Charlie must have CanEdit=false")
	}

	// Charlie tries to add a node -> must be rejected with permission denied
	_, err = backend.MutateCanvas(ctx, PrincipalCharlie, DocIncidentPostmortem, CanvasOp{
		Op: "add_node",
		Node: &CanvasNode{
			ID:    "node-charlie-hack",
			Type:  "gateway",
			Label: "Unauthorized Gateway",
		},
	})
	if err == nil {
		t.Fatalf("expected mutation by viewer Charlie to fail")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "permission denied") {
		t.Fatalf("expected permission denied error, got %v", err)
	}
}

func TestGatewayHTTP(t *testing.T) {
	backend := NewLocalSimulatorBackend("anything-but-agents", "canvas")
	gw := NewGatewayServer(backend, "test-secret-presenter-token")
	srv := httptest.NewServer(gw.Mux())
	defer srv.Close()

	client := srv.Client()

	// 1. GET /readyz and /healthz
	for _, path := range []string{"/readyz", "/healthz"} {
		res, err := client.Get(srv.URL + path)
		if err != nil {
			t.Fatalf("failed GET %s: %v", path, err)
		}
		if res.StatusCode != http.StatusOK {
			t.Fatalf("expected 200 on %s, got %d", path, res.StatusCode)
		}
	}

	// 2. GET / (Dashboard UI)
	res, err := client.Get(srv.URL + "/")
	if err != nil {
		t.Fatalf("failed GET /: %v", err)
	}
	if res.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 on /, got %d", res.StatusCode)
	}
	body, _ := io.ReadAll(res.Body)
	if !strings.Contains(string(body), "Demo 8: Canvas Collaborative Document Rooms") {
		t.Fatalf("dashboard HTML missing expected title header")
	}
	if !strings.Contains(string(body), "Projector Light Mode") {
		t.Fatalf("dashboard HTML missing Projector Light Mode toggle")
	}

	// 3. GET /api/state for Alice (Allowed)
	resAlice, err := client.Get(srv.URL + "/api/state?principal=user:alice&doc=doc-merger-acquisition")
	if err != nil {
		t.Fatalf("failed GET /api/state: %v", err)
	}
	if resAlice.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for Alice, got %d", resAlice.StatusCode)
	}
	var aliceState CanvasStateResponse
	_ = json.NewDecoder(resAlice.Body).Decode(&aliceState)
	if aliceState.IsDeniedBeforeWakeup {
		t.Fatalf("Alice should not be denied")
	}

	// 4. GET /api/state for Charlie on doc-merger-acquisition (Denied before wakeup -> 403)
	resCharlie, err := client.Get(srv.URL + "/api/state?principal=user:charlie&doc=doc-merger-acquisition")
	if err != nil {
		t.Fatalf("failed GET /api/state for Charlie: %v", err)
	}
	if resCharlie.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 Forbidden for unauthorized Charlie, got %d", resCharlie.StatusCode)
	}
	var charlieState CanvasStateResponse
	_ = json.NewDecoder(resCharlie.Body).Decode(&charlieState)
	if !charlieState.IsDeniedBeforeWakeup {
		t.Fatalf("expected IsDeniedBeforeWakeup to be true for Charlie")
	}

	// 5. POST /api/policy/grant (Alice grants Charlie viewer)
	grantReq := map[string]string{
		"doc_id":      DocMergerAcquisition,
		"principal":   PrincipalAlice,
		"target_user": PrincipalCharlie,
		"role":        RoleViewer,
	}
	grantBody, _ := json.Marshal(grantReq)
	resGrant, err := client.Post(srv.URL+"/api/policy/grant", "application/json", bytes.NewReader(grantBody))
	if err != nil {
		t.Fatalf("failed POST /api/policy/grant: %v", err)
	}
	if resGrant.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 on grant, got %d", resGrant.StatusCode)
	}

	// 6. GET /api/policy/yaml
	resYAML, err := client.Get(srv.URL + "/api/policy/yaml?doc=doc-merger-acquisition")
	if err != nil {
		t.Fatalf("failed GET /api/policy/yaml: %v", err)
	}
	if resYAML.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 on /api/policy/yaml, got %d", resYAML.StatusCode)
	}
	yamlBytes, _ := io.ReadAll(resYAML.Body)
	yamlStr := string(yamlBytes)
	if !strings.Contains(yamlStr, "user:alice") || !strings.Contains(yamlStr, "user:charlie") {
		t.Fatalf("expected YAML to contain Alice and Charlie, got:\n%s", yamlStr)
	}

	// 7. POST /api/reset
	resReset, err := client.Post(srv.URL+"/api/reset", "application/json", nil)
	if err != nil {
		t.Fatalf("failed POST /api/reset: %v", err)
	}
	if resReset.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 on /api/reset, got %d", resReset.StatusCode)
	}
}
