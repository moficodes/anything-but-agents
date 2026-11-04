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

package presenterauth

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func mustSignTestRS256(t *testing.T, priv *rsa.PrivateKey, kid string, claims map[string]any) string {
	t.Helper()
	hdrBytes, _ := json.Marshal(map[string]string{"alg": "RS256", "kid": kid, "typ": "JWT"})
	claimsBytes, _ := json.Marshal(claims)
	signingInput := base64.RawURLEncoding.EncodeToString(hdrBytes) + "." + base64.RawURLEncoding.EncodeToString(claimsBytes)
	digest := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, priv, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatalf("SignPKCS1v15: %v", err)
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func startTestJWKSServer(t *testing.T, kid string, pub *rsa.PublicKey) *httptest.Server {
	t.Helper()
	eBytes := big.NewInt(int64(pub.E)).Bytes()
	payload, _ := json.Marshal(map[string]any{
		"keys": []map[string]string{
			{
				"kty": "RSA",
				"alg": "RS256",
				"use": "sig",
				"kid": kid,
				"n":   base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
				"e":   base64.RawURLEncoding.EncodeToString(eBytes),
			},
		},
	})
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(payload)
	}))
}

func TestGoogleVerifierOIDCAndFirebaseDomains(t *testing.T) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	jwks := startTestJWKSServer(t, "test-kid-1", &priv.PublicKey)
	defer jwks.Close()

	fixedNow := time.Date(2026, 11, 3, 18, 0, 0, 0, time.UTC)
	v := NewGoogleVerifier(GoogleAuthConfig{
		AllowedDomains:    []string{"@google.com, event.goog", "cloud-demos.goog", "gcdemos.goog"},
		ClientID:          "web-client-123.apps.googleusercontent.com",
		FirebaseProjectID: "demo-idp-proj",
		GoogleJWKSURL:     jwks.URL,
		FirebaseJWKSURL:   jwks.URL,
		Now:               func() time.Time { return fixedNow },
	})
	if v == nil {
		t.Fatal("expected non-nil GoogleVerifier")
	}

	// 1. Valid @google.com OIDC token
	tokGoogle := mustSignTestRS256(t, priv, "test-kid-1", map[string]any{
		"iss":            "https://accounts.google.com",
		"aud":            "web-client-123.apps.googleusercontent.com",
		"email":          "Speaker@google.com",
		"email_verified": true,
		"hd":             "google.com",
		"iat":            fixedNow.Unix(),
		"exp":            fixedNow.Add(30 * time.Minute).Unix(),
	})
	email, err := v.VerifyIDToken(context.Background(), tokGoogle)
	if err != nil || email != "speaker@google.com" {
		t.Fatalf("expected speaker@google.com, got email=%q err=%v", email, err)
	}

	// 2. Valid @event.goog Firebase ID token
	tokFirebase := mustSignTestRS256(t, priv, "test-kid-1", map[string]any{
		"iss":            "https://securetoken.google.com/demo-idp-proj",
		"aud":            "demo-idp-proj",
		"email":          "presenter@event.goog",
		"email_verified": true,
		"iat":            fixedNow.Unix(),
		"exp":            fixedNow.Add(30 * time.Minute).Unix(),
	})
	email, err = v.VerifyIDToken(context.Background(), tokFirebase)
	if err != nil || email != "presenter@event.goog" {
		t.Fatalf("expected presenter@event.goog, got email=%q err=%v", email, err)
	}

	// 3. Reject unauthorized domain (@gmail.com)
	tokGmail := mustSignTestRS256(t, priv, "test-kid-1", map[string]any{
		"iss":            "https://accounts.google.com",
		"aud":            "web-client-123.apps.googleusercontent.com",
		"email":          "user@gmail.com",
		"email_verified": true,
		"iat":            fixedNow.Unix(),
		"exp":            fixedNow.Add(30 * time.Minute).Unix(),
	})
	if _, err := v.VerifyIDToken(context.Background(), tokGmail); err == nil {
		t.Fatal("expected @gmail.com token to be rejected")
	}

	// 4. Reject unverified email
	tokUnverified := mustSignTestRS256(t, priv, "test-kid-1", map[string]any{
		"iss":            "https://accounts.google.com",
		"aud":            "web-client-123.apps.googleusercontent.com",
		"email":          "speaker@google.com",
		"email_verified": false,
		"iat":            fixedNow.Unix(),
		"exp":            fixedNow.Add(30 * time.Minute).Unix(),
	})
	if _, err := v.VerifyIDToken(context.Background(), tokUnverified); err == nil {
		t.Fatal("expected unverified email token to be rejected")
	}

	// 5. Reject alg:none forgery
	noneHdr := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","kid":"test-kid-1"}`))
	nonePayload := base64.RawURLEncoding.EncodeToString([]byte(`{"iss":"https://accounts.google.com","aud":"web-client-123.apps.googleusercontent.com","email":"speaker@google.com","email_verified":true,"exp":9999999999}`))
	if _, err := v.VerifyIDToken(context.Background(), noneHdr+"."+nonePayload+"."); err == nil {
		t.Fatal("expected alg:none token to be rejected")
	}

	// 6. Reject expired token
	tokExpired := mustSignTestRS256(t, priv, "test-kid-1", map[string]any{
		"iss":            "https://accounts.google.com",
		"aud":            "web-client-123.apps.googleusercontent.com",
		"email":          "speaker@google.com",
		"email_verified": true,
		"iat":            fixedNow.Add(-2 * time.Hour).Unix(),
		"exp":            fixedNow.Add(-1 * time.Minute).Unix(),
	})
	if _, err := v.VerifyIDToken(context.Background(), tokExpired); err == nil {
		t.Fatal("expected expired token to be rejected")
	}

	// 7. Reject mismatched hd claim
	tokBadHD := mustSignTestRS256(t, priv, "test-kid-1", map[string]any{
		"iss":            "https://accounts.google.com",
		"aud":            "web-client-123.apps.googleusercontent.com",
		"email":          "speaker@google.com",
		"email_verified": true,
		"hd":             "evil.example",
		"iat":            fixedNow.Unix(),
		"exp":            fixedNow.Add(30 * time.Minute).Unix(),
	})
	if _, err := v.VerifyIDToken(context.Background(), tokBadHD); err == nil {
		t.Fatal("expected mismatched hd claim to be rejected")
	}
}

func TestCrossSubdomainSSOAndCSRFGuards(t *testing.T) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	jwks := startTestJWKSServer(t, "kid-sso", &priv.PublicKey)
	defer jwks.Close()

	sharedSecret := []byte("shared-hmac-secret-across-all-6-demos-32b")
	googleCfg := GoogleAuthConfig{
		AllowedDomains:    []string{"google.com", "event.goog", "cloud-demos.goog", "gcdemos.goog"},
		FirebaseAPIKey:    "fake-firebase-api-key",
		FirebaseProjectID: "demo-idp-proj",
		GoogleJWKSURL:     jwks.URL,
		FirebaseJWKSURL:   jwks.URL,
	}

	// Gate A: Slides on abs.slides.substrate.learngke.dev
	slidesGate := NewGate(GateConfig{
		Title:            "Demo 0: Slides on Substrate",
		LegacyCookieName: "slides_presenter",
		PresenterToken:   "shared-stage-token",
		HMACSecret:       sharedSecret,
		CookieDomain:     "substrate.learngke.dev",
		GoogleAuth:       googleCfg,
	})
	slidesMux := http.NewServeMux()
	slidesGate.RegisterRoutes(slidesMux)
	slidesMux.HandleFunc("/deck", func(w http.ResponseWriter, r *http.Request) {
		if slidesGate.HandleUnlockOrGate(w, r, "/deck") {
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("SLIDES_UNLOCKED"))
	})

	// Gate B: Dungeon Dashboard on dungeon.substrate.learngke.dev
	dungeonGate := NewGate(GateConfig{
		Title:            "Demo 1b: Dungeon 2D Dashboard",
		LegacyCookieName: "d2d_presenter",
		PresenterToken:   "shared-stage-token",
		HMACSecret:       sharedSecret,
		CookieDomain:     "substrate.learngke.dev",
		GoogleAuth:       googleCfg,
	})
	dungeonMux := http.NewServeMux()
	dungeonGate.RegisterRoutes(dungeonMux)
	dungeonMux.HandleFunc("/dashboard", func(w http.ResponseWriter, r *http.Request) {
		if dungeonGate.HandleUnlockOrGate(w, r, "/dashboard") {
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("DUNGEON_UNLOCKED"))
	})
	dungeonMux.HandleFunc("/api/session/delete", dungeonGate.RequireAPI(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"deleted":"d2d-hero"}`))
	}))

	// 1. Unauthenticated GET /deck on abs.slides returns 401 Google Sign-In Gate + CSRF cookie + Moon/Sun toggle + zero emojis
	gateReq := httptest.NewRequest(http.MethodGet, "https://abs.slides.substrate.learngke.dev/deck", nil)
	gateReq.Header.Set("X-Forwarded-Proto", "https")
	gateRec := httptest.NewRecorder()
	slidesMux.ServeHTTP(gateRec, gateReq)
	if gateRec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 on unauthenticated /deck, got %d", gateRec.Code)
	}
	htmlBody := gateRec.Body.String()
	for _, want := range []string{"@google.com", "@event.goog", "@cloud-demos.goog", "@gcdemos.goog", "btnThemeToggle", "Sign in with Google"} {
		if !strings.Contains(htmlBody, want) {
			t.Fatalf("expected Sign-In Gate HTML to contain %q", want)
		}
	}
	for _, r := range htmlBody {
		if r >= 0x1F300 && r <= 0x1FAFF {
			t.Fatalf("found prohibited emoji %U in Sign-In Gate HTML", r)
		}
	}
	if gateRec.Header().Get("Content-Security-Policy") == "" || gateRec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("expected CSP and nosniff headers on Sign-In Gate")
	}

	var csrfCookie *http.Cookie
	for _, c := range gateRec.Result().Cookies() {
		if c.Name == CSRFCookieName {
			csrfCookie = c
		}
	}
	if csrfCookie == nil || !csrfCookie.HttpOnly {
		t.Fatalf("expected HttpOnly %s cookie", CSRFCookieName)
	}

	// 2. POST /api/auth/google WITH csrf cookie but WITHOUT X-CSRF-Token header -> 403 Forbidden
	idToken := mustSignTestRS256(t, priv, "kid-sso", map[string]any{
		"iss":            "https://securetoken.google.com/demo-idp-proj",
		"aud":            "demo-idp-proj",
		"email":          "speaker@cloud-demos.goog",
		"email_verified": true,
		"iat":            time.Now().Unix(),
		"exp":            time.Now().Add(30 * time.Minute).Unix(),
	})
	missingCSRFReq := httptest.NewRequest(http.MethodPost, "https://abs.slides.substrate.learngke.dev/api/auth/google",
		bytes.NewBufferString(`{"id_token":"`+idToken+`"}`))
	missingCSRFReq.Header.Set("Content-Type", "application/json")
	missingCSRFReq.Header.Set("X-Forwarded-Proto", "https")
	missingCSRFReq.AddCookie(csrfCookie)
	missingCSRFRec := httptest.NewRecorder()
	slidesMux.ServeHTTP(missingCSRFRec, missingCSRFReq)
	if missingCSRFRec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 when X-CSRF-Token is missing, got %d", missingCSRFRec.Code)
	}

	// 3. POST /api/auth/google WITH matching X-CSRF-Token -> 200 OK & issues __Secure-aba_session for substrate.learngke.dev
	authReq := httptest.NewRequest(http.MethodPost, "https://abs.slides.substrate.learngke.dev/api/auth/google",
		bytes.NewBufferString(`{"id_token":"`+idToken+`"}`))
	authReq.Header.Set("Content-Type", "application/json")
	authReq.Header.Set("X-Forwarded-Proto", "https")
	authReq.Header.Set("Origin", "https://abs.slides.substrate.learngke.dev")
	authReq.Header.Set(CSRFHeaderName, csrfCookie.Value)
	authReq.AddCookie(csrfCookie)
	authRec := httptest.NewRecorder()
	slidesMux.ServeHTTP(authRec, authReq)
	if authRec.Code != http.StatusOK {
		t.Fatalf("expected 200 on Google Sign-In, got %d: %s", authRec.Code, authRec.Body.String())
	}

	var ssoCookie *http.Cookie
	for _, c := range authRec.Result().Cookies() {
		if c.Name == SecureSessionCookieName {
			ssoCookie = c
		}
	}
	if ssoCookie == nil || ssoCookie.Domain != "substrate.learngke.dev" || !ssoCookie.Secure || !ssoCookie.HttpOnly {
		t.Fatalf("expected cross-subdomain __Secure-aba_session cookie on substrate.learngke.dev, got %+v", ssoCookie)
	}

	// 4. Present ONLY ssoCookie from abs.slides to dungeon.substrate.learngke.dev/dashboard -> 200 OK (Single Sign-On!)
	dReq := httptest.NewRequest(http.MethodGet, "https://dungeon.substrate.learngke.dev/dashboard", nil)
	dReq.Header.Set("X-Forwarded-Proto", "https")
	dReq.AddCookie(ssoCookie)
	dRec := httptest.NewRecorder()
	dungeonMux.ServeHTTP(dRec, dReq)
	if dRec.Code != http.StatusOK || dRec.Body.String() != "DUNGEON_UNLOCKED" {
		t.Fatalf("expected cross-subdomain SSO to unlock dungeon /dashboard, got status=%d body=%q", dRec.Code, dRec.Body.String())
	}

	// 5. Cross-site POST with ssoCookie (Sec-Fetch-Site: cross-site or foreign Origin) -> 401 rejected (CSRF protection!)
	csrfAttackReq := httptest.NewRequest(http.MethodPost, "https://dungeon.substrate.learngke.dev/api/session/delete", nil)
	csrfAttackReq.Header.Set("Origin", "https://attacker.example")
	csrfAttackReq.Header.Set("Sec-Fetch-Site", "cross-site")
	csrfAttackReq.AddCookie(ssoCookie)
	csrfAttackRec := httptest.NewRecorder()
	dungeonMux.ServeHTTP(csrfAttackRec, csrfAttackReq)
	if csrfAttackRec.Code != http.StatusUnauthorized {
		t.Fatalf("expected cross-site POST to be rejected with 401, got %d", csrfAttackRec.Code)
	}

	// 6. Same-origin POST with ssoCookie -> 200 OK
	validPostReq := httptest.NewRequest(http.MethodPost, "https://dungeon.substrate.learngke.dev/api/session/delete", nil)
	validPostReq.Header.Set("Origin", "https://dungeon.substrate.learngke.dev")
	validPostReq.Header.Set("Sec-Fetch-Site", "same-origin")
	validPostReq.AddCookie(ssoCookie)
	validPostRec := httptest.NewRecorder()
	dungeonMux.ServeHTTP(validPostRec, validPostReq)
	if validPostRec.Code != http.StatusOK {
		t.Fatalf("expected same-origin POST with ssoCookie to succeed, got %d", validPostRec.Code)
	}

	// 7. GET /api/auth/session returns masked Google identity + CSRF token, and POST /api/auth/logout enforces Double-Submit CSRF and clears session cookies
	sessReq := httptest.NewRequest(http.MethodGet, "https://abs.slides.substrate.learngke.dev/api/auth/session", nil)
	sessReq.Header.Set("X-Forwarded-Proto", "https")
	sessReq.AddCookie(ssoCookie)
	sessRec := httptest.NewRecorder()
	slidesMux.ServeHTTP(sessRec, sessReq)
	if sessRec.Code != http.StatusOK {
		t.Fatalf("expected 200 on /api/auth/session, got %d: %s", sessRec.Code, sessRec.Body.String())
	}
	var sessBody struct {
		Enabled       bool   `json:"enabled"`
		Authenticated bool   `json:"authenticated"`
		AuthMethod    string `json:"auth_method"`
		Subject       string `json:"subject"`
		CSRFToken     string `json:"csrf_token"`
	}
	if err := json.Unmarshal(sessRec.Body.Bytes(), &sessBody); err != nil {
		t.Fatalf("unmarshal /api/auth/session: %v", err)
	}
	if !sessBody.Enabled || !sessBody.Authenticated || sessBody.AuthMethod != "google" || sessBody.Subject != "s***@cloud-demos.goog" || sessBody.CSRFToken == "" {
		t.Fatalf("unexpected /api/auth/session payload: %+v", sessBody)
	}
	var logoutCSRFCookie *http.Cookie
	for _, c := range sessRec.Result().Cookies() {
		if c.Name == CSRFCookieName {
			logoutCSRFCookie = c
		}
	}
	if logoutCSRFCookie == nil {
		t.Fatalf("expected %s cookie from /api/auth/session", CSRFCookieName)
	}

	// Logout WITHOUT X-CSRF-Token when aba_csrf cookie is present -> 403 Forbidden
	badLogoutReq := httptest.NewRequest(http.MethodPost, "https://abs.slides.substrate.learngke.dev/api/auth/logout", nil)
	badLogoutReq.Header.Set("Origin", "https://abs.slides.substrate.learngke.dev")
	badLogoutReq.Header.Set("X-Forwarded-Proto", "https")
	badLogoutReq.AddCookie(logoutCSRFCookie)
	badLogoutRec := httptest.NewRecorder()
	slidesMux.ServeHTTP(badLogoutRec, badLogoutReq)
	if badLogoutRec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 on logout without X-CSRF-Token, got %d", badLogoutRec.Code)
	}

	// Logout WITH matching X-CSRF-Token -> 200 OK & clears session cookies
	logoutReq := httptest.NewRequest(http.MethodPost, "https://abs.slides.substrate.learngke.dev/api/auth/logout", nil)
	logoutReq.Header.Set("Origin", "https://abs.slides.substrate.learngke.dev")
	logoutReq.Header.Set("X-Forwarded-Proto", "https")
	logoutReq.Header.Set(CSRFHeaderName, sessBody.CSRFToken)
	logoutReq.AddCookie(logoutCSRFCookie)
	logoutRec := httptest.NewRecorder()
	slidesMux.ServeHTTP(logoutRec, logoutReq)
	if logoutRec.Code != http.StatusOK {
		t.Fatalf("expected 200 on logout, got %d", logoutRec.Code)
	}
	var cleared bool
	for _, c := range logoutRec.Result().Cookies() {
		if c.Name == SecureSessionCookieName && c.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Fatalf("expected %s to be expired on logout", SecureSessionCookieName)
	}
}

func TestTokenAuthAndLogoutFlow(t *testing.T) {
	gate := NewGate(GateConfig{
		Title:            "Demo Token Logout Test",
		LegacyCookieName: "demo_presenter",
		PresenterToken:   "stage-secret-token-123",
	})
	mux := http.NewServeMux()
	gate.RegisterRoutes(mux)
	mux.HandleFunc("/dashboard", func(w http.ResponseWriter, r *http.Request) {
		if gate.HandleUnlockOrGate(w, r, "/dashboard") {
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("DASHBOARD_OK"))
	})

	// 1. /api/auth/presenter.js serves the shared logout client script
	jsRec := httptest.NewRecorder()
	mux.ServeHTTP(jsRec, httptest.NewRequest(http.MethodGet, "/api/auth/presenter.js", nil))
	if jsRec.Code != http.StatusOK || !strings.Contains(jsRec.Body.String(), "btnLogout") {
		t.Fatalf("expected 200 presenter.js containing btnLogout, got %d", jsRec.Code)
	}

	// 2. Unlock via POST /api/auth/token (Sign-In form flow)
	gateRec := httptest.NewRecorder()
	mux.ServeHTTP(gateRec, httptest.NewRequest(http.MethodGet, "http://localhost:8080/dashboard", nil))
	var csrfCookie *http.Cookie
	for _, c := range gateRec.Result().Cookies() {
		if c.Name == CSRFCookieName {
			csrfCookie = c
		}
	}
	if csrfCookie == nil {
		t.Fatal("expected CSRF cookie on Sign-In Gate")
	}

	tokReq := httptest.NewRequest(http.MethodPost, "http://localhost:8080/api/auth/token",
		bytes.NewBufferString(`{"key":"stage-secret-token-123"}`))
	tokReq.Header.Set("Content-Type", "application/json")
	tokReq.Header.Set("Origin", "http://localhost:8080")
	tokReq.Header.Set(CSRFHeaderName, csrfCookie.Value)
	tokReq.AddCookie(csrfCookie)
	tokRec := httptest.NewRecorder()
	mux.ServeHTTP(tokRec, tokReq)
	if tokRec.Code != http.StatusOK {
		t.Fatalf("expected 200 on POST /api/auth/token, got %d: %s", tokRec.Code, tokRec.Body.String())
	}
	issued := tokRec.Result().Cookies()

	// 3. Authenticated /api/auth/session reports auth_method="token"
	sessReq := httptest.NewRequest(http.MethodGet, "http://localhost:8080/api/auth/session", nil)
	for _, c := range issued {
		if c.MaxAge >= 0 && c.Value != "" {
			sessReq.AddCookie(c)
		}
	}
	sessRec := httptest.NewRecorder()
	mux.ServeHTTP(sessRec, sessReq)
	if sessRec.Code != http.StatusOK || !strings.Contains(sessRec.Body.String(), `"auth_method": "token"`) {
		t.Fatalf("expected token session metadata, got %d: %s", sessRec.Code, sessRec.Body.String())
	}
	var sessPayload struct {
		CSRFToken string `json:"csrf_token"`
	}
	_ = json.Unmarshal(sessRec.Body.Bytes(), &sessPayload)
	var sessCSRF *http.Cookie
	for _, c := range sessRec.Result().Cookies() {
		if c.Name == CSRFCookieName {
			sessCSRF = c
		}
	}

	// 4. POST /api/auth/logout expires both demo_presenter and aba_session cookies
	logoutReq := httptest.NewRequest(http.MethodPost, "http://localhost:8080/api/auth/logout", nil)
	logoutReq.Header.Set("Origin", "http://localhost:8080")
	logoutReq.Header.Set(CSRFHeaderName, sessPayload.CSRFToken)
	if sessCSRF != nil {
		logoutReq.AddCookie(sessCSRF)
	}
	logoutRec := httptest.NewRecorder()
	mux.ServeHTTP(logoutRec, logoutReq)
	if logoutRec.Code != http.StatusOK {
		t.Fatalf("expected 200 on token logout, got %d", logoutRec.Code)
	}
	expiredNames := map[string]bool{}
	for _, c := range logoutRec.Result().Cookies() {
		if c.MaxAge < 0 {
			expiredNames[c.Name] = true
		}
	}
	for _, want := range []string{"demo_presenter", SessionCookieName, CSRFCookieName} {
		if !expiredNames[want] {
			t.Fatalf("expected cookie %q to be expired on token logout, got %+v", want, expiredNames)
		}
	}
}
