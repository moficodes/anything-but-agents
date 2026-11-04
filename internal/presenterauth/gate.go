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
	"cmp"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const (
	// PresenterHeaderName carries the presenter token for CLI scripts and tests.
	PresenterHeaderName = "X-Presenter-Key"
	// SessionCookieName is the host-only HMAC-signed session cookie.
	SessionCookieName = "aba_session"
	// SecureSessionCookieName is the cross-subdomain HTTPS session cookie when COOKIE_DOMAIN is set.
	SecureSessionCookieName = "__Secure-aba_session"
	// CSRFCookieName stores the Double-Submit CSRF token for /api/auth/google.
	CSRFCookieName = "aba_csrf"
	// CSRFHeaderName carries the Double-Submit CSRF token from the Sign-In page.
	CSRFHeaderName = "X-CSRF-Token"

	sessionTTL          = 12 * time.Hour
	maxAuthPayloadBytes = 32 << 10
)

// GateConfig configures presenter authentication for a demo gateway.
type GateConfig struct {
	// Title identifies the demo on the Sign-In Gate card.
	Title string
	// Subtitle describes what signing in unlocks.
	Subtitle string
	// SecretName is the Kubernetes Secret holding PRESENTER_TOKEN (for CLI hint).
	SecretName string
	// LegacyCookieName is the per-demo cookie name (e.g. "cs_presenter") kept
	// for backward compatibility with existing scripts and unit tests.
	LegacyCookieName string
	// PresenterToken unlocks presenter routes via ?key= or X-Presenter-Key.
	PresenterToken string
	// HMACSecret signs cross-subdomain session cookies (defaults to SESSION_HMAC_SECRET env var).
	HMACSecret []byte
	// CookieDomain scopes __Secure-aba_session across demo subdomains (defaults to COOKIE_DOMAIN env var).
	CookieDomain string
	// GoogleAuth configures Google Workspace / event-domain sign-in (merged with env vars).
	GoogleAuth GoogleAuthConfig
	// Now overrides time.Now in tests.
	Now func() time.Time
}

// Gate enforces Google Sign-In and/or PRESENTER_TOKEN authentication across presenter pages and APIs.
type Gate struct {
	title            string
	subtitle         string
	secretName       string
	legacyCookieName string
	presenterToken   string
	hmacSecret       []byte
	cookieDomain     string
	googleCfg        GoogleAuthConfig
	googleVerifier   *GoogleVerifier
	now              func() time.Time
}

// RandomToken returns a cryptographically random hex string of byteLen bytes.
func RandomToken(byteLen int) string {
	if byteLen <= 0 {
		byteLen = 24
	}
	b := make([]byte, byteLen)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// NewGate constructs a Gate from cfg and environment defaults.
func NewGate(cfg GateConfig) *Gate {
	googleCfg := MergeEnv(cfg.GoogleAuth)
	verifier := NewGoogleVerifier(googleCfg)
	if verifier != nil {
		googleCfg = verifier.Config()
	}

	presenterToken := strings.TrimSpace(cfg.PresenterToken)
	cookieDomain := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(cmp.Or(cfg.CookieDomain, os.Getenv("COOKIE_DOMAIN"))), "."))

	var secret []byte
	switch {
	case len(cfg.HMACSecret) > 0:
		secret = bytes.Clone(cfg.HMACSecret)
	case strings.TrimSpace(os.Getenv("SESSION_HMAC_SECRET")) != "":
		secret = []byte(strings.TrimSpace(os.Getenv("SESSION_HMAC_SECRET")))
	case presenterToken != "":
		sum := sha256.Sum256([]byte("aba-session-v1:" + presenterToken))
		secret = sum[:]
	default:
		secret = make([]byte, 32)
		_, _ = rand.Read(secret)
	}

	nowFn := cfg.Now
	if nowFn == nil {
		nowFn = time.Now
	}

	title := cmp.Or(strings.TrimSpace(cfg.Title), "Agent Substrate Live Demo")
	subtitle := cmp.Or(
		strings.TrimSpace(cfg.Subtitle),
		"Sign in with an authorized Google Workspace or event account to access the presenter console and live actor controls.",
	)
	secretName := cmp.Or(strings.TrimSpace(cfg.SecretName), "presenter-auth")

	return &Gate{
		title:            title,
		subtitle:         subtitle,
		secretName:       secretName,
		legacyCookieName: strings.TrimSpace(cfg.LegacyCookieName),
		presenterToken:   presenterToken,
		hmacSecret:       secret,
		cookieDomain:     cookieDomain,
		googleCfg:        googleCfg,
		googleVerifier:   verifier,
		now:              nowFn,
	}
}

// Enabled reports whether either PRESENTER_TOKEN or Google Sign-In is configured.
func (g *Gate) Enabled() bool {
	return g != nil && (g.presenterToken != "" || g.googleVerifier != nil)
}

// GoogleEnabled reports whether Google Sign-In is active.
func (g *Gate) GoogleEnabled() bool {
	return g != nil && g.googleVerifier != nil
}

// PresenterKey returns the configured PRESENTER_TOKEN.
func (g *Gate) PresenterKey() string {
	if g == nil {
		return ""
	}
	return g.presenterToken
}

type sessionClaims struct {
	Sub        string `json:"sub"`
	AuthMethod string `json:"auth,omitempty"`
	Exp        int64  `json:"exp"`
	Iat        int64  `json:"iat"`
}

func inferAuthMethod(sub string) string {
	if strings.Contains(sub, "@") {
		return "google"
	}
	return "token"
}

func maskEmailSubject(sub string) string {
	sub = strings.TrimSpace(sub)
	at := strings.IndexByte(sub, '@')
	if at < 0 {
		return "presenter-token"
	}
	local, domain := sub[:at], sub[at+1:]
	if len(local) <= 1 {
		return "*@" + domain
	}
	return string(local[0]) + "***@" + domain
}

// MintSession creates an HS256-signed session JWT for sub.
func (g *Gate) MintSession(sub string) string {
	now := g.now()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	claimsBytes, _ := json.Marshal(sessionClaims{
		Sub:        sub,
		AuthMethod: inferAuthMethod(sub),
		Iat:        now.Unix(),
		Exp:        now.Add(sessionTTL).Unix(),
	})
	payload := base64.RawURLEncoding.EncodeToString(claimsBytes)
	signingInput := header + "." + payload

	mac := hmac.New(sha256.New, g.hmacSecret)
	mac.Write([]byte(signingInput))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return signingInput + "." + sig
}

func (g *Gate) verifySessionClaims(tokenStr string) (sessionClaims, bool) {
	if g == nil || len(g.hmacSecret) == 0 {
		return sessionClaims{}, false
	}
	parts := strings.Split(strings.TrimSpace(tokenStr), ".")
	if len(parts) != 3 {
		return sessionClaims{}, false
	}

	hdrBytes, err := decodeBase64URL(parts[0])
	if err != nil {
		return sessionClaims{}, false
	}
	var hdr jwtHeader
	if err := json.Unmarshal(hdrBytes, &hdr); err != nil || hdr.Alg != "HS256" {
		return sessionClaims{}, false
	}

	signingInput := parts[0] + "." + parts[1]
	mac := hmac.New(sha256.New, g.hmacSecret)
	mac.Write([]byte(signingInput))
	expectedSig := mac.Sum(nil)

	actualSig, err := decodeBase64URL(parts[2])
	if err != nil || !hmac.Equal(actualSig, expectedSig) {
		return sessionClaims{}, false
	}

	payloadBytes, err := decodeBase64URL(parts[1])
	if err != nil {
		return sessionClaims{}, false
	}
	var claims sessionClaims
	if err := json.Unmarshal(payloadBytes, &claims); err != nil {
		return sessionClaims{}, false
	}
	if claims.Sub == "" || claims.Exp <= g.now().Unix() {
		return sessionClaims{}, false
	}
	if claims.AuthMethod == "" {
		claims.AuthMethod = inferAuthMethod(claims.Sub)
	}
	return claims, true
}

// VerifySession validates an HS256 session JWT and returns its subject claim.
func (g *Gate) VerifySession(tokenStr string) (string, bool) {
	claims, ok := g.verifySessionClaims(tokenStr)
	if !ok {
		return "", false
	}
	return claims.Sub, true
}

func (g *Gate) authenticatedIdentity(r *http.Request) (authMethod, maskedSub string, ok bool) {
	if !g.Enabled() {
		return "none", "", true
	}

	if g.presenterToken != "" {
		if key := strings.TrimSpace(r.Header.Get(PresenterHeaderName)); key != "" {
			if subtle.ConstantTimeCompare([]byte(key), []byte(g.presenterToken)) == 1 {
				return "token", "presenter-token", true
			}
		}
		if r.Method == http.MethodGet || r.Method == http.MethodHead {
			q := r.URL.Query()
			key := strings.TrimSpace(q.Get("key"))
			if key == "" {
				key = strings.TrimSpace(q.Get("token"))
			}
			if key != "" && subtle.ConstantTimeCompare([]byte(key), []byte(g.presenterToken)) == 1 {
				return "token", "presenter-token", true
			}
		}
	}

	if isStateChangingMethod(r.Method) && !g.sameOriginRequest(r) {
		return "", "", false
	}

	if c, err := r.Cookie(SecureSessionCookieName); err == nil {
		if claims, valid := g.verifySessionClaims(c.Value); valid {
			return claims.AuthMethod, maskEmailSubject(claims.Sub), true
		}
	}
	if c, err := r.Cookie(SessionCookieName); err == nil {
		if claims, valid := g.verifySessionClaims(c.Value); valid {
			return claims.AuthMethod, maskEmailSubject(claims.Sub), true
		}
	}
	if g.legacyCookieName != "" {
		if c, err := r.Cookie(g.legacyCookieName); err == nil {
			if g.presenterToken != "" && subtle.ConstantTimeCompare([]byte(c.Value), []byte(g.presenterToken)) == 1 {
				return "token", "presenter-token", true
			}
			if claims, valid := g.verifySessionClaims(c.Value); valid {
				return claims.AuthMethod, maskEmailSubject(claims.Sub), true
			}
		}
	}

	return "", "", false
}

// IsAuthorized reports whether r carries a valid X-Presenter-Key header,
// ?key= query parameter (on GET/HEAD), cross-subdomain session cookie, or
// legacy presenter cookie.
func (g *Gate) IsAuthorized(r *http.Request) bool {
	_, _, ok := g.authenticatedIdentity(r)
	return ok
}

// IssueSessionCookies sets the cross-subdomain __Secure-aba_session cookie
// when served over HTTPS on COOKIE_DOMAIN (expiring any stale host-only cookies
// so a single logout revokes access across every subdomain), or sets the
// host-only legacy + session cookies for local HTTP / port-forward / tests.
func (g *Gate) IssueSessionCookies(w http.ResponseWriter, r *http.Request, sub string) {
	secure := isHTTPS(r)
	exp := g.now().Add(sessionTTL)
	signed := g.MintSession(sub)
	useCrossDomainSSO := secure && g.cookieDomain != "" && (r.Host == "" || hostMatchesDomain(r.Host, g.cookieDomain))

	if useCrossDomainSSO {
		epoch := time.Unix(0, 0)
		if g.legacyCookieName != "" {
			http.SetCookie(w, &http.Cookie{
				Name:     g.legacyCookieName,
				Value:    "",
				Path:     "/",
				MaxAge:   -1,
				Expires:  epoch,
				HttpOnly: true,
				Secure:   true,
				SameSite: http.SameSiteStrictMode,
			})
		}
		http.SetCookie(w, &http.Cookie{
			Name:     SessionCookieName,
			Value:    "",
			Path:     "/",
			MaxAge:   -1,
			Expires:  epoch,
			HttpOnly: true,
			Secure:   true,
			SameSite: http.SameSiteLaxMode,
		})
		http.SetCookie(w, &http.Cookie{
			Name:     SecureSessionCookieName,
			Value:    signed,
			Domain:   g.cookieDomain,
			Path:     "/",
			HttpOnly: true,
			Secure:   true,
			SameSite: http.SameSiteLaxMode,
			Expires:  exp,
		})
		return
	}

	if g.legacyCookieName != "" {
		legacyVal := signed
		if g.presenterToken != "" {
			legacyVal = g.presenterToken
		}
		http.SetCookie(w, &http.Cookie{
			Name:     g.legacyCookieName,
			Value:    legacyVal,
			Path:     "/",
			HttpOnly: true,
			Secure:   secure,
			SameSite: http.SameSiteStrictMode,
			Expires:  exp,
		})
	}

	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    signed,
		Path:     "/",
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
		Expires:  exp,
	})
}

// ClearSessionCookies expires all presenter cookies (both Google SSO and token-based) on logout.
func (g *Gate) ClearSessionCookies(w http.ResponseWriter, r *http.Request) {
	secure := isHTTPS(r)
	epoch := time.Unix(0, 0)
	if g.legacyCookieName != "" {
		http.SetCookie(w, &http.Cookie{
			Name:     g.legacyCookieName,
			Value:    "",
			Path:     "/",
			MaxAge:   -1,
			Expires:  epoch,
			HttpOnly: true,
			Secure:   secure,
			SameSite: http.SameSiteStrictMode,
		})
	}
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		Expires:  epoch,
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	})
	http.SetCookie(w, &http.Cookie{
		Name:     CSRFCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		Expires:  epoch,
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteStrictMode,
	})
	if g.cookieDomain != "" {
		http.SetCookie(w, &http.Cookie{
			Name:     SecureSessionCookieName,
			Value:    "",
			Domain:   g.cookieDomain,
			Path:     "/",
			MaxAge:   -1,
			Expires:  epoch,
			HttpOnly: true,
			Secure:   true,
			SameSite: http.SameSiteLaxMode,
		})
	}
}

// HandleUnlockOrGate processes ?key= / ?token= unlock redirects or renders the
// 401 HTML Sign-In Gate if r is unauthenticated. Returns true if it wrote a
// response (so the caller should return immediately), or false if authorized.
func (g *Gate) HandleUnlockOrGate(w http.ResponseWriter, r *http.Request, defaultRedirectPath string) bool {
	if !g.Enabled() {
		return false
	}

	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		q := r.URL.Query()
		key := strings.TrimSpace(q.Get("key"))
		if key == "" {
			key = strings.TrimSpace(q.Get("token"))
		}
		if g.presenterToken != "" && key != "" && subtle.ConstantTimeCompare([]byte(key), []byte(g.presenterToken)) == 1 {
			g.IssueSessionCookies(w, r, "presenter-key")
			q.Del("key")
			q.Del("token")
			target := defaultRedirectPath
			if target == "" {
				target = r.URL.Path
			}
			if target == "" {
				target = "/"
			}
			if encoded := q.Encode(); encoded != "" {
				target += "?" + encoded
			}
			http.Redirect(w, r, target, http.StatusSeeOther)
			return true
		}
	}

	if !g.IsAuthorized(r) {
		g.RenderLoginPage(w, r)
		return true
	}
	return false
}

// RequireAPI wraps an HTTP API handler and returns 401 JSON when unauthenticated.
func (g *Gate) RequireAPI(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !g.IsAuthorized(r) {
			writeAuthJSON(w, http.StatusUnauthorized, map[string]any{
				"error":  "unauthorized: sign in with Google or provide X-Presenter-Key",
				"status": http.StatusUnauthorized,
			})
			return
		}
		next(w, r)
	}
}

// RegisterRoutes mounts /api/auth/google, /api/auth/token, /api/auth/session,
// /api/auth/logout, and /api/auth/presenter.js on mux.
func (g *Gate) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/auth/google", g.handleGoogleAuth)
	mux.HandleFunc("/api/auth/token", g.handleTokenAuth)
	mux.HandleFunc("/api/auth/session", g.handleSession)
	mux.HandleFunc("/api/auth/logout", g.handleLogout)
	mux.HandleFunc("/api/auth/presenter.js", g.handlePresenterJS)
}

func (g *Gate) verifyDoubleSubmitCSRF(r *http.Request) bool {
	if csrfCookie, err := r.Cookie(CSRFCookieName); err == nil && csrfCookie.Value != "" {
		hdrToken := strings.TrimSpace(r.Header.Get(CSRFHeaderName))
		if hdrToken == "" || subtle.ConstantTimeCompare([]byte(hdrToken), []byte(csrfCookie.Value)) != 1 {
			return false
		}
	}
	return true
}

func (g *Gate) issueCSRFCookie(w http.ResponseWriter, r *http.Request) string {
	csrfToken := RandomToken(16)
	if existing, err := r.Cookie(CSRFCookieName); err == nil && len(existing.Value) >= 16 {
		csrfToken = existing.Value
	}
	http.SetCookie(w, &http.Cookie{
		Name:     CSRFCookieName,
		Value:    csrfToken,
		Path:     "/",
		HttpOnly: true,
		Secure:   isHTTPS(r),
		SameSite: http.SameSiteStrictMode,
		Expires:  g.now().Add(2 * time.Hour),
	})
	return csrfToken
}

func (g *Gate) clearCSRFCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     CSRFCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		Expires:  time.Unix(0, 0),
		HttpOnly: true,
		Secure:   isHTTPS(r),
		SameSite: http.SameSiteStrictMode,
	})
}

func (g *Gate) handleGoogleAuth(w http.ResponseWriter, r *http.Request) {
	setBaseSecurityHeaders(w)
	if r.Method != http.MethodPost {
		writeAuthJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if g.googleVerifier == nil {
		writeAuthJSON(w, http.StatusNotFound, map[string]any{"error": "Google Sign-In is not configured"})
		return
	}
	if !g.sameOriginRequest(r) {
		writeAuthJSON(w, http.StatusForbidden, map[string]any{"error": "cross-origin auth request rejected"})
		return
	}
	if !g.verifyDoubleSubmitCSRF(r) {
		writeAuthJSON(w, http.StatusForbidden, map[string]any{"error": "invalid or missing CSRF token"})
		return
	}

	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		writeAuthJSON(w, http.StatusUnsupportedMediaType, map[string]any{"error": "Content-Type must be application/json"})
		return
	}

	var req struct {
		IDToken string `json:"id_token"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxAuthPayloadBytes))
	if err := dec.Decode(&req); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeAuthJSON(w, http.StatusRequestEntityTooLarge, map[string]any{"error": "request body too large"})
			return
		}
		writeAuthJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json payload"})
		return
	}
	if strings.TrimSpace(req.IDToken) == "" {
		writeAuthJSON(w, http.StatusBadRequest, map[string]any{"error": "missing id_token"})
		return
	}

	email, err := g.googleVerifier.VerifyIDToken(r.Context(), req.IDToken)
	if err != nil {
		writeAuthJSON(w, http.StatusForbidden, map[string]any{"error": err.Error()})
		return
	}

	g.clearCSRFCookie(w, r)
	g.IssueSessionCookies(w, r, email)
	slog.Info("Presenter authenticated via Google Sign-In", slog.String("email", maskEmailSubject(email)), slog.String("demo", g.title))

	writeAuthJSON(w, http.StatusOK, map[string]any{
		"status": "ok",
		"email":  maskEmailSubject(email),
	})
}

func (g *Gate) handleTokenAuth(w http.ResponseWriter, r *http.Request) {
	setBaseSecurityHeaders(w)
	if r.Method != http.MethodPost {
		writeAuthJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if g.presenterToken == "" {
		writeAuthJSON(w, http.StatusNotFound, map[string]any{"error": "presenter token auth is not configured"})
		return
	}
	if !g.sameOriginRequest(r) {
		writeAuthJSON(w, http.StatusForbidden, map[string]any{"error": "cross-origin auth request rejected"})
		return
	}
	if !g.verifyDoubleSubmitCSRF(r) {
		writeAuthJSON(w, http.StatusForbidden, map[string]any{"error": "invalid or missing CSRF token"})
		return
	}
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		writeAuthJSON(w, http.StatusUnsupportedMediaType, map[string]any{"error": "Content-Type must be application/json"})
		return
	}

	var req struct {
		Key string `json:"key"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxAuthPayloadBytes))
	if err := dec.Decode(&req); err != nil {
		writeAuthJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json payload"})
		return
	}
	key := strings.TrimSpace(req.Key)
	if key == "" || subtle.ConstantTimeCompare([]byte(key), []byte(g.presenterToken)) != 1 {
		writeAuthJSON(w, http.StatusForbidden, map[string]any{"error": "invalid presenter token"})
		return
	}

	g.clearCSRFCookie(w, r)
	g.IssueSessionCookies(w, r, "presenter-key")
	writeAuthJSON(w, http.StatusOK, map[string]any{
		"status":      "ok",
		"auth_method": "token",
	})
}

func (g *Gate) handleSession(w http.ResponseWriter, r *http.Request) {
	setBaseSecurityHeaders(w)
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeAuthJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if !g.Enabled() {
		writeAuthJSON(w, http.StatusOK, map[string]any{
			"enabled":       false,
			"authenticated": true,
			"auth_method":   "none",
		})
		return
	}
	authMethod, maskedSub, ok := g.authenticatedIdentity(r)
	if !ok {
		writeAuthJSON(w, http.StatusUnauthorized, map[string]any{
			"enabled":       true,
			"authenticated": false,
		})
		return
	}
	csrfToken := g.issueCSRFCookie(w, r)
	writeAuthJSON(w, http.StatusOK, map[string]any{
		"enabled":       true,
		"authenticated": true,
		"auth_method":   authMethod,
		"subject":       maskedSub,
		"csrf_token":    csrfToken,
	})
}

func (g *Gate) handleLogout(w http.ResponseWriter, r *http.Request) {
	setBaseSecurityHeaders(w)
	if r.Method != http.MethodPost {
		writeAuthJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if !g.sameOriginRequest(r) {
		writeAuthJSON(w, http.StatusForbidden, map[string]any{"error": "cross-origin logout request rejected"})
		return
	}
	if !g.verifyDoubleSubmitCSRF(r) {
		writeAuthJSON(w, http.StatusForbidden, map[string]any{"error": "invalid or missing CSRF token"})
		return
	}
	g.ClearSessionCookies(w, r)
	writeAuthJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

func (g *Gate) handlePresenterJS(w http.ResponseWriter, r *http.Request) {
	setBaseSecurityHeaders(w)
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write([]byte(presenterLogoutJS))
}

const presenterLogoutJS = `(function () {
  'use strict';
  const btn = document.getElementById('btnLogout');
  if (!btn) return;

  let csrfToken = '';

  async function loadSession() {
    try {
      const resp = await fetch('/api/auth/session', {
        method: 'GET',
        credentials: 'same-origin',
        cache: 'no-store'
      });
      if (!resp.ok) return null;
      const data = await resp.json();
      if (data && data.enabled === false) {
        btn.style.display = 'none';
        return data;
      }
      if (data && typeof data.csrf_token === 'string') {
        csrfToken = data.csrf_token;
      }
      if (data && data.auth_method === 'google') {
        const label = data.subject ? 'Sign out (' + data.subject + ')' : 'Sign out (Google SSO)';
        btn.setAttribute('title', label);
        btn.setAttribute('aria-label', label);
      } else if (data && data.auth_method === 'token') {
        btn.setAttribute('title', 'Sign out (Presenter Token)');
        btn.setAttribute('aria-label', 'Sign out (Presenter Token)');
      }
      return data;
    } catch (_) {
      return null;
    }
  }

  loadSession();

  btn.addEventListener('click', async function () {
    btn.disabled = true;
    try {
      if (!csrfToken) {
        await loadSession();
      }
      const headers = { 'X-Requested-With': 'XMLHttpRequest' };
      if (csrfToken) {
        headers['X-CSRF-Token'] = csrfToken;
      }
      const resp = await fetch('/api/auth/logout', {
        method: 'POST',
        credentials: 'same-origin',
        headers: headers
      });
      if (!resp.ok && resp.status === 403) {
        await loadSession();
        if (csrfToken) {
          headers['X-CSRF-Token'] = csrfToken;
          await fetch('/api/auth/logout', {
            method: 'POST',
            credentials: 'same-origin',
            headers: headers
          });
        }
      }
    } catch (_) {
      // Navigate to clean path so the server re-renders the 401 Sign-In Gate.
    }
    const target = new URL(window.location.href);
    target.searchParams.delete('key');
    target.searchParams.delete('token');
    window.location.replace(target.pathname + target.search + target.hash);
  });
})();
`

// RenderLoginPage writes the HTTP 401 Sign-In Gate HTML response with CSP nonces,
// Google Workspace / event domain chips, Moon/Sun Projector Light Mode toggle,
// and an expandable PRESENTER_TOKEN fallback form.
func (g *Gate) RenderLoginPage(w http.ResponseWriter, r *http.Request) {
	nonce := RandomToken(16)
	csrfToken := RandomToken(16)
	if existing, err := r.Cookie(CSRFCookieName); err == nil && len(existing.Value) >= 16 {
		csrfToken = existing.Value
	}

	http.SetCookie(w, &http.Cookie{
		Name:     CSRFCookieName,
		Value:    csrfToken,
		Path:     "/",
		HttpOnly: true,
		Secure:   isHTTPS(r),
		SameSite: http.SameSiteStrictMode,
		Expires:  g.now().Add(2 * time.Hour),
	})

	csp := fmt.Sprintf(
		"default-src 'self'; "+
			"script-src 'self' 'nonce-%s' https://www.gstatic.com https://accounts.google.com https://apis.google.com; "+
			"style-src 'self' 'nonce-%s' https://accounts.google.com; "+
			"frame-src https://accounts.google.com https://*.firebaseapp.com; "+
			"connect-src 'self' https://identitytoolkit.googleapis.com https://securetoken.googleapis.com https://www.googleapis.com https://accounts.google.com; "+
			"img-src 'self' data: https://*.googleusercontent.com https://www.gstatic.com; "+
			"frame-ancestors 'none'; base-uri 'self'; form-action 'self'",
		nonce, nonce,
	)

	setBaseSecurityHeaders(w)
	w.Header().Set("Content-Security-Policy", csp)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusUnauthorized)
	if r.Method == http.MethodHead {
		return
	}

	authDomain := g.googleCfg.FirebaseAuthDomain
	if authDomain == "" && g.googleCfg.FirebaseProjectID != "" {
		authDomain = g.googleCfg.FirebaseProjectID + ".firebaseapp.com"
	}

	cfgJSON, _ := json.Marshal(map[string]string{
		"clientId":           g.googleCfg.ClientID,
		"firebaseApiKey":     g.googleCfg.FirebaseAPIKey,
		"firebaseAuthDomain": authDomain,
		"csrfToken":          csrfToken,
	})

	heading := "Presenter Key Required"
	if g.GoogleEnabled() {
		heading = "Presenter Sign-In Required"
	}

	actionPath := r.URL.Path
	if actionPath == "" {
		actionPath = "/"
	}

	_ = signInPageTmpl.Execute(w, signInPageData{
		Nonce:          nonce,
		Title:          g.title,
		Heading:        heading,
		Subtitle:       g.subtitle,
		SecretName:     g.secretName,
		ActionPath:     actionPath,
		GoogleEnabled:  g.GoogleEnabled(),
		HasToken:       g.presenterToken != "",
		CookieDomain:   g.cookieDomain,
		AllowedDomains: g.googleCfg.AllowedDomains,
		ClientID:       g.googleCfg.ClientID,
		FirebaseAPIKey: g.googleCfg.FirebaseAPIKey,
		ConfigJSON:     template.JS(cfgJSON),
	})
}

type signInPageData struct {
	Nonce          string
	Title          string
	Heading        string
	Subtitle       string
	SecretName     string
	ActionPath     string
	GoogleEnabled  bool
	HasToken       bool
	CookieDomain   string
	AllowedDomains []string
	ClientID       string
	FirebaseAPIKey string
	ConfigJSON     template.JS
}

func setBaseSecurityHeaders(w http.ResponseWriter) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
	w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate, max-age=0")
}

func writeAuthJSON(w http.ResponseWriter, status int, payload any) {
	setBaseSecurityHeaders(w)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(payload)
}

func isHTTPS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	proto := strings.TrimSpace(r.Header.Get("X-Forwarded-Proto"))
	if comma := strings.IndexByte(proto, ','); comma >= 0 {
		proto = strings.TrimSpace(proto[:comma])
	}
	return strings.EqualFold(proto, "https")
}

func isStateChangingMethod(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return false
	default:
		return true
	}
}

func stripPort(hostport string) string {
	hostport = strings.TrimSpace(hostport)
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		return strings.ToLower(h)
	}
	return strings.ToLower(hostport)
}

func hostMatchesDomain(host, domain string) bool {
	h := stripPort(host)
	d := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(domain), "."))
	if h == "" || d == "" {
		return false
	}
	return h == d || strings.HasSuffix(h, "."+d)
}

func (g *Gate) sameOriginRequest(r *http.Request) bool {
	if strings.EqualFold(strings.TrimSpace(r.Header.Get("Sec-Fetch-Site")), "cross-site") {
		return false
	}
	reqHost := stripPort(r.Host)
	if origin := strings.TrimSpace(r.Header.Get("Origin")); origin != "" {
		if origin == "null" {
			return false
		}
		u, err := url.Parse(origin)
		if err != nil || u.Host == "" {
			return false
		}
		originHost := stripPort(u.Host)
		if reqHost != "" && strings.EqualFold(originHost, reqHost) {
			return true
		}
		if g.cookieDomain != "" && hostMatchesDomain(originHost, g.cookieDomain) && (reqHost == "" || hostMatchesDomain(reqHost, g.cookieDomain)) {
			return true
		}
		return false
	}
	if ref := strings.TrimSpace(r.Header.Get("Referer")); ref != "" && reqHost != "" {
		u, err := url.Parse(ref)
		if err != nil || u.Host == "" {
			return false
		}
		refHost := stripPort(u.Host)
		if strings.EqualFold(refHost, reqHost) {
			return true
		}
		if g.cookieDomain != "" && hostMatchesDomain(refHost, g.cookieDomain) && hostMatchesDomain(reqHost, g.cookieDomain) {
			return true
		}
		return false
	}
	return true
}

var signInPageTmpl = template.Must(template.New("aba-signin").Parse(`<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8" />
  <meta name="viewport" content="width=device-width, initial-scale=1.0" />
  <title>{{.Heading}} — {{.Title}}</title>
  <style nonce="{{.Nonce}}">
    :root {
      --bg: #070b14;
      --bg-glow: rgba(56, 189, 248, 0.08);
      --card: #0f172a;
      --border: #1e293b;
      --text: #f8fafc;
      --muted: #94a3b8;
      --eyebrow: #38bdf8;
      --chip-bg: #162033;
      --chip-border: #334155;
      --chip-text: #e2e8f0;
      --input-bg: #070b14;
      --btn-google-bg: #ffffff;
      --btn-google-text: #0f172a;
      --btn-google-border: #cbd5e1;
      --btn-google-hover: #f1f5f9;
      --accent-bg: rgba(16, 185, 129, 0.16);
      --accent-border: #10b981;
      --accent-text: #34d399;
      --danger-bg: rgba(239, 68, 68, 0.12);
      --danger-border: rgba(239, 68, 68, 0.4);
      --danger-text: #fca5a5;
      --toggle-bg: #0f172a;
      --toggle-active-bg: #1e293b;
      --toggle-icon: #94a3b8;
      --toggle-active-icon: #38bdf8;
      --shadow: 0 24px 60px rgba(0, 0, 0, 0.6);
    }
    [data-theme="light"] {
      --bg: #f1f5f9;
      --bg-glow: rgba(2, 132, 199, 0.08);
      --card: #ffffff;
      --border: #cbd5e1;
      --text: #0f172a;
      --muted: #475569;
      --eyebrow: #0284c7;
      --chip-bg: #f8fafc;
      --chip-border: #cbd5e1;
      --chip-text: #0f172a;
      --input-bg: #f8fafc;
      --btn-google-bg: #0f172a;
      --btn-google-text: #ffffff;
      --btn-google-border: #0f172a;
      --btn-google-hover: #1e293b;
      --accent-bg: rgba(5, 150, 105, 0.12);
      --accent-border: #059669;
      --accent-text: #047857;
      --danger-bg: rgba(220, 38, 38, 0.08);
      --danger-border: rgba(220, 38, 38, 0.35);
      --danger-text: #b91c1c;
      --toggle-bg: #ffffff;
      --toggle-active-bg: #e2e8f0;
      --toggle-icon: #64748b;
      --toggle-active-icon: #0284c7;
      --shadow: 0 20px 44px rgba(15, 23, 42, 0.12);
    }
    * { box-sizing: border-box; }
    body {
      margin: 0;
      min-height: 100vh;
      display: flex;
      align-items: center;
      justify-content: center;
      padding: 24px;
      background: radial-gradient(circle at 50% 15%, var(--bg-glow) 0%, var(--bg) 70%);
      color: var(--text);
      font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif;
    }
    .theme-toggle {
      position: fixed;
      top: 16px;
      right: 16px;
      display: inline-flex;
      align-items: center;
      padding: 3px;
      border-radius: 999px;
      background: var(--toggle-bg);
      border: 1px solid var(--border);
      z-index: 20;
    }
    .theme-btn {
      width: 30px;
      height: 30px;
      padding: 0;
      border: 0;
      border-radius: 999px;
      background: transparent;
      color: var(--toggle-icon);
      display: inline-flex;
      align-items: center;
      justify-content: center;
      cursor: pointer;
    }
    .theme-btn.active {
      background: var(--toggle-active-bg);
      color: var(--toggle-active-icon);
    }
    .auth-card {
      width: 100%;
      max-width: 460px;
      background: var(--card);
      border: 1px solid var(--border);
      border-top: 3px solid var(--eyebrow);
      border-radius: 14px;
      padding: 28px;
      box-shadow: var(--shadow);
    }
    .eyebrow {
      font-family: ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, monospace;
      font-size: 11px;
      font-weight: 700;
      letter-spacing: 0.08em;
      text-transform: uppercase;
      color: var(--eyebrow);
      margin-bottom: 10px;
    }
    .brand-row {
      display: flex;
      align-items: center;
      gap: 12px;
      margin-bottom: 12px;
    }
    .brand-icon {
      width: 38px;
      height: 38px;
      border-radius: 9px;
      background: var(--chip-bg);
      border: 1px solid var(--chip-border);
      display: flex;
      align-items: center;
      justify-content: center;
      color: var(--eyebrow);
      flex-shrink: 0;
    }
    h1 {
      font-size: 20px;
      font-weight: 700;
      margin: 0;
    }
    .demo-title {
      font-size: 12px;
      color: var(--muted);
      margin-top: 2px;
    }
    .subtitle {
      color: var(--muted);
      font-size: 13.5px;
      line-height: 1.55;
      margin: 0 0 18px 0;
    }
    .domains-label {
      font-size: 11px;
      font-weight: 600;
      text-transform: uppercase;
      letter-spacing: 0.06em;
      color: var(--muted);
      margin-bottom: 8px;
    }
    .domain-chips {
      display: flex;
      flex-wrap: wrap;
      gap: 6px;
      margin-bottom: 18px;
    }
    .domain-chip {
      font-family: ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, monospace;
      font-size: 12px;
      padding: 4px 9px;
      border-radius: 6px;
      background: var(--chip-bg);
      border: 1px solid var(--chip-border);
      color: var(--chip-text);
    }
    .sso-note {
      font-size: 12px;
      color: var(--muted);
      margin: -8px 0 16px 0;
    }
    .google-btn {
      width: 100%;
      display: inline-flex;
      align-items: center;
      justify-content: center;
      gap: 10px;
      padding: 12px 16px;
      border-radius: 8px;
      border: 1px solid var(--btn-google-border);
      background: var(--btn-google-bg);
      color: var(--btn-google-text);
      font-size: 14.5px;
      font-weight: 600;
      cursor: pointer;
    }
    .google-btn:hover { background: var(--btn-google-hover); }
    .google-btn:disabled { opacity: 0.65; cursor: wait; }
    #gsi-container {
      display: flex;
      justify-content: center;
      margin-top: 6px;
    }
    .error-box {
      display: none;
      margin-top: 14px;
      padding: 10px 12px;
      border-radius: 8px;
      background: var(--danger-bg);
      border: 1px solid var(--danger-border);
      color: var(--danger-text);
      font-size: 12.5px;
      line-height: 1.45;
    }
    details.key-fallback {
      margin-top: 20px;
      padding-top: 14px;
      border-top: 1px solid var(--border);
      font-size: 12.5px;
      color: var(--muted);
    }
    details.key-fallback summary {
      cursor: pointer;
      user-select: none;
      font-weight: 500;
    }
    .key-form {
      display: flex;
      gap: 8px;
      margin-top: 10px;
    }
    .key-input {
      flex: 1;
      padding: 9px 11px;
      border-radius: 6px;
      border: 1px solid var(--chip-border);
      background: var(--input-bg);
      color: var(--text);
      font-family: ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, monospace;
      font-size: 12.5px;
    }
    .key-submit {
      padding: 9px 15px;
      border-radius: 6px;
      border: 1px solid var(--accent-border);
      background: var(--accent-bg);
      color: var(--accent-text);
      font-size: 12.5px;
      font-weight: 700;
      cursor: pointer;
    }
    .cli-hint {
      margin: 12px 0 0 0;
      padding: 9px 11px;
      border-radius: 6px;
      background: var(--input-bg);
      border: 1px solid var(--border);
      color: var(--eyebrow);
      font-family: ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, monospace;
      font-size: 11px;
      overflow-x: auto;
    }
  </style>
</head>
<body>
  <div class="theme-toggle" id="btnThemeToggle" role="group" aria-label="Color theme">
    <button type="button" class="theme-btn active" id="theme-dark" aria-label="Dark mode" title="Dark mode">
      <svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
        <path d="M12 3a6 6 0 0 0 9 9 9 9 0 1 1-9-9Z"></path>
      </svg>
    </button>
    <button type="button" class="theme-btn" id="theme-light" aria-label="Projector light mode" title="Projector light mode">
      <svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
        <circle cx="12" cy="12" r="4"></circle>
        <path d="M12 2v2"></path><path d="M12 20v2"></path>
        <path d="m4.93 4.93 1.41 1.41"></path><path d="m17.66 17.66 1.41 1.41"></path>
        <path d="M2 12h2"></path><path d="M20 12h2"></path>
        <path d="m6.34 17.66-1.41 1.41"></path><path d="m19.07 4.93-1.41 1.41"></path>
      </svg>
    </button>
  </div>

  <main class="auth-card">
    <div class="eyebrow">AGENT SUBSTRATE // PRESENTER SECURITY GATE</div>
    <div class="brand-row">
      <div class="brand-icon" aria-hidden="true">
        <svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
          <rect x="3" y="11" width="18" height="11" rx="2" ry="2"></rect>
          <path d="M7 11V7a5 5 0 0 1 10 0v4"></path>
        </svg>
      </div>
      <div>
        <h1>{{.Heading}}</h1>
        <div class="demo-title">{{.Title}}</div>
      </div>
    </div>

    <p class="subtitle">{{.Subtitle}}</p>

    {{- if .GoogleEnabled}}
    <div class="domains-label">Allowed Google Domains</div>
    <div class="domain-chips">
      {{- range .AllowedDomains}}
      <span class="domain-chip">@{{.}}</span>
      {{- end}}
    </div>
    {{- if .CookieDomain}}
    <p class="sso-note">Signing in unlocks all <code>*.{{.CookieDomain}}</code> presenter consoles.</p>
    {{- end}}

    {{- if .FirebaseAPIKey}}
    <button id="btn-google-signin" type="button" class="google-btn">
      <svg width="18" height="18" viewBox="0 0 48 48" aria-hidden="true">
        <path fill="#EA4335" d="M24 9.5c3.54 0 6.71 1.22 9.21 3.6l6.85-6.85C35.9 2.38 30.47 0 24 0 14.62 0 6.51 5.38 2.56 13.22l7.98 6.19C12.43 13.72 17.74 9.5 24 9.5z"/>
        <path fill="#4285F4" d="M46.98 24.55c0-1.57-.15-3.09-.38-4.55H24v9.02h12.94c-.58 2.96-2.26 5.48-4.78 7.18l7.73 6c4.51-4.18 7.09-10.36 7.09-17.65z"/>
        <path fill="#FBBC05" d="M10.53 28.59c-.48-1.45-.76-2.99-.76-4.59s.27-3.14.76-4.59l-7.98-6.19C.92 16.46 0 20.12 0 24c0 3.88.92 7.54 2.56 10.78l7.97-6.19z"/>
        <path fill="#34A853" d="M24 48c6.48 0 11.93-2.13 15.89-5.81l-7.73-6c-2.15 1.45-4.92 2.3-8.16 2.3-6.26 0-11.57-4.22-13.47-9.91l-7.98 6.19C6.51 42.62 14.62 48 24 48z"/>
      </svg>
      <span>Sign in with Google</span>
    </button>
    {{- else if .ClientID}}
    <div id="gsi-container"></div>
    {{- end}}

    <div id="auth-error" class="error-box" role="alert"></div>

    {{- if .HasToken}}
    <details class="key-fallback">
      <summary>Use presenter token instead (CLI / port-forward)</summary>
      <form class="key-form" method="GET" action="{{.ActionPath}}">
        <input class="key-input" type="password" name="key" placeholder="PRESENTER_TOKEN" required autocomplete="off" />
        <button class="key-submit" type="submit">Unlock</button>
      </form>
      <pre class="cli-hint">kubectl get secret {{.SecretName}} -n anything-but-agents -o jsonpath='{.data.PRESENTER_TOKEN}' | base64 -d</pre>
    </details>
    {{- end}}

    {{- else}}
    <form class="key-form" method="GET" action="{{.ActionPath}}">
      <input class="key-input" type="password" name="key" placeholder="Paste PRESENTER_TOKEN..." required autocomplete="off" />
      <button class="key-submit" type="submit">Unlock</button>
    </form>
    <pre class="cli-hint">kubectl get secret {{.SecretName}} -n anything-but-agents -o jsonpath='{.data.PRESENTER_TOKEN}' | base64 -d</pre>
    {{- end}}
  </main>

  <script type="module" nonce="{{.Nonce}}">
    const root = document.documentElement;
    const darkBtn = document.getElementById('theme-dark');
    const lightBtn = document.getElementById('theme-light');
    function applyTheme(t) {
      if (t === 'light') {
        root.setAttribute('data-theme', 'light');
        lightBtn.classList.add('active');
        darkBtn.classList.remove('active');
      } else {
        root.removeAttribute('data-theme');
        darkBtn.classList.add('active');
        lightBtn.classList.remove('active');
      }
      try { localStorage.setItem('aba_theme', t); } catch (_) {}
    }
    try {
      const saved = localStorage.getItem('aba_theme');
      if (saved === 'light' || saved === 'dark') applyTheme(saved);
    } catch (_) {}
    darkBtn.addEventListener('click', () => applyTheme('dark'));
    lightBtn.addEventListener('click', () => applyTheme('light'));

    const cfg = {{.ConfigJSON}};
    const errorBox = document.getElementById('auth-error');
    function showError(msg) {
      if (!errorBox) return;
      errorBox.textContent = msg;
      errorBox.style.display = 'block';
    }
    async function exchangeToken(idToken) {
      if (errorBox) errorBox.style.display = 'none';
      const resp = await fetch('/api/auth/google', {
        method: 'POST',
        headers: {
          'Content-Type': 'application/json',
          'X-CSRF-Token': cfg.csrfToken || ''
        },
        body: JSON.stringify({ id_token: idToken })
      });
      if (!resp.ok) {
        let detail = '';
        try {
          const parsed = await resp.json();
          detail = parsed && parsed.error ? parsed.error : '';
        } catch (_) {
          detail = await resp.text();
        }
        throw new Error((detail || '').trim() || ('Authentication failed (HTTP ' + resp.status + ')'));
      }
      const cleanTarget = new URL(window.location.href);
      cleanTarget.searchParams.delete('key');
      cleanTarget.searchParams.delete('token');
      window.location.replace(cleanTarget.pathname + cleanTarget.search + cleanTarget.hash);
    }

    document.querySelectorAll('.key-form').forEach((form) => {
      form.addEventListener('submit', async (ev) => {
        ev.preventDefault();
        if (errorBox) errorBox.style.display = 'none';
        const input = form.querySelector('input[name="key"]');
        const submitBtn = form.querySelector('button[type="submit"]');
        const keyVal = input ? input.value.trim() : '';
        if (!keyVal) return;
        if (submitBtn) submitBtn.disabled = true;
        try {
          const resp = await fetch('/api/auth/token', {
            method: 'POST',
            headers: {
              'Content-Type': 'application/json',
              'X-CSRF-Token': cfg.csrfToken || ''
            },
            body: JSON.stringify({ key: keyVal })
          });
          if (!resp.ok) {
            let detail = '';
            try {
              const parsed = await resp.json();
              detail = parsed && parsed.error ? parsed.error : '';
            } catch (_) {
              detail = await resp.text();
            }
            throw new Error((detail || '').trim() || ('Invalid presenter token (HTTP ' + resp.status + ')'));
          }
          const cleanTarget = new URL(window.location.href);
          cleanTarget.searchParams.delete('key');
          cleanTarget.searchParams.delete('token');
          window.location.replace(cleanTarget.pathname + cleanTarget.search + cleanTarget.hash);
        } catch (err) {
          showError(err && err.message ? err.message : String(err));
          if (submitBtn) submitBtn.disabled = false;
        }
      });
    });

    if (cfg.firebaseApiKey) {
      const btn = document.getElementById('btn-google-signin');
      if (btn) {
        btn.addEventListener('click', async () => {
          btn.disabled = true;
          try {
            const [{ getApp, getApps, initializeApp }, { getAuth, GoogleAuthProvider, signInWithPopup, signOut }] = await Promise.all([
              import('https://www.gstatic.com/firebasejs/11.0.1/firebase-app.js'),
              import('https://www.gstatic.com/firebasejs/11.0.1/firebase-auth.js')
            ]);
            const app = getApps().length ? getApp() : initializeApp({
              apiKey: cfg.firebaseApiKey,
              authDomain: cfg.firebaseAuthDomain
            });
            const auth = getAuth(app);
            const provider = new GoogleAuthProvider();
            provider.setCustomParameters({ prompt: 'select_account' });
            const result = await signInWithPopup(auth, provider);
            const idToken = await result.user.getIdToken();
            await exchangeToken(idToken);
            await signOut(auth).catch(() => {});
          } catch (err) {
            showError(err && err.message ? err.message : String(err));
            btn.disabled = false;
          }
        });
      }
    } else if (cfg.clientId) {
      const s = document.createElement('script');
      s.src = 'https://accounts.google.com/gsi/client';
      s.async = true;
      s.onload = () => {
        window.google.accounts.id.initialize({
          client_id: cfg.clientId,
          callback: async (response) => {
            try {
              await exchangeToken(response.credential);
            } catch (err) {
              showError(err && err.message ? err.message : String(err));
            }
          }
        });
        const container = document.getElementById('gsi-container');
        if (container) {
          window.google.accounts.id.renderButton(
            container,
            { theme: 'outline', size: 'large', width: 360, text: 'signin_with' }
          );
        }
      };
      document.head.appendChild(s);
    }
  </script>
</body>
</html>
`))
