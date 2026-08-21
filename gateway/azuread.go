package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/modelcontextprotocol/go-sdk/auth"
)

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

// NewAzureADAuthServerMetadataHandler serves an RFC 8414 authorization
// server metadata document at selfIssuer, mirroring Azure AD's real OIDC
// discovery document but adding "code_challenge_methods_supported":
// ["S256"]. Azure AD accepts PKCE/S256 on its real authorize/token
// endpoints but never advertises it in discovery; strict clients (e.g.
// ChatGPT's apps_sdk validator) reject an authorization server whose
// metadata doesn't say so, even though the flow itself would work.
// selfIssuer must match this Gateway's own authorization_servers entry —
// RFC 8414 §3 requires the document's "issuer" to equal the URL it was
// fetched from. This proxy only affects what clients see at discovery
// time; token verification (NewAzureADVerifier) still validates against
// Azure's real issuer and is unaffected by it.
func NewAzureADAuthServerMetadataHandler(ctx context.Context, tenantID, selfIssuer string) (http.HandlerFunc, error) {
	issuer := "https://login.microsoftonline.com/" + tenantID + "/v2.0"
	provider, err := oidc.NewProvider(ctx, issuer)
	if err != nil {
		return nil, fmt.Errorf("azure ad discovery: %w", err)
	}
	var metadata map[string]any
	if err := provider.Claims(&metadata); err != nil {
		return nil, fmt.Errorf("azure ad discovery: reading raw metadata: %w", err)
	}
	body, err := buildAzureADAuthServerMetadataBody(metadata, selfIssuer)
	if err != nil {
		return nil, err
	}
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}, nil
}

// buildAzureADAuthServerMetadataBody is split out from
// NewAzureADAuthServerMetadataHandler so tests can drive it directly on a
// literal metadata map, without any network call or live Azure tenant —
// mirroring how newAzureADTokenVerifier is split out for the same reason.
func buildAzureADAuthServerMetadataBody(azureMetadata map[string]any, selfIssuer string) ([]byte, error) {
	merged := make(map[string]any, len(azureMetadata)+2)
	for k, v := range azureMetadata {
		merged[k] = v
	}
	merged["issuer"] = selfIssuer
	merged["code_challenge_methods_supported"] = []string{"S256"}
	body, err := json.Marshal(merged)
	if err != nil {
		return nil, fmt.Errorf("azure ad discovery: encoding proxied metadata: %w", err)
	}
	return body, nil
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
