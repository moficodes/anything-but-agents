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

func TestAllSlideDefsCompletenessAndStructure(t *testing.T) {
	slides := AllSlideDefs()
	if len(slides) != 22 {
		t.Fatalf("expected 22 slides, got %d", len(slides))
	}

	validLayouts := map[string]bool{
		"asd-title":    true,
		"asd-demo":     true,
		"asd-cards":    true,
		"asd-two-col":  true,
		"asd-big-stat": true,
		"asd-code":     true,
		"asd-puzzle":   true,
		"asd-closing":  true,
	}

	seenIDs := make(map[string]bool)
	for i, s := range slides {
		if s.ID == "" {
			t.Errorf("slide at index %d has empty ID", i)
		}
		if seenIDs[s.ID] {
			t.Errorf("duplicate slide ID: %s", s.ID)
		}
		seenIDs[s.ID] = true

		if s.Index != i+1 {
			t.Errorf("slide %s has index %d, expected %d", s.ID, s.Index, i+1)
		}
		if s.Total != 22 {
			t.Errorf("slide %s has total %d, expected 22", s.ID, s.Total)
		}
		if !validLayouts[s.Layout] {
			t.Errorf("slide %s has unexpected layout %q", s.ID, s.Layout)
		}
		if s.Kicker == "" {
			t.Errorf("slide %s has empty kicker", s.ID)
		}
		if s.Title == "" {
			t.Errorf("slide %s has empty title", s.ID)
		}
		if s.Chapter == "" {
			t.Errorf("slide %s has empty chapter", s.ID)
		}
		if len(s.Points) == 0 {
			t.Errorf("slide %s has no points", s.ID)
		}
		if len(s.SubstrateFeatures) == 0 {
			t.Errorf("slide %s has no substrate features", s.ID)
		}
		if s.SpeakerNotes == "" {
			t.Errorf("slide %s has empty speaker notes", s.ID)
		}
		switch s.Layout {
		case "asd-cards":
			if len(s.Cards) != 3 {
				t.Errorf("slide %s (asd-cards) expected 3 cards, got %d", s.ID, len(s.Cards))
			}
		case "asd-two-col":
			if len(s.Columns) != 2 {
				t.Errorf("slide %s (asd-two-col) expected 2 columns, got %d", s.ID, len(s.Columns))
			}
		case "asd-big-stat":
			if s.StatValue == "" {
				t.Errorf("slide %s (asd-big-stat) expected StatValue", s.ID)
			}
		case "asd-code":
			if len(s.CodeLines) == 0 {
				t.Errorf("slide %s (asd-code) expected CodeLines", s.ID)
			}
		case "asd-puzzle":
			if s.PuzzleStage != "loot" && s.PuzzleStage != "epiphany" && s.PuzzleStage != "complete" {
				t.Errorf("slide %s (asd-puzzle) has unexpected puzzle_stage %q", s.ID, s.PuzzleStage)
			}
			if len(s.PuzzlePieces) != 9 {
				t.Fatalf("slide %s (asd-puzzle) expected 9 puzzle pieces, got %d", s.ID, len(s.PuzzlePieces))
			}
			filledCount := 0
			for idx, p := range s.PuzzlePieces {
				if p.Slot != idx {
					t.Errorf("slide %s piece %d has slot %d", s.ID, idx, p.Slot)
				}
				if p.Filled {
					filledCount++
				} else if p.EmptyLabel == "" {
					t.Errorf("slide %s unfilled piece at slot %d missing empty_label", s.ID, p.Slot)
				}
			}
			if s.PuzzlePieces[4].Kind != "keystone" {
				t.Errorf("slide %s expected keystone at center slot 4, got %q", s.ID, s.PuzzlePieces[4].Kind)
			}
			expectedFilled := 8
			if s.PuzzleStage == "complete" {
				expectedFilled = 9
			}
			if filledCount != expectedFilled {
				t.Errorf("slide %s (%s) expected %d filled puzzle pieces, got %d", s.ID, s.PuzzleStage, expectedFilled, filledCount)
			}
		case "asd-closing":
			if len(s.AgendaItems) == 0 {
				t.Errorf("slide %s (asd-closing) expected AgendaItems", s.ID)
			}
		}
	}
}

func TestSlideActorHeapStateAndMux(t *testing.T) {
	actor := NewSlideActor("slide-01")
	mux := NewActorMux(actor)

	// Test /readyz
	rReq := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rRec := httptest.NewRecorder()
	mux.ServeHTTP(rRec, rReq)
	if rRec.Code != http.StatusOK {
		t.Fatalf("expected 200 for /readyz, got %d", rRec.Code)
	}
	if !strings.Contains(rRec.Body.String(), "ok") {
		t.Fatalf("expected 'ok' in /readyz, got %q", rRec.Body.String())
	}

	// Test /healthz
	hReq := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	hRec := httptest.NewRecorder()
	mux.ServeHTTP(hRec, hReq)
	if hRec.Code != http.StatusOK {
		t.Fatalf("expected 200 for /healthz, got %d", hRec.Code)
	}

	// Test GET /api/slide: view 1
	sReq1 := httptest.NewRequest(http.MethodGet, "/api/slide", nil)
	sRec1 := httptest.NewRecorder()
	mux.ServeHTTP(sRec1, sReq1)
	if sRec1.Code != http.StatusOK {
		t.Fatalf("expected 200 for /api/slide, got %d", sRec1.Code)
	}
	var data1 SlideData
	if err := json.Unmarshal(sRec1.Body.Bytes(), &data1); err != nil {
		t.Fatalf("failed to parse slide json: %v", err)
	}
	if data1.ViewCount != 1 {
		t.Errorf("expected view count 1, got %d", data1.ViewCount)
	}
	if len(data1.PodIPHistory) == 0 {
		t.Errorf("expected non-empty pod ip history")
	}

	// Test GET /api/slide: view 2
	sReq2 := httptest.NewRequest(http.MethodGet, "/api/slide", nil)
	sRec2 := httptest.NewRecorder()
	mux.ServeHTTP(sRec2, sReq2)
	var data2 SlideData
	_ = json.Unmarshal(sRec2.Body.Bytes(), &data2)
	if data2.ViewCount != 2 {
		t.Errorf("expected view count 2, got %d", data2.ViewCount)
	}

	// Test POST /api/slide/note
	notePayload := `{"notes": "live presenter scratchpad note in heap RAM"}`
	nReq := httptest.NewRequest(http.MethodPost, "/api/slide/note", bytes.NewBufferString(notePayload))
	nRec := httptest.NewRecorder()
	mux.ServeHTTP(nRec, nReq)
	if nRec.Code != http.StatusOK {
		t.Fatalf("expected 200 for note update, got %d", nRec.Code)
	}
	var dataWithNote SlideData
	_ = json.Unmarshal(nRec.Body.Bytes(), &dataWithNote)
	if dataWithNote.Notes != "live presenter scratchpad note in heap RAM" {
		t.Errorf("expected updated note, got %q", dataWithNote.Notes)
	}

	// Test ResetToInitial
	resetData := actor.ResetToInitial()
	if resetData.Notes != "" {
		t.Errorf("expected empty notes after reset, got %q", resetData.Notes)
	}

	// Test Golden Snapshot binding via ate-target-actor / ?id= query param
	bReq := httptest.NewRequest(http.MethodGet, "/api/slide?id=slide-07", nil)
	bReq.Header.Set("ate-target-actor", "anything-but-agents/slide-07")
	bRec := httptest.NewRecorder()
	mux.ServeHTTP(bRec, bReq)
	if bRec.Code != http.StatusOK {
		t.Fatalf("expected 200 for bound /api/slide?id=slide-07, got %d", bRec.Code)
	}
	var bound SlideData
	if err := json.Unmarshal(bRec.Body.Bytes(), &bound); err != nil {
		t.Fatalf("failed to parse bound slide json: %v", err)
	}
	if bound.ID != "slide-07" || bound.Index != 7 {
		t.Errorf("expected bound slide-07 (index 7), got %s (index %d)", bound.ID, bound.Index)
	}
}

func TestLocalSimulatorBackendLifecycleAndRevert(t *testing.T) {
	ctx := context.Background()
	backend := NewLocalSimulatorBackend()

	// ListSlides
	summaries, err := backend.ListSlides(ctx)
	if err != nil {
		t.Fatalf("ListSlides failed: %v", err)
	}
	if len(summaries) != 22 {
		t.Fatalf("expected 22 summaries, got %d", len(summaries))
	}

	// GetSlide
	resp, err := backend.GetSlide(ctx, "slide-02", "slide-01")
	if err != nil {
		t.Fatalf("GetSlide failed: %v", err)
	}
	if resp.Slide.ID != "slide-02" {
		t.Errorf("expected slide-02, got %s", resp.Slide.ID)
	}
	if resp.Source != "simulated" {
		t.Errorf("expected simulated source, got %s", resp.Source)
	}
	if resp.WakeLatencyMs <= 0 {
		t.Errorf("expected positive wake latency, got %d", resp.WakeLatencyMs)
	}
	if len(resp.PodIPHistory) == 0 {
		t.Errorf("expected non-empty pod ip history")
	}

	// UpdateSlideNote
	updated, err := backend.UpdateSlideNote(ctx, "slide-02", "dungeon 2d notes")
	if err != nil {
		t.Fatalf("UpdateSlideNote failed: %v", err)
	}
	if updated.Slide.Notes != "dungeon 2d notes" {
		t.Errorf("expected updated note, got %q", updated.Slide.Notes)
	}

	// RevertSlide
	reverted, err := backend.RevertSlide(ctx, "slide-02")
	if err != nil {
		t.Fatalf("RevertSlide failed: %v", err)
	}
	if reverted.Slide.Notes != "" {
		t.Errorf("expected empty notes after revert, got %q", reverted.Slide.Notes)
	}

	// Non-existent slide
	_, err = backend.GetSlide(ctx, "slide-99", "")
	if err == nil {
		t.Errorf("expected error for non-existent slide")
	}
}

func TestGatewayServerEndpointsAndPresenterAuth(t *testing.T) {
	backend := NewLocalSimulatorBackend()
	presenterToken := "super-secret-token"
	gw := NewGatewayServer(backend, presenterToken)
	mux := gw.Mux()

	// 1. /readyz
	rReq := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rRec := httptest.NewRecorder()
	mux.ServeHTTP(rRec, rReq)
	if rRec.Code != http.StatusOK {
		t.Errorf("expected 200 for /readyz, got %d", rRec.Code)
	}

	// 2. /deck HTML without token -> 401 Sign-In / Presenter Key Gate
	dReq := httptest.NewRequest(http.MethodGet, "/deck", nil)
	dRec := httptest.NewRecorder()
	mux.ServeHTTP(dRec, dReq)
	if dRec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for unauthenticated /deck, got %d", dRec.Code)
	}
	if !strings.Contains(dRec.Body.String(), "Presenter Key Required") {
		t.Errorf("expected Presenter Key Required gate HTML in unauthenticated /deck response")
	}

	// 3. /deck?key=super-secret-token sets cookie & redirects
	kReq := httptest.NewRequest(http.MethodGet, "/deck?key=super-secret-token", nil)
	kRec := httptest.NewRecorder()
	mux.ServeHTTP(kRec, kReq)
	if kRec.Code != http.StatusSeeOther {
		t.Errorf("expected 303 redirect when passing token, got %d", kRec.Code)
	}
	cookies := kRec.Result().Cookies()
	var presenterCookie *http.Cookie
	for _, c := range cookies {
		if c.Name == PresenterCookieName && c.Value == presenterToken {
			presenterCookie = c
			if !c.HttpOnly {
				t.Errorf("expected HttpOnly cookie")
			}
			if c.SameSite != http.SameSiteStrictMode {
				t.Errorf("expected SameSite=Strict cookie")
			}
		}
	}
	if presenterCookie == nil {
		t.Fatalf("expected %s cookie to be set", PresenterCookieName)
	}

	// 3b. /deck with cookie -> 200 Deck HTML
	authDeckReq := httptest.NewRequest(http.MethodGet, "/deck", nil)
	authDeckReq.AddCookie(presenterCookie)
	authDeckRec := httptest.NewRecorder()
	mux.ServeHTTP(authDeckRec, authDeckReq)
	if authDeckRec.Code != http.StatusOK || !strings.Contains(authDeckRec.Body.String(), "<!DOCTYPE html>") {
		t.Errorf("expected 200 HTML for authenticated /deck, got %d", authDeckRec.Code)
	}

	// 4. GET /api/slides without auth -> 401; with cookie -> 200
	unauthSlidesReq := httptest.NewRequest(http.MethodGet, "/api/slides", nil)
	unauthSlidesRec := httptest.NewRecorder()
	mux.ServeHTTP(unauthSlidesRec, unauthSlidesReq)
	if unauthSlidesRec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for unauthenticated /api/slides, got %d", unauthSlidesRec.Code)
	}

	sReq := httptest.NewRequest(http.MethodGet, "/api/slides", nil)
	sReq.AddCookie(presenterCookie)
	sRec := httptest.NewRecorder()
	mux.ServeHTTP(sRec, sReq)
	if sRec.Code != http.StatusOK {
		t.Errorf("expected 200 for /api/slides, got %d", sRec.Code)
	}
	var slidesResp struct {
		Slides      []SlideSummary `json:"slides"`
		IsPresenter bool           `json:"is_presenter"`
		Total       int            `json:"total"`
	}
	if err := json.Unmarshal(sRec.Body.Bytes(), &slidesResp); err != nil {
		t.Fatalf("failed to unmarshal /api/slides: %v", err)
	}
	if len(slidesResp.Slides) != 22 || slidesResp.Total != 22 || !slidesResp.IsPresenter {
		t.Errorf("expected 22 slides and IsPresenter=true, got total=%d presenter=%v", slidesResp.Total, slidesResp.IsPresenter)
	}

	// 5. GET /api/slide/slide-01 with cookie -> 200
	gReq := httptest.NewRequest(http.MethodGet, "/api/slide/slide-01", nil)
	gReq.AddCookie(presenterCookie)
	gRec := httptest.NewRecorder()
	mux.ServeHTTP(gRec, gReq)
	if gRec.Code != http.StatusOK {
		t.Errorf("expected 200 for /api/slide/slide-01, got %d", gRec.Code)
	}

	// 6. POST /api/slide/slide-01/note without auth -> 401
	unauthReq := httptest.NewRequest(http.MethodPost, "/api/slide/slide-01/note", bytes.NewBufferString(`{"notes":"test"}`))
	unauthRec := httptest.NewRecorder()
	mux.ServeHTTP(unauthRec, unauthReq)
	if unauthRec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for unauthorized note edit, got %d", unauthRec.Code)
	}

	// 7. POST /api/slide/slide-01/note with X-Presenter-Key header -> 200
	authReq := httptest.NewRequest(http.MethodPost, "/api/slide/slide-01/note", bytes.NewBufferString(`{"notes":"presenter verified"}`))
	authReq.Header.Set(PresenterHeaderName, presenterToken)
	authRec := httptest.NewRecorder()
	mux.ServeHTTP(authRec, authReq)
	if authRec.Code != http.StatusOK {
		t.Errorf("expected 200 for authorized note edit, got %d", authRec.Code)
	}

	// 8. POST /api/slide/slide-01/revert without auth -> 401
	unauthRevReq := httptest.NewRequest(http.MethodPost, "/api/slide/slide-01/revert", nil)
	unauthRevRec := httptest.NewRecorder()
	mux.ServeHTTP(unauthRevRec, unauthRevReq)
	if unauthRevRec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for unauthorized revert, got %d", unauthRevRec.Code)
	}

	// 9. POST /api/slide/slide-01/revert with auth -> 200
	authRevReq := httptest.NewRequest(http.MethodPost, "/api/slide/slide-01/revert", nil)
	authRevReq.Header.Set(PresenterHeaderName, presenterToken)
	authRevRec := httptest.NewRecorder()
	mux.ServeHTTP(authRevRec, authRevReq)
	if authRevRec.Code != http.StatusOK {
		t.Errorf("expected 200 for authorized revert, got %d", authRevRec.Code)
	}

	// 10. Embedded official Agent Substrate Day assets -> 200 image/png; unknown/traversal -> 404
	for _, asset := range []string{"bg-hero.png", "bg-section.png", "bg-content.png", "mascot.png"} {
		aReq := httptest.NewRequest(http.MethodGet, "/assets/"+asset, nil)
		aRec := httptest.NewRecorder()
		mux.ServeHTTP(aRec, aReq)
		if aRec.Code != http.StatusOK {
			t.Errorf("expected 200 for /assets/%s, got %d", asset, aRec.Code)
		}
		if ct := aRec.Header().Get("Content-Type"); ct != "image/png" {
			t.Errorf("expected image/png for /assets/%s, got %q", asset, ct)
		}
		if aRec.Body.Len() < 1024 {
			t.Errorf("expected non-trivial PNG payload for /assets/%s, got %d bytes", asset, aRec.Body.Len())
		}
	}
	badAssetReq := httptest.NewRequest(http.MethodGet, "/assets/secret.png", nil)
	badAssetRec := httptest.NewRecorder()
	mux.ServeHTTP(badAssetRec, badAssetReq)
	if badAssetRec.Code != http.StatusNotFound {
		t.Errorf("expected 404 for unknown asset, got %d", badAssetRec.Code)
	}

	// 11. Cross-origin cookie-authenticated POST rejected (401/403)
	csrfReq := httptest.NewRequest(http.MethodPost, "https://abs.slides.substrate.learngke.dev/api/slide/slide-01/note", bytes.NewBufferString(`{"notes":"csrf"}`))
	csrfReq.AddCookie(presenterCookie)
	csrfReq.Header.Set("Origin", "https://evil.example")
	csrfRec := httptest.NewRecorder()
	mux.ServeHTTP(csrfRec, csrfReq)
	if csrfRec.Code != http.StatusUnauthorized && csrfRec.Code != http.StatusForbidden {
		t.Errorf("expected 401 or 403 for cross-origin cookie mutation, got %d", csrfRec.Code)
	}

	// 12. GET /api/qr.svg auth + input validation + SVG output
	unauthQRReq := httptest.NewRequest(http.MethodGet, "/api/qr.svg", nil)
	unauthQRRec := httptest.NewRecorder()
	mux.ServeHTTP(unauthQRRec, unauthQRReq)
	if unauthQRRec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for unauthenticated /api/qr.svg, got %d", unauthQRRec.Code)
	}

	qrReq := httptest.NewRequest(http.MethodGet, "/api/qr.svg", nil)
	qrReq.AddCookie(presenterCookie)
	qrRec := httptest.NewRecorder()
	mux.ServeHTTP(qrRec, qrReq)
	if qrRec.Code != http.StatusOK {
		t.Errorf("expected 200 for authenticated /api/qr.svg, got %d", qrRec.Code)
	}
	if ct := qrRec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "image/svg+xml") {
		t.Errorf("expected image/svg+xml Content-Type, got %q", ct)
	}
	if !strings.Contains(qrRec.Body.String(), "<svg") || !strings.Contains(qrRec.Body.String(), "<path") {
		t.Errorf("expected valid QR <svg><path> body from /api/qr.svg")
	}

	badSchemeReq := httptest.NewRequest(http.MethodGet, "/api/qr.svg?url=javascript:void(0)", nil)
	badSchemeReq.AddCookie(presenterCookie)
	badSchemeRec := httptest.NewRecorder()
	mux.ServeHTTP(badSchemeRec, badSchemeReq)
	if badSchemeRec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for non-http(s) QR URL scheme, got %d", badSchemeRec.Code)
	}

	tooLongURL := "https://dungeon.substrate.learngke.dev/play?" + strings.Repeat("x", 120)
	longReq := httptest.NewRequest(http.MethodGet, "/api/qr.svg?url="+tooLongURL, nil)
	longReq.AddCookie(presenterCookie)
	longRec := httptest.NewRecorder()
	mux.ServeHTTP(longRec, longReq)
	if longRec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for >104-byte QR URL, got %d", longRec.Code)
	}
}

func TestIndexHTMLDesignAndConventions(t *testing.T) {
	html := string(indexHTML)

	// Check Agent Substrate Day official template branding, palette, typography, ate.dev grid/viz, and jigsaw puzzle UI
	for _, token := range []string{
		"#07090D",
		"#56FD5A",
		"Ubuntu",
		"Ubuntu Mono",
		"Google Cloud Booth",
		"AGENT SUBSTRATE",
		"btnLogout",
		"/api/auth/presenter.js",
		"asd-puzzle",
		"buildJigsawCellPath",
		"ActorEgressPolicy",
		"ate-secret://",
		"OpenFGA",
		"asd-grid-layer",
		"as-viz",
		"legendModal",
		"legendGrid",
		"chrome-hidden",
		"toggleChrome",
		"slideCounter",
		"qrModal",
		"hudPoolSlots",
		"buildHeroRightVisual",
		"buildMultiverseTimeline",
		"buildTieringCompressionViz",
		"startTerminalTypewriter",
		"asd-pz-shockwave-ring",
		"asd-mascot-welcome",
	} {
		if !strings.Contains(html, token) {
			t.Errorf("expected official template token %q in index.html", token)
		}
	}

	// Ensure safe DOM construction (no innerHTML / outerHTML / native alert / confirm)
	for _, forbidden := range []string{".innerHTML", ".outerHTML", "insertAdjacentHTML", "document.write", "alert(", "confirm("} {
		if strings.Contains(html, forbidden) {
			t.Errorf("unexpected unsafe/blocking browser API %q in index.html", forbidden)
		}
	}

	// Check Projector Light Mode toggle with pure SVG icons and zero text label
	if !strings.Contains(html, "data-theme") {
		t.Errorf("expected data-theme support in HTML")
	}
	if !strings.Contains(html, "btnThemeLight") || !strings.Contains(html, "btnThemeDark") {
		t.Errorf("expected btnThemeLight and btnThemeDark toggle elements")
	}

	// Check for SVG icon usage
	if !strings.Contains(html, "<svg") {
		t.Errorf("expected SVG icons in HTML")
	}

	// Check for Substrate telemetry HUD strip
	if !strings.Contains(html, "hud-strip") {
		t.Errorf("expected hud-strip in HTML")
	}
	if !strings.Contains(html, "hudStatus") {
		t.Errorf("expected hudStatus indicator in HTML")
	}

	// Check for keyboard shortcuts
	if !strings.Contains(html, "ArrowRight") || !strings.Contains(html, "ArrowLeft") {
		t.Errorf("expected arrow key navigation in JavaScript")
	}

	// Check for 1.5s offline cache fallback
	if !strings.Contains(html, "STATIC_SLIDES") {
		t.Errorf("expected STATIC_SLIDES pre-bundled cache in JavaScript")
	}
	if !strings.Contains(html, "1500") {
		t.Errorf("expected 1500ms timeout race in JavaScript")
	}
	if !strings.Contains(html, "LOCAL CACHE FALLBACK") {
		t.Errorf("expected LOCAL CACHE FALLBACK status string in JavaScript")
	}

	// Check that all 16 slide IDs are pre-bundled in STATIC_SLIDES
	for _, def := range AllSlideDefs() {
		if !strings.Contains(html, def.ID) {
			t.Errorf("expected slide ID %s in HTML cache", def.ID)
		}
	}
}
