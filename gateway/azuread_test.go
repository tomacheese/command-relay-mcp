package gateway

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
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

// TestBuildAzureADAuthServerMetadataBody_RewritesEndpointsAndAddsPKCESupport
// covers the rationale documented on NewAzureADOAuthHandlers.
func TestBuildAzureADAuthServerMetadataBody_RewritesEndpointsAndAddsPKCESupport(t *testing.T) {
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
	if got["authorization_endpoint"] != "https://gateway.example.invalid"+OAuthAuthorizePath {
		t.Fatalf("authorization_endpoint = %v, want the Gateway's own proxy route", got["authorization_endpoint"])
	}
	if got["token_endpoint"] != "https://gateway.example.invalid"+OAuthTokenPath {
		t.Fatalf("token_endpoint = %v, want the Gateway's own proxy route", got["token_endpoint"])
	}
}

func TestAzureADOAuthEndpoints_RejectsMissingEndpoints(t *testing.T) {
	tests := []struct {
		name     string
		metadata map[string]any
	}{
		{"missing authorization_endpoint", map[string]any{
			"token_endpoint": testIssuer + "/oauth2/v2.0/token",
		}},
		{"missing token_endpoint", map[string]any{
			"authorization_endpoint": testIssuer + "/oauth2/v2.0/authorize",
		}},
		{"empty authorization_endpoint", map[string]any{
			"authorization_endpoint": "",
			"token_endpoint":         testIssuer + "/oauth2/v2.0/token",
		}},
		{"wrong type", map[string]any{
			"authorization_endpoint": 123,
			"token_endpoint":         testIssuer + "/oauth2/v2.0/token",
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, _, err := azureADOAuthEndpoints(tt.metadata); err == nil {
				t.Fatal("expected an error for a missing/invalid endpoint")
			}
		})
	}
}

func TestAzureADOAuthEndpoints_AcceptsValidEndpoints(t *testing.T) {
	authorize, token, err := azureADOAuthEndpoints(map[string]any{
		"authorization_endpoint": testIssuer + "/oauth2/v2.0/authorize",
		"token_endpoint":         testIssuer + "/oauth2/v2.0/token",
	})
	if err != nil {
		t.Fatalf("azureADOAuthEndpoints: %v", err)
	}
	if authorize != testIssuer+"/oauth2/v2.0/authorize" || token != testIssuer+"/oauth2/v2.0/token" {
		t.Fatalf("authorize = %q, token = %q, want the metadata's own values", authorize, token)
	}
}

func TestOAuthAuthorizeProxy_StripsResourceAndPreservesParams(t *testing.T) {
	tests := []struct {
		name  string
		query string
	}{
		{"with resource", "resource=https://mcp.example.invalid/mcp&client_id=abc&redirect_uri=https://client.example.invalid/cb&response_type=code&state=xyz&scope=openid&code_challenge=chal&code_challenge_method=S256"},
		{"without resource", "client_id=abc&redirect_uri=https://client.example.invalid/cb&response_type=code&state=xyz&scope=openid&code_challenge=chal&code_challenge_method=S256"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				t.Fatalf("upstream should never be hit directly; got %s", r.URL)
			}))
			defer upstream.Close()

			srv := httptest.NewServer(newOAuthAuthorizeProxy(upstream.URL + "/authorize"))
			defer srv.Close()

			client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			}}
			resp, err := client.Get(srv.URL + "/oauth/authorize?" + tt.query)
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusFound {
				t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusFound)
			}
			loc, err := url.Parse(resp.Header.Get("Location"))
			if err != nil {
				t.Fatalf("parse Location: %v", err)
			}
			if got := upstream.URL + loc.Path; got != upstream.URL+"/authorize" {
				t.Fatalf("Location path = %q, want upstream authorize endpoint", got)
			}
			q := loc.Query()
			if q.Has("resource") {
				t.Fatalf("resource param leaked to upstream: %v", q)
			}
			for _, key := range []string{"client_id", "redirect_uri", "state", "scope", "code_challenge", "code_challenge_method"} {
				want := (url.Values{"client_id": {"abc"}, "redirect_uri": {"https://client.example.invalid/cb"}, "state": {"xyz"}, "scope": {"openid"}, "code_challenge": {"chal"}, "code_challenge_method": {"S256"}})[key][0]
				if got := q.Get(key); got != want {
					t.Fatalf("%s = %q, want %q", key, got, want)
				}
			}
		})
	}
}

func TestOAuthTokenProxy_StripsResourceAndRelaysUpstream(t *testing.T) {
	tests := []struct {
		name         string
		form         url.Values
		upstreamCode int
		upstreamBody string
	}{
		{
			name: "authorization_code success",
			form: url.Values{
				"grant_type":    {"authorization_code"},
				"code":          {"auth-code"},
				"client_id":     {"abc"},
				"code_verifier": {"verifier"},
				"redirect_uri":  {"https://client.example.invalid/cb"},
				"resource":      {"https://mcp.example.invalid/mcp"},
			},
			upstreamCode: http.StatusOK,
			upstreamBody: `{"access_token":"tok","token_type":"Bearer"}`,
		},
		{
			name: "refresh_token no resource",
			form: url.Values{
				"grant_type":    {"refresh_token"},
				"refresh_token": {"rt-1"},
				"client_id":     {"abc"},
			},
			upstreamCode: http.StatusOK,
			upstreamBody: `{"access_token":"tok2","token_type":"Bearer"}`,
		},
		{
			name: "upstream error relayed",
			form: url.Values{
				"grant_type": {"authorization_code"},
				"code":       {"bad-code"},
				"client_id":  {"abc"},
				"resource":   {"https://mcp.example.invalid/mcp"},
			},
			upstreamCode: http.StatusBadRequest,
			upstreamBody: `{"error":"invalid_grant"}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotForm url.Values
			var gotContentType string
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				gotForm, _ = url.ParseQuery(string(body))
				gotContentType = r.Header.Get("Content-Type")
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tt.upstreamCode)
				_, _ = w.Write([]byte(tt.upstreamBody))
			}))
			defer upstream.Close()

			srv := httptest.NewServer(newOAuthTokenProxy(upstream.URL))
			defer srv.Close()

			resp, err := http.Post(srv.URL+"/oauth/token", "application/x-www-form-urlencoded", strings.NewReader(tt.form.Encode()))
			if err != nil {
				t.Fatalf("POST: %v", err)
			}
			defer resp.Body.Close()
			respBody, _ := io.ReadAll(resp.Body)

			if gotForm.Has("resource") {
				t.Fatalf("resource param leaked to upstream: %v", gotForm)
			}
			for key, want := range tt.form {
				if key == "resource" {
					continue
				}
				if got := gotForm.Get(key); got != want[0] {
					t.Fatalf("upstream field %s = %q, want %q", key, got, want[0])
				}
			}
			if gotContentType != "application/x-www-form-urlencoded" {
				t.Fatalf("upstream Content-Type = %q, want application/x-www-form-urlencoded", gotContentType)
			}
			if resp.StatusCode != tt.upstreamCode {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tt.upstreamCode)
			}
			if string(respBody) != tt.upstreamBody {
				t.Fatalf("body = %q, want %q", respBody, tt.upstreamBody)
			}
		})
	}
}

func TestOAuthTokenProxy_ForwardsAuthorizationHeader(t *testing.T) {
	var gotAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"access_token":"tok"}`))
	}))
	defer upstream.Close()

	srv := httptest.NewServer(newOAuthTokenProxy(upstream.URL))
	defer srv.Close()

	form := url.Values{"grant_type": {"authorization_code"}, "code": {"c"}}
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/oauth/token", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Authorization", "Basic Y2xpZW50OnNlY3JldA==")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()

	if gotAuth != "Basic Y2xpZW50OnNlY3JldA==" {
		t.Fatalf("upstream Authorization = %q, want the client's Basic header forwarded", gotAuth)
	}
}

func TestOAuthTokenProxy_RejectsOversizedBody(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("upstream should never be reached for an oversized body")
	}))
	defer upstream.Close()

	srv := httptest.NewServer(newOAuthTokenProxy(upstream.URL))
	defer srv.Close()

	oversized := strings.Repeat("a", maxOAuthTokenBody+1)
	resp, err := http.Post(srv.URL+"/oauth/token", "application/x-www-form-urlencoded", strings.NewReader("code="+oversized))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusBadRequest)
	}
}

func TestAzureDefaultScope(t *testing.T) {
	tests := []struct {
		audience string
		want     string
	}{
		{"1df6537b-d0ad-48f3-b9e6-98b9940b52a5", "1df6537b-d0ad-48f3-b9e6-98b9940b52a5/.default"},
		{"api://1df6537b-d0ad-48f3-b9e6-98b9940b52a5", "1df6537b-d0ad-48f3-b9e6-98b9940b52a5/.default"},
		{"api://1df6537b-d0ad-48f3-b9e6-98b9940b52a5/mcp.access", "1df6537b-d0ad-48f3-b9e6-98b9940b52a5/.default"},
	}
	for _, tt := range tests {
		if got := AzureDefaultScope(tt.audience); got != tt.want {
			t.Errorf("AzureDefaultScope(%q) = %q, want %q", tt.audience, got, tt.want)
		}
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
