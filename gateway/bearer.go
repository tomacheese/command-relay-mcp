package gateway

import (
	"context"
	"crypto/subtle"
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
)

// NewFixedBearerVerifier stands in for OAuth until a later phase
// implements it: every MCP request must present this exact bearer
// token.
func NewFixedBearerVerifier(token string) auth.TokenVerifier {
	return func(ctx context.Context, presented string, req *http.Request) (*auth.TokenInfo, error) {
		if subtle.ConstantTimeCompare([]byte(presented), []byte(token)) != 1 {
			log.Printf("gateway: MCP authentication failure from %s", req.RemoteAddr)
			return nil, errors.New("invalid bearer token")
		}
		return &auth.TokenInfo{Expiration: time.Now().Add(24 * time.Hour)}, nil
	}
}
