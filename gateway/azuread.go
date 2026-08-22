package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/modelcontextprotocol/go-sdk/auth"
)

// OAuthAuthorizePath and OAuthTokenPath are the Gateway's own proxy routes
// for Azure AD's authorize/token endpoints (see AzureADOAuthHandlers).
// Exported so main.go's mux registration and the metadata body this file
// builds can't drift apart.
const (
	OAuthAuthorizePath = "/oauth/authorize"
	OAuthTokenPath     = "/oauth/token"
)

// maxOAuthTokenBody caps the body newOAuthTokenProxy will read from an
// unauthenticated client request. Plain form-encoded OAuth token
// requests are a few hundred bytes. 64 KiB leaves generous headroom
// while still bounding the read.
const maxOAuthTokenBody = 64 * 1024

// oauthTokenProxyClient bounds newOAuthTokenProxy's outbound call to
// Azure. http.DefaultClient has no timeout, so a slow or unresponsive
// Azure token endpoint could hang the handler goroutine indefinitely.
var oauthTokenProxyClient = &http.Client{Timeout: 10 * time.Second}

// azureADClaims carries the extra claims read out of a verified Azure AD
// token beyond the go-oidc IDToken's own Subject/Expiry: "oid" is the
// stable per-user object ID Microsoft recommends over "sub" for
// multi-tenant apps, and "scp" is the space-delimited scope string
// Azure AD emits (unlike the OAuth-standard JSON array).
type azureADClaims struct {
	OID    string `json:"oid"`
	Scopes string `json:"scp"`
}

// NewAzureADVerifier verifies bearer tokens issued by Microsoft Entra ID
// (Azure AD) for the given tenant and expected audience (addendum §2).
// It performs OIDC discovery once, at startup, against the tenant's v2.0
// issuer via the established github.com/coreos/go-oidc/v3 library — no
// hand-rolled JWT/JWKS handling, per the standing "no custom crypto" rule.
func NewAzureADVerifier(ctx context.Context, tenantID, audience string) (auth.TokenVerifier, error) {
	issuer := "https://login.microsoftonline.com/" + tenantID + "/v2.0"
	provider, err := oidc.NewProvider(ctx, issuer)
	if err != nil {
		return nil, fmt.Errorf("azure ad discovery: %w", err)
	}
	return newAzureADTokenVerifier(provider.Verifier(&oidc.Config{ClientID: audience})), nil
}

// newAzureADTokenVerifier is separated from NewAzureADVerifier so tests
// can drive it with an oidc.NewVerifier built on a StaticKeySet, without
// any network call or live Azure tenant.
func newAzureADTokenVerifier(idv *oidc.IDTokenVerifier) auth.TokenVerifier {
	return func(ctx context.Context, presented string, req *http.Request) (*auth.TokenInfo, error) {
		idToken, err := idv.Verify(ctx, presented)
		if err != nil {
			logAzureADAuthFailure(req)
			return nil, fmt.Errorf("%w: %v", auth.ErrInvalidToken, err)
		}
		var claims azureADClaims
		if err := idToken.Claims(&claims); err != nil {
			logAzureADAuthFailure(req)
			return nil, fmt.Errorf("%w: %v", auth.ErrInvalidToken, err)
		}
		userID := claims.OID
		if userID == "" {
			userID = idToken.Subject
		}
		var scopes []string
		if claims.Scopes != "" {
			scopes = strings.Fields(claims.Scopes)
		}
		return &auth.TokenInfo{UserID: userID, Expiration: idToken.Expiry, Scopes: scopes}, nil
	}
}

// AzureADOAuthHandlers bundles the proxied RFC 8414 metadata document and
// the authorize/token proxies it advertises. This lets main.go mount all
// three without re-deriving Azure's endpoint URLs itself.
type AzureADOAuthHandlers struct {
	Metadata  http.HandlerFunc
	Authorize http.HandlerFunc
	Token     http.HandlerFunc
}

// NewAzureADOAuthHandlers discovers Azure AD's real OIDC endpoints for
// tenantID. The returned handlers take over selfIssuer's whole
// authorization-server role, not just discovery metadata.
//
// Metadata mirrors Azure's own RFC 8414 document, with "issuer" rewritten
// to selfIssuer. It also adds "code_challenge_methods_supported". Azure
// supports PKCE/S256 but never advertises it. Strict clients like
// ChatGPT's apps_sdk validator reject metadata that omits it.
//
// Authorize and Token forward to Azure's real endpoints, with the RFC
// 8707 "resource" parameter stripped. Azure AD's v2 endpoint doesn't
// implement RFC 8707. It rejects a request carrying both "resource" and
// "scope" together. That breaks any MCP client (e.g. ChatGPT) that sends
// "resource" per the MCP spec.
//
// selfIssuer must match this Gateway's own authorization_servers entry.
// RFC 8414 §3 requires the document's "issuer" to equal the URL it was
// fetched from. Token verification (NewAzureADVerifier) still validates
// against Azure's real issuer, unaffected by any of this.
func NewAzureADOAuthHandlers(ctx context.Context, tenantID, selfIssuer string) (*AzureADOAuthHandlers, error) {
	issuer := "https://login.microsoftonline.com/" + tenantID + "/v2.0"
	provider, err := oidc.NewProvider(ctx, issuer)
	if err != nil {
		return nil, fmt.Errorf("azure ad discovery: %w", err)
	}
	var metadata map[string]any
	if err := provider.Claims(&metadata); err != nil {
		return nil, fmt.Errorf("azure ad discovery: reading raw metadata: %w", err)
	}
	azureAuthorizeEndpoint, azureTokenEndpoint, err := azureADOAuthEndpoints(metadata)
	if err != nil {
		return nil, err
	}
	body, err := buildAzureADAuthServerMetadataBody(metadata, selfIssuer)
	if err != nil {
		return nil, err
	}
	return &AzureADOAuthHandlers{
		Metadata: func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(body)
		},
		Authorize: newOAuthAuthorizeProxy(azureAuthorizeEndpoint),
		Token:     newOAuthTokenProxy(azureTokenEndpoint),
	}, nil
}

// azureADOAuthEndpoints is split out from NewAzureADOAuthHandlers.
// Tests can then drive its validation directly on a literal metadata
// map, without any network call or live Azure tenant.
func azureADOAuthEndpoints(azureMetadata map[string]any) (authorizeEndpoint, tokenEndpoint string, err error) {
	authorizeEndpoint, ok := azureMetadata["authorization_endpoint"].(string)
	if !ok || authorizeEndpoint == "" {
		return "", "", fmt.Errorf("azure ad discovery: missing authorization_endpoint")
	}
	tokenEndpoint, ok = azureMetadata["token_endpoint"].(string)
	if !ok || tokenEndpoint == "" {
		return "", "", fmt.Errorf("azure ad discovery: missing token_endpoint")
	}
	return authorizeEndpoint, tokenEndpoint, nil
}

// buildAzureADAuthServerMetadataBody is split out from
// NewAzureADOAuthHandlers. This mirrors how newAzureADTokenVerifier is
// split out from NewAzureADVerifier.
//
// Tests can drive it directly on a literal metadata map, without any
// network call or live Azure tenant.
func buildAzureADAuthServerMetadataBody(azureMetadata map[string]any, selfIssuer string) ([]byte, error) {
	merged := make(map[string]any, len(azureMetadata)+2)
	for k, v := range azureMetadata {
		merged[k] = v
	}
	merged["issuer"] = selfIssuer
	merged["code_challenge_methods_supported"] = []string{"S256"}
	merged["authorization_endpoint"] = selfIssuer + OAuthAuthorizePath
	merged["token_endpoint"] = selfIssuer + OAuthTokenPath
	body, err := json.Marshal(merged)
	if err != nil {
		return nil, fmt.Errorf("azure ad discovery: encoding proxied metadata: %w", err)
	}
	return body, nil
}

// newOAuthAuthorizeProxy redirects a browser's /oauth/authorize
// navigation to azureAuthorizeEndpoint. The RFC 8707 "resource"
// parameter is stripped first (see NewAzureADOAuthHandlers).
func newOAuthAuthorizeProxy(azureAuthorizeEndpoint string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		q.Del("resource")
		http.Redirect(w, r, azureAuthorizeEndpoint+"?"+q.Encode(), http.StatusFound)
	}
}

// newOAuthTokenProxy forwards a client's /oauth/token POST to
// azureTokenEndpoint. The RFC 8707 "resource" parameter is stripped from
// the form body first (see NewAzureADOAuthHandlers). One code path
// covers both the authorization_code and refresh_token grants. Azure's
// response — success or OAuth error JSON — is relayed back verbatim.
func newOAuthTokenProxy(azureTokenEndpoint string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxOAuthTokenBody)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			log.Printf("gateway: azure ad token proxy: reading request body: %v", err)
			http.Error(w, "request body too large or unreadable", http.StatusBadRequest)
			return
		}
		form, err := url.ParseQuery(string(body))
		if err != nil {
			log.Printf("gateway: azure ad token proxy: parsing form body: %v", err)
			http.Error(w, "invalid form body", http.StatusBadRequest)
			return
		}
		form.Del("resource")

		req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, azureTokenEndpoint, strings.NewReader(form.Encode()))
		if err != nil {
			log.Printf("gateway: azure ad token proxy: building upstream request: %v", err)
			http.Error(w, "failed to build upstream request", http.StatusInternalServerError)
			return
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if authHeader := r.Header.Get("Authorization"); authHeader != "" {
			req.Header.Set("Authorization", authHeader)
		}

		resp, err := oauthTokenProxyClient.Do(req)
		if err != nil {
			log.Printf("gateway: azure ad token proxy: upstream request failed: %v", err)
			http.Error(w, "upstream token request failed", http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()

		if ct := resp.Header.Get("Content-Type"); ct != "" {
			w.Header().Set("Content-Type", ct)
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}
}

// This project's Gateway app registration is self-referencing (one Azure
// AD app is both the protected resource and the OAuth client), a scenario
// where Azure AD (AADSTS90009) requires the bare appId GUID as the scope
// and rejects the "api://..." Application ID URI form.
func AzureDefaultScope(audience string) string {
	guid := strings.TrimPrefix(audience, "api://")
	if i := strings.IndexByte(guid, '/'); i != -1 {
		guid = guid[:i]
	}
	return guid + "/.default"
}

// logAzureADAuthFailure mirrors NewFixedBearerVerifier's own log line —
// req may be nil in tests that verify the token in isolation, and
// RemoteAddr (never the token itself) is the only detail logged either
// way.
func logAzureADAuthFailure(req *http.Request) {
	remoteAddr := "unknown"
	if req != nil {
		remoteAddr = req.RemoteAddr
	}
	log.Printf("gateway: MCP authentication failure from %s", remoteAddr)
}
