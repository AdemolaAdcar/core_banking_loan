package auth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func generateTestKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generating RSA key: %v", err)
	}
	return key
}

func jwksServer(t *testing.T, kid string, pub *rsa.PublicKey) *httptest.Server {
	t.Helper()
	eBytes := big64(pub.E)
	doc := jwksDoc{Keys: []jwksKey{{
		Kty: "RSA",
		Kid: kid,
		N:   base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
		E:   base64.RawURLEncoding.EncodeToString(eBytes),
	}}}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(doc)
	}))
}

// big64 mirrors the standard 3-byte encoding of the common exponent
// 65537 used by parseRSAPublicKey's byte-shift decode.
func big64(e int) []byte {
	return []byte{byte(e >> 16), byte(e >> 8), byte(e)}
}

func signToken(t *testing.T, key *rsa.PrivateKey, kid string, claims tokenClaims) string {
	t.Helper()
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	token.Header["kid"] = kid
	signed, err := token.SignedString(key)
	if err != nil {
		t.Fatalf("signing token: %v", err)
	}
	return signed
}

func TestJWKSValidator_ValidToken_ReturnsClaims(t *testing.T) {
	key := generateTestKey(t)
	srv := jwksServer(t, "kid-1", &key.PublicKey)
	defer srv.Close()

	v := NewJWKSValidator(srv.URL, "", "")
	tok := signToken(t, key, "kid-1", tokenClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   "svc-account-1",
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		},
		Scope: "crm:case:read crm:case:write",
	})

	claims, err := v.Validate(context.Background(), tok)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if claims.Subject != "svc-account-1" {
		t.Fatalf("expected subject svc-account-1, got %q", claims.Subject)
	}
	if !claims.HasScope(ScopeCaseRead) || !claims.HasScope(ScopeCaseWrite) {
		t.Fatalf("expected both scopes present, got %v", claims.Scopes)
	}
}

func TestJWKSValidator_ExpiredToken_Rejected(t *testing.T) {
	key := generateTestKey(t)
	srv := jwksServer(t, "kid-1", &key.PublicKey)
	defer srv.Close()

	v := NewJWKSValidator(srv.URL, "", "")
	tok := signToken(t, key, "kid-1", tokenClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   "svc-account-1",
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(-time.Hour)),
		},
		Scope: "crm:case:read",
	})

	if _, err := v.Validate(context.Background(), tok); err != ErrInvalidToken {
		t.Fatalf("expected ErrInvalidToken for an expired token, got %v", err)
	}
}

func TestJWKSValidator_WrongSigningKey_Rejected(t *testing.T) {
	signingKey := generateTestKey(t)
	otherKey := generateTestKey(t)
	// JWKS advertises otherKey's public key under kid-1, but the token
	// is actually signed with signingKey -- must fail signature
	// verification, not silently succeed.
	srv := jwksServer(t, "kid-1", &otherKey.PublicKey)
	defer srv.Close()

	v := NewJWKSValidator(srv.URL, "", "")
	tok := signToken(t, signingKey, "kid-1", tokenClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   "svc-account-1",
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		},
	})

	if _, err := v.Validate(context.Background(), tok); err != ErrInvalidToken {
		t.Fatalf("expected ErrInvalidToken for a signature that doesn't match the advertised key, got %v", err)
	}
}

func TestJWKSValidator_HS256AlgorithmConfusion_Rejected(t *testing.T) {
	key := generateTestKey(t)
	srv := jwksServer(t, "kid-1", &key.PublicKey)
	defer srv.Close()

	v := NewJWKSValidator(srv.URL, "", "")

	// An attacker who obtains the RSA public key (advertised openly via
	// JWKS, by design) tries to forge a token by HMAC-signing it USING
	// that public key's bytes as the HMAC secret -- the classic RS256/
	// HS256 algorithm-confusion attack. This must be rejected outright,
	// not validated against the RSA key.
	forged := jwt.NewWithClaims(jwt.SigningMethodHS256, tokenClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   "attacker",
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		},
		Scope: "crm:case:write",
	})
	forged.Header["kid"] = "kid-1"
	signed, err := forged.SignedString(key.PublicKey.N.Bytes())
	if err != nil {
		t.Fatalf("forging token: %v", err)
	}

	if _, err := v.Validate(context.Background(), signed); err != ErrInvalidToken {
		t.Fatalf("expected ErrInvalidToken for an HS256-signed token against an RS256-only validator, got %v", err)
	}
}

func TestJWKSValidator_WrongIssuer_Rejected(t *testing.T) {
	key := generateTestKey(t)
	srv := jwksServer(t, "kid-1", &key.PublicKey)
	defer srv.Close()

	v := NewJWKSValidator(srv.URL, "https://auth.internal.core-banking.example", "")
	tok := signToken(t, key, "kid-1", tokenClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   "svc-account-1",
			Issuer:    "https://not-the-real-issuer.example",
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		},
	})

	if _, err := v.Validate(context.Background(), tok); err != ErrInvalidToken {
		t.Fatalf("expected ErrInvalidToken for a mismatched issuer, got %v", err)
	}
}

func TestJWKSValidator_UnknownKeyID_TriggersRefreshThenFails(t *testing.T) {
	key := generateTestKey(t)
	srv := jwksServer(t, "kid-1", &key.PublicKey)
	defer srv.Close()

	v := NewJWKSValidator(srv.URL, "", "")
	tok := signToken(t, key, "kid-does-not-exist", tokenClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   "svc-account-1",
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		},
	})

	if _, err := v.Validate(context.Background(), tok); err != ErrInvalidToken {
		t.Fatalf("expected ErrInvalidToken for an unknown kid, got %v", err)
	}
}

func TestClaims_HasScope(t *testing.T) {
	c := Claims{Scopes: []string{"crm:case:read"}}
	if !c.HasScope("crm:case:read") {
		t.Fatalf("expected HasScope(crm:case:read) to be true")
	}
	if c.HasScope("crm:case:write") {
		t.Fatalf("expected HasScope(crm:case:write) to be false")
	}
}

func TestTokenClaims_ScopesFallsBackToScpArray(t *testing.T) {
	c := tokenClaims{Scp: []string{"crm:case:read", "crm:case:write"}}
	got := c.scopes()
	if len(got) != 2 || got[0] != "crm:case:read" || got[1] != "crm:case:write" {
		t.Fatalf("expected scp fallback to be used, got %v", got)
	}
}
