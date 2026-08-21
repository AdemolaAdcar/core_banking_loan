package auth

import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// tokenClaims is the wire shape of the RS256 access tokens the internal
// OAuth2 authorization server (gl-posting-engine.yaml's serviceAuth.tokenUrl)
// issues. Scope is the standard OAuth2 space-delimited "scope" claim;
// Scp is accepted as a fallback array form some identity providers use
// instead — a token is read from whichever of the two is present.
type tokenClaims struct {
	jwt.RegisteredClaims
	Scope string   `json:"scope,omitempty"`
	Scp   []string `json:"scp,omitempty"`
}

func (c tokenClaims) scopes() []string {
	if c.Scope != "" {
		return strings.Fields(c.Scope)
	}
	return c.Scp
}

// JWKSValidator validates RS256-signed access tokens against a JSON Web
// Key Set fetched from the issuing authorization server, entirely
// locally — it never calls back to the auth server per request (that
// would make every gl-posting-engine.yaml call depend on the auth server's
// availability and latency). Keys are cached; an unrecognized key ID
// triggers exactly one re-fetch, which is what lets key rotation on the
// auth server's side work without a background refresh goroutine here.
type JWKSValidator struct {
	jwksURL    string
	issuer     string // "" = issuer claim not checked
	audience   string // "" = audience claim not checked
	httpClient *http.Client

	mu   sync.RWMutex
	keys map[string]*rsa.PublicKey
}

// NewJWKSValidator builds a Validator backed by the given JWKS endpoint.
// issuer/audience may be left empty to skip that specific check, but in
// production both should be set — accepting tokens from any issuer or
// meant for any audience defeats the point of checking them at all.
func NewJWKSValidator(jwksURL, issuer, audience string) *JWKSValidator {
	return &JWKSValidator{
		jwksURL:    jwksURL,
		issuer:     issuer,
		audience:   audience,
		httpClient: &http.Client{Timeout: 10 * time.Second},
		keys:       map[string]*rsa.PublicKey{},
	}
}

func (v *JWKSValidator) Validate(ctx context.Context, rawToken string) (Claims, error) {
	var claims tokenClaims
	token, err := jwt.ParseWithClaims(rawToken, &claims, v.keyFunc(ctx), jwt.WithValidMethods([]string{"RS256"}))
	if err != nil || !token.Valid {
		return Claims{}, ErrInvalidToken
	}
	if v.issuer != "" && claims.Issuer != v.issuer {
		return Claims{}, ErrInvalidToken
	}
	if v.audience != "" && !containsString(claims.RegisteredClaims.Audience, v.audience) {
		return Claims{}, ErrInvalidToken
	}
	return Claims{Subject: claims.Subject, Scopes: claims.scopes()}, nil
}

// keyFunc rejects anything not signed with RS256 before touching the key
// cache at all — validating the algorithm inside the callback (rather
// than trusting jwt.WithValidMethods alone) is deliberate defense in
// depth against algorithm-confusion attacks, where a token crafted with
// "alg": "none" or an HMAC algorithm tries to bypass RSA signature
// verification entirely.
func (v *JWKSValidator) keyFunc(ctx context.Context) jwt.Keyfunc {
	return func(token *jwt.Token) (any, error) {
		if _, ok := token.Method.(*jwt.SigningMethodRSA); !ok {
			return nil, fmt.Errorf("auth: unexpected signing method %v", token.Header["alg"])
		}
		kid, _ := token.Header["kid"].(string)
		if kid == "" {
			return nil, fmt.Errorf("auth: token has no kid header")
		}
		if key, ok := v.lookup(kid); ok {
			return key, nil
		}
		if err := v.refresh(ctx); err != nil {
			return nil, fmt.Errorf("auth: refreshing JWKS: %w", err)
		}
		key, ok := v.lookup(kid)
		if !ok {
			return nil, fmt.Errorf("auth: unknown key id %q", kid)
		}
		return key, nil
	}
}

func (v *JWKSValidator) lookup(kid string) (*rsa.PublicKey, bool) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	key, ok := v.keys[kid]
	return key, ok
}

type jwksDoc struct {
	Keys []jwksKey `json:"keys"`
}

type jwksKey struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	N   string `json:"n"`
	E   string `json:"e"`
}

func (v *JWKSValidator) refresh(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.jwksURL, nil)
	if err != nil {
		return err
	}
	resp, err := v.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("jwks endpoint returned status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	var doc jwksDoc
	if err := json.Unmarshal(body, &doc); err != nil {
		return fmt.Errorf("decoding JWKS document: %w", err)
	}

	keys := make(map[string]*rsa.PublicKey, len(doc.Keys))
	for _, k := range doc.Keys {
		if k.Kty != "RSA" || k.Kid == "" {
			continue
		}
		pub, err := parseRSAPublicKey(k)
		if err != nil {
			continue // a single malformed key must not fail the whole refresh
		}
		keys[k.Kid] = pub
	}

	v.mu.Lock()
	v.keys = keys
	v.mu.Unlock()
	return nil
}

func containsString(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}

func parseRSAPublicKey(k jwksKey) (*rsa.PublicKey, error) {
	nBytes, err := base64.RawURLEncoding.DecodeString(k.N)
	if err != nil {
		return nil, fmt.Errorf("decoding modulus: %w", err)
	}
	eBytes, err := base64.RawURLEncoding.DecodeString(k.E)
	if err != nil {
		return nil, fmt.Errorf("decoding exponent: %w", err)
	}
	e := 0
	for _, b := range eBytes {
		e = e<<8 | int(b)
	}
	if e == 0 {
		return nil, fmt.Errorf("zero exponent")
	}
	return &rsa.PublicKey{N: new(big.Int).SetBytes(nBytes), E: e}, nil
}
