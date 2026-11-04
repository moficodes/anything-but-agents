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

// Package presenterauth provides Google Workspace / event-domain OIDC and
// Firebase Auth sign-in, cross-subdomain HMAC session cookies, and a
// PRESENTER_TOKEN fallback gate using only the Go standard library.
package presenterauth

import (
	"cmp"
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	defaultGoogleJWKSURL   = "https://www.googleapis.com/oauth2/v3/certs"
	defaultFirebaseJWKSURL = "https://www.googleapis.com/service_accounts/v1/jwk/securetoken@system.gserviceaccount.com"
	jwksCacheTTL           = time.Hour
	jwksRefreshCooldown    = 30 * time.Second
	maxJWKSBytes           = 256 << 10
)

// GoogleAuthConfig configures Google Sign-In / OIDC presenter authentication.
type GoogleAuthConfig struct {
	// AllowedDomains lists Google Workspace / event domains whose verified
	// users may unlock presenter pages (for example: "google.com",
	// "event.goog", "cloud-demos.goog", "gcdemos.goog"). Empty disables
	// Google Sign-In.
	AllowedDomains []string
	// ClientID is the Google OAuth 2.0 Web Client ID for Google Identity
	// Services (GIS) or OIDC tokens (iss: https://accounts.google.com).
	ClientID string
	// FirebaseAPIKey and FirebaseAuthDomain configure Firebase Auth popup
	// sign-in when using Identity Platform / Firebase Authentication.
	FirebaseAPIKey     string
	FirebaseAuthDomain string
	// FirebaseProjectID is the GCP project ID for verifying Firebase Auth
	// ID tokens (iss: https://securetoken.google.com/<project>).
	FirebaseProjectID string
	// HTTPClient fetches JWK sets; nil uses a 5-second timeout client.
	HTTPClient *http.Client
	// GoogleJWKSURL and FirebaseJWKSURL override the default JWK endpoints in tests.
	GoogleJWKSURL   string
	FirebaseJWKSURL string
	// Now overrides time.Now in tests.
	Now func() time.Time
}

// ConfigFromEnv reads GoogleAuthConfig from standard environment variables.
func ConfigFromEnv() GoogleAuthConfig {
	var domains []string
	if raw := strings.TrimSpace(os.Getenv("ALLOWED_GOOGLE_DOMAINS")); raw != "" {
		domains = NormalizeDomains([]string{raw})
	}
	projectID := cmp.Or(
		strings.TrimSpace(os.Getenv("FIREBASE_PROJECT_ID")),
		strings.TrimSpace(os.Getenv("GOOGLE_CLOUD_PROJECT")),
		strings.TrimSpace(os.Getenv("PROJECT_ID")),
	)
	return GoogleAuthConfig{
		AllowedDomains:     domains,
		ClientID:           strings.TrimSpace(os.Getenv("GOOGLE_CLIENT_ID")),
		FirebaseAPIKey:     strings.TrimSpace(os.Getenv("FIREBASE_API_KEY")),
		FirebaseAuthDomain: strings.TrimSpace(os.Getenv("FIREBASE_AUTH_DOMAIN")),
		FirebaseProjectID:  projectID,
	}
}

// MergeEnv fills unset fields in cfg from ConfigFromEnv().
func MergeEnv(cfg GoogleAuthConfig) GoogleAuthConfig {
	envCfg := ConfigFromEnv()
	if len(cfg.AllowedDomains) == 0 {
		cfg.AllowedDomains = envCfg.AllowedDomains
	} else {
		cfg.AllowedDomains = NormalizeDomains(cfg.AllowedDomains)
	}
	cfg.ClientID = cmp.Or(strings.TrimSpace(cfg.ClientID), envCfg.ClientID)
	cfg.FirebaseAPIKey = cmp.Or(strings.TrimSpace(cfg.FirebaseAPIKey), envCfg.FirebaseAPIKey)
	cfg.FirebaseAuthDomain = cmp.Or(strings.TrimSpace(cfg.FirebaseAuthDomain), envCfg.FirebaseAuthDomain)
	cfg.FirebaseProjectID = cmp.Or(strings.TrimSpace(cfg.FirebaseProjectID), envCfg.FirebaseProjectID)
	return cfg
}

// NormalizeDomains returns a deduplicated slice of lowercase domain names with
// leading "@" and whitespace stripped.
func NormalizeDomains(domains []string) []string {
	var out []string
	for _, raw := range domains {
		for _, part := range strings.Split(raw, ",") {
			d := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(part), "@"))
			if d != "" && !slices.Contains(out, d) {
				out = append(out, d)
			}
		}
	}
	return out
}

// Enabled reports whether Google Sign-In is configured with at least one
// allowed domain and a client ID or Firebase project/API key.
func (c GoogleAuthConfig) Enabled() bool {
	if len(NormalizeDomains(c.AllowedDomains)) == 0 {
		return false
	}
	return strings.TrimSpace(c.ClientID) != "" ||
		strings.TrimSpace(c.FirebaseProjectID) != "" ||
		strings.TrimSpace(c.FirebaseAPIKey) != ""
}

// GoogleVerifier verifies RS256 Google OIDC and Firebase Auth ID tokens and
// checks that the authenticated email belongs to an allowed Workspace domain.
type GoogleVerifier struct {
	cfg          GoogleAuthConfig
	googleKeys   *jwkCache
	firebaseKeys *jwkCache
	now          func() time.Time
}

// NewGoogleVerifier constructs a GoogleVerifier from cfg, or returns nil when
// Google Sign-In is not enabled.
func NewGoogleVerifier(cfg GoogleAuthConfig) *GoogleVerifier {
	cfg.AllowedDomains = NormalizeDomains(cfg.AllowedDomains)
	cfg.ClientID = strings.TrimSpace(cfg.ClientID)
	cfg.FirebaseAPIKey = strings.TrimSpace(cfg.FirebaseAPIKey)
	cfg.FirebaseAuthDomain = strings.TrimSpace(cfg.FirebaseAuthDomain)
	cfg.FirebaseProjectID = strings.TrimSpace(cfg.FirebaseProjectID)
	if cfg.FirebaseAuthDomain == "" && cfg.FirebaseProjectID != "" {
		cfg.FirebaseAuthDomain = cfg.FirebaseProjectID + ".firebaseapp.com"
	}
	if !cfg.Enabled() {
		return nil
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	nowFn := cfg.Now
	if nowFn == nil {
		nowFn = time.Now
	}
	googleURL := cmp.Or(cfg.GoogleJWKSURL, defaultGoogleJWKSURL)
	firebaseURL := cmp.Or(cfg.FirebaseJWKSURL, defaultFirebaseJWKSURL)
	return &GoogleVerifier{
		cfg:          cfg,
		googleKeys:   newJWKCache(googleURL, client, nowFn),
		firebaseKeys: newJWKCache(firebaseURL, client, nowFn),
		now:          nowFn,
	}
}

// Config returns the normalized configuration.
func (v *GoogleVerifier) Config() GoogleAuthConfig {
	if v == nil {
		return GoogleAuthConfig{}
	}
	return v.cfg
}

type jwtHeader struct {
	Alg string `json:"alg"`
	Kid string `json:"kid"`
}

type googleIDTokenClaims struct {
	Iss           string `json:"iss"`
	Aud           any    `json:"aud"`
	Email         string `json:"email"`
	EmailVerified bool   `json:"email_verified"`
	Hd            string `json:"hd"`
	Exp           int64  `json:"exp"`
	Iat           int64  `json:"iat"`
}

// VerifyIDToken validates an RS256 Google OIDC or Firebase Auth ID token and
// returns the verified email address if its domain is in AllowedDomains.
func (v *GoogleVerifier) VerifyIDToken(ctx context.Context, idToken string) (string, error) {
	if v == nil {
		return "", errors.New("google auth is not configured")
	}
	parts := strings.Split(strings.TrimSpace(idToken), ".")
	if len(parts) != 3 {
		return "", errors.New("invalid id token format")
	}

	hdrBytes, err := decodeBase64URL(parts[0])
	if err != nil {
		return "", fmt.Errorf("invalid id token header: %w", err)
	}
	var hdr jwtHeader
	if err := json.Unmarshal(hdrBytes, &hdr); err != nil {
		return "", fmt.Errorf("invalid id token header json: %w", err)
	}
	if hdr.Alg != "RS256" {
		return "", fmt.Errorf("unsupported token algorithm %q", hdr.Alg)
	}
	if hdr.Kid == "" {
		return "", errors.New("id token missing key id (kid)")
	}

	payloadBytes, err := decodeBase64URL(parts[1])
	if err != nil {
		return "", fmt.Errorf("invalid id token payload: %w", err)
	}
	var claims googleIDTokenClaims
	if err := json.Unmarshal(payloadBytes, &claims); err != nil {
		return "", fmt.Errorf("invalid id token claims: %w", err)
	}

	now := v.now()
	if claims.Exp <= now.Unix() {
		return "", errors.New("id token has expired")
	}
	if claims.Iat > now.Add(5*time.Minute).Unix() {
		return "", errors.New("id token issued in the future")
	}

	var keys *jwkCache
	switch {
	case claims.Iss == "https://accounts.google.com" || claims.Iss == "accounts.google.com":
		if v.cfg.ClientID == "" {
			return "", errors.New("google OIDC client ID is not configured")
		}
		if !audienceMatches(claims.Aud, v.cfg.ClientID) {
			return "", errors.New("id token audience mismatch")
		}
		keys = v.googleKeys
	case v.cfg.FirebaseProjectID != "" && claims.Iss == "https://securetoken.google.com/"+v.cfg.FirebaseProjectID:
		if !audienceMatches(claims.Aud, v.cfg.FirebaseProjectID) {
			return "", errors.New("firebase id token audience mismatch")
		}
		keys = v.firebaseKeys
	default:
		return "", fmt.Errorf("unsupported token issuer %q", claims.Iss)
	}

	pubKey, err := keys.key(ctx, hdr.Kid)
	if err != nil {
		return "", fmt.Errorf("verifying signing key: %w", err)
	}

	sig, err := decodeBase64URL(parts[2])
	if err != nil {
		return "", fmt.Errorf("invalid id token signature encoding: %w", err)
	}
	signingInput := parts[0] + "." + parts[1]
	digest := sha256.Sum256([]byte(signingInput))
	if err := rsa.VerifyPKCS1v15(pubKey, crypto.SHA256, digest[:], sig); err != nil {
		return "", errors.New("invalid id token signature")
	}

	if !claims.EmailVerified {
		return "", errors.New("google account email is not verified")
	}
	email := strings.ToLower(strings.TrimSpace(claims.Email))
	at := strings.LastIndexByte(email, '@')
	if at <= 0 || at == len(email)-1 {
		return "", errors.New("id token missing valid email claim")
	}
	domain := email[at+1:]
	if claims.Hd != "" && !strings.EqualFold(claims.Hd, domain) {
		return "", fmt.Errorf("hosted domain %q does not match email domain %q", claims.Hd, domain)
	}
	if !slices.Contains(v.cfg.AllowedDomains, domain) {
		return "", fmt.Errorf("email domain @%s is not in allowed domains (%s)", domain, strings.Join(v.cfg.AllowedDomains, ", "))
	}

	return email, nil
}

func audienceMatches(aud any, want string) bool {
	switch v := aud.(type) {
	case string:
		return v == want
	case []any:
		for _, item := range v {
			if s, ok := item.(string); ok && s == want {
				return true
			}
		}
	}
	return false
}

func decodeBase64URL(s string) ([]byte, error) {
	if b, err := base64.RawURLEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	return base64.URLEncoding.DecodeString(s)
}

type jwkSet struct {
	Keys []jwkKey `json:"keys"`
}

type jwkKey struct {
	Kty string `json:"kty"`
	Alg string `json:"alg"`
	Use string `json:"use"`
	Kid string `json:"kid"`
	N   string `json:"n"`
	E   string `json:"e"`
}

type jwkCache struct {
	url       string
	client    *http.Client
	now       func() time.Time
	mu        sync.Mutex
	keys      map[string]*rsa.PublicKey
	fetchedAt time.Time
}

func newJWKCache(url string, client *http.Client, now func() time.Time) *jwkCache {
	return &jwkCache{
		url:    url,
		client: client,
		now:    now,
		keys:   make(map[string]*rsa.PublicKey),
	}
}

func (c *jwkCache) key(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := c.now()
	if len(c.keys) > 0 && now.Sub(c.fetchedAt) < jwksCacheTTL {
		if k, ok := c.keys[kid]; ok {
			return k, nil
		}
		if now.Sub(c.fetchedAt) < jwksRefreshCooldown {
			return nil, fmt.Errorf("unknown key id %q", kid)
		}
	}

	if err := c.refreshLocked(ctx, now); err != nil {
		return nil, err
	}
	k, ok := c.keys[kid]
	if !ok {
		return nil, fmt.Errorf("unknown key id %q", kid)
	}
	return k, nil
}

func (c *jwkCache) refreshLocked(ctx context.Context, now time.Time) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url, nil)
	if err != nil {
		return err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("fetching JWKs: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("JWK endpoint returned HTTP %d", resp.StatusCode)
	}
	var set jwkSet
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxJWKSBytes)).Decode(&set); err != nil {
		return fmt.Errorf("decoding JWKs: %w", err)
	}
	parsed := make(map[string]*rsa.PublicKey, len(set.Keys))
	for _, k := range set.Keys {
		if k.Kty != "RSA" || k.Kid == "" || k.N == "" || k.E == "" {
			continue
		}
		nBytes, err := decodeBase64URL(k.N)
		if err != nil {
			continue
		}
		eBytes, err := decodeBase64URL(k.E)
		if err != nil {
			continue
		}
		eInt := new(big.Int).SetBytes(eBytes)
		if !eInt.IsInt64() || eInt.Int64() <= 1 {
			continue
		}
		parsed[k.Kid] = &rsa.PublicKey{
			N: new(big.Int).SetBytes(nBytes),
			E: int(eInt.Int64()),
		}
	}
	if len(parsed) == 0 {
		return errors.New("no valid RSA keys in JWK set")
	}
	c.keys = parsed
	c.fetchedAt = now
	return nil
}
