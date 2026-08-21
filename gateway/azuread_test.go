package gateway

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	jose "github.com/go-jose/go-jose/v4"
	"github.com/modelcontextprotocol/go-sdk/auth"
)

const testIssuer = "https://login.microsoftonline.com/test-tenant/v2.0"
const testAudience = "api://test-app/remote.access"

type testClaims struct {
	Issuer   string `json:"iss"`
	Subject  string `json:"sub"`
	Audience string `json:"aud"`
	Expiry   int64  `json:"exp"`
	IssuedAt int64  `json:"iat"`
	OID      string `json:"oid"`
}

func signTestToken(t *testing.T, key *rsa.PrivateKey, claims testClaims) string {
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key}, nil)
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("Marshal claims: %v", err)
	}
	obj, err := signer.Sign(payload)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	raw, err := obj.CompactSerialize()
	if err != nil {
		t.Fatalf("CompactSerialize: %v", err)
	}
	return raw
}

func testVerifier(t *testing.T, key *rsa.PrivateKey) auth.TokenVerifier {
	// oidc.StaticKeySet.PublicKeys is typed []crypto.PublicKey, a named
	// type distinct from []any — a bare []any{...} literal does not
	// satisfy it.
	keySet := &oidc.StaticKeySet{PublicKeys: []crypto.PublicKey{&key.PublicKey}}
	idv := oidc.NewVerifier(testIssuer, keySet, &oidc.Config{ClientID: testAudience})
	return newAzureADTokenVerifier(idv)
}

func TestAzureADVerifier_AcceptsValidToken(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	now := time.Now()
	tok := signTestToken(t, key, testClaims{
		Issuer: testIssuer, Subject: "sub-1", Audience: testAudience,
		Expiry: now.Add(time.Hour).Unix(), IssuedAt: now.Unix(), OID: "oid-123",
	})

	info, err := testVerifier(t, key)(context.Background(), tok, nil)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if info.UserID != "oid-123" {
		t.Fatalf("UserID = %q, want oid-123", info.UserID)
	}
	if info.Expiration.IsZero() {
		t.Fatal("Expiration not set")
	}
}

func TestAzureADVerifier_RejectsWrongIssuer(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	now := time.Now()
	tok := signTestToken(t, key, testClaims{
		Issuer: "https://login.microsoftonline.com/some-other-tenant/v2.0", Subject: "sub-1", Audience: testAudience,
		Expiry: now.Add(time.Hour).Unix(), IssuedAt: now.Unix(),
	})

	if _, err := testVerifier(t, key)(context.Background(), tok, nil); err == nil {
		t.Fatal("expected an error for wrong issuer")
	} else if !errors.Is(err, auth.ErrInvalidToken) {
		t.Fatalf("error = %v, want wrapping auth.ErrInvalidToken", err)
	}
}

func TestAzureADVerifier_RejectsWrongAudience(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	now := time.Now()
	tok := signTestToken(t, key, testClaims{
		Issuer: testIssuer, Subject: "sub-1", Audience: "api://someone-else/scope",
		Expiry: now.Add(time.Hour).Unix(), IssuedAt: now.Unix(),
	})

	if _, err := testVerifier(t, key)(context.Background(), tok, nil); err == nil {
		t.Fatal("expected an error for wrong audience")
	} else if !errors.Is(err, auth.ErrInvalidToken) {
		t.Fatalf("error = %v, want wrapping auth.ErrInvalidToken", err)
	}
}

func TestAzureADVerifier_RejectsExpiredToken(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	now := time.Now()
	tok := signTestToken(t, key, testClaims{
		Issuer: testIssuer, Subject: "sub-1", Audience: testAudience,
		Expiry: now.Add(-time.Hour).Unix(), IssuedAt: now.Add(-2 * time.Hour).Unix(),
	})

	if _, err := testVerifier(t, key)(context.Background(), tok, nil); err == nil {
		t.Fatal("expected an error for expired token")
	} else if !errors.Is(err, auth.ErrInvalidToken) {
		t.Fatalf("error = %v, want wrapping auth.ErrInvalidToken", err)
	}
}

func TestAzureADVerifier_RejectsBadSignature(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	wrongKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	now := time.Now()
	tok := signTestToken(t, wrongKey, testClaims{ // signed with the WRONG key
		Issuer: testIssuer, Subject: "sub-1", Audience: testAudience,
		Expiry: now.Add(time.Hour).Unix(), IssuedAt: now.Unix(),
	})

	if _, err := testVerifier(t, key)(context.Background(), tok, nil); err == nil {
		t.Fatal("expected an error for bad signature")
	} else if !errors.Is(err, auth.ErrInvalidToken) {
		t.Fatalf("error = %v, want wrapping auth.ErrInvalidToken", err)
	}
}

// TestBuildAzureADAuthServerMetadataBody_AddsPKCESupportAndSelfIssuer
// covers the fix for ChatGPT's apps_sdk validator rejecting Azure AD:
// Azure's real discovery document never advertises
// "code_challenge_methods_supported" even though it accepts PKCE/S256 at
// runtime, so the proxy must add it — and its "issuer" must become
// selfIssuer, per RFC 8414 §3, rather than Azure's own issuer.
func TestBuildAzureADAuthServerMetadataBody_AddsPKCESupportAndSelfIssuer(t *testing.T) {
	azureMetadata := map[string]any{
		"issuer":                 testIssuer,
		"authorization_endpoint": testIssuer + "/oauth2/v2.0/authorize",
		"token_endpoint":         testIssuer + "/oauth2/v2.0/token",
	}

	body, err := buildAzureADAuthServerMetadataBody(azureMetadata, "https://gateway.example.invalid")
	if err != nil {
		t.Fatalf("buildAzureADAuthServerMetadataBody: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got["issuer"] != "https://gateway.example.invalid" {
		t.Fatalf("issuer = %v, want the Gateway's own origin", got["issuer"])
	}
	methods, ok := got["code_challenge_methods_supported"].([]any)
	if !ok || len(methods) != 1 || methods[0] != "S256" {
		t.Fatalf("code_challenge_methods_supported = %v, want [\"S256\"]", got["code_challenge_methods_supported"])
	}
	if got["authorization_endpoint"] != testIssuer+"/oauth2/v2.0/authorize" {
		t.Fatalf("authorization_endpoint = %v, want Azure's real endpoint preserved", got["authorization_endpoint"])
	}
	if got["token_endpoint"] != testIssuer+"/oauth2/v2.0/token" {
		t.Fatalf("token_endpoint = %v, want Azure's real endpoint preserved", got["token_endpoint"])
	}
}

// TestAzureADVerifier_LogsAuthenticationFailure covers the Gateway's
// "authentication failure" logging for the Azure AD verifier path —
// the fixed-bearer-token verifier already logs this (gateway/bearer.go);
// the Azure AD verifier must too.
func TestAzureADVerifier_LogsAuthenticationFailure(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	now := time.Now()
	tok := signTestToken(t, key, testClaims{
		Issuer: testIssuer, Subject: "sub-1", Audience: "api://someone-else/scope",
		Expiry: now.Add(time.Hour).Unix(), IssuedAt: now.Unix(),
	})

	var logBuf bytes.Buffer
	log.SetOutput(&logBuf)
	defer log.SetOutput(os.Stderr)

	req, _ := http.NewRequest(http.MethodPost, "http://example.invalid/mcp", nil)
	req.RemoteAddr = "203.0.113.1:12345"
	if _, err := testVerifier(t, key)(context.Background(), tok, req); err == nil {
		t.Fatal("expected an error for wrong audience")
	}
	if !strings.Contains(logBuf.String(), "authentication failure") {
		t.Fatalf("log output = %q, want it to mention \"authentication failure\"", logBuf.String())
	}
}
