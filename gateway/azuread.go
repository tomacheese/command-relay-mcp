package gateway

import (
	"context"
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

// logAzureADAuthFailure mirrors NewFixedBearerVerifier's own log line
// (base spec §24's "authentication failure" requirement) — req may be
// nil in tests that verify the token in isolation, and RemoteAddr
// (never the token itself) is the only detail logged either way.
func logAzureADAuthFailure(req *http.Request) {
	remoteAddr := "unknown"
	if req != nil {
		remoteAddr = req.RemoteAddr
	}
	log.Printf("gateway: MCP authentication failure from %s", remoteAddr)
}
