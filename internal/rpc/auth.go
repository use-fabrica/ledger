package rpc

import (
	"context"
	"crypto/subtle"
	"errors"
	"net/http"

	"connectrpc.com/connect"
)

// authHeader carries the API key. A plain header is used (rather than only
// Authorization: Bearer) so simple HTTP/JSON clients can call the service
// without constructing a scheme; Authorization Bearer is accepted too.
const authHeader = "X-Api-Key"

// NewAuthInterceptor gates every RPC it is wired onto with the configured
// API key, rejecting missing or invalid keys with CodeUnauthenticated.
// It is a pure middleware seam: swapping API-key auth for mTLS means
// replacing this interceptor, with no handler changes.
func NewAuthInterceptor(apiKey string) connect.Interceptor {
	return connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			if !validKey(req.Header(), apiKey) {
				return nil, connect.NewError(
					connect.CodeUnauthenticated,
					errors.New("rpc: missing or invalid API key"),
				)
			}
			return next(ctx, req)
		}
	})
}

func validKey(header http.Header, apiKey string) bool {
	key := header.Get(authHeader)
	if key == "" {
		// Also accept "Authorization: Bearer <key>".
		const prefix = "Bearer "
		auth := header.Get("Authorization")
		if len(auth) > len(prefix) && auth[:len(prefix)] == prefix {
			key = auth[len(prefix):]
		}
	}
	if key == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(key), []byte(apiKey)) == 1
}
