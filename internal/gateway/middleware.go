package gateway

import (
	"net/http"
	"strings"

	"github.com/PWZER/llm-switch/internal/engine"
)

// ClientKeyRecord is the authenticated caller identity for a request.
type ClientKeyRecord struct {
	ID   int64
	Name string
}

// ClientKeyAuth guards the data plane with gateway keys, accepted from
// `Authorization: Bearer` (OpenAI SDKs) or `x-api-key` (Anthropic SDKs).
// validate performs the snapshot hash lookup; no DB access on the hot path.
func ClientKeyAuth(validate func(key string) (ClientKeyRecord, bool)) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := r.Header.Get("x-api-key")
			if key == "" {
				key = clientBearer(r)
			}
			rec, ok := validate(key)
			if !ok {
				protocol := protocolOpenAI
				if strings.Contains(r.URL.Path, "/messages") {
					protocol = protocolAnthropic
				}
				writeProtocolError(w, r, protocol, http.StatusUnauthorized, "authentication_error", "invalid api key")
				return
			}
			ctx := WithClientKey(r.Context(), engine.ClientKey{ID: rec.ID, Name: rec.Name})
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func clientBearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	const prefix = "bearer "
	if len(h) > len(prefix) && strings.EqualFold(h[:len(prefix)], prefix) {
		return strings.TrimSpace(h[len(prefix):])
	}
	return ""
}
