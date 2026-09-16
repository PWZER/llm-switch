// Package httpx holds shared HTTP plumbing: middleware, error rendering, and
// the admin response envelope.
package httpx

import (
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5/middleware"
)

// ctxKey is the private context key type for this package.
type ctxKey int

const adminTokenKey ctxKey = iota

// AdminAuth guards admin routes with the session bearer token. The skip list
// is expressed by the caller mounting protected and public routes separately.
type AdminAuth struct {
	Valid func(token string) bool
}

// Middleware returns the chi-compatible admin authentication middleware.
func (a AdminAuth) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := bearerToken(r)
		if token == "" || !a.Valid(token) {
			WriteEnvelopeError(w, r, http.StatusUnauthorized, 40101, "unauthorized")
			return
		}
		ctx := withAdminToken(r.Context(), token)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// AdminToken returns the authenticated session token from the request context.
func AdminToken(r *http.Request) string {
	if v, ok := r.Context().Value(adminTokenKey).(string); ok {
		return v
	}
	return ""
}

func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if h == "" {
		return ""
	}
	if len(h) > 7 && strings.EqualFold(h[:7], "bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return strings.TrimSpace(h)
}

// RequestID returns chi's per-request id (X-Request-Id).
func RequestID(r *http.Request) string { return middleware.GetReqID(r.Context()) }
