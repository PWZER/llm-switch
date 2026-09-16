package httpx

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/PWZER/llm-switch/internal/store"
)

// Envelope is the uniform admin API response body (Kimi Code style):
// every JSON response carries code/msg/data/request_id.
type Envelope struct {
	Code      int    `json:"code"`
	Msg       string `json:"msg"`
	Data      any    `json:"data"`
	RequestID string `json:"request_id"`
}

type requestIDCtxKey int

const reqIDKey requestIDCtxKey = 0

func withAdminToken(ctx context.Context, token string) context.Context {
	return context.WithValue(ctx, adminTokenKey, token)
}

func withReqID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, reqIDKey, id)
}

// WriteEnvelope writes a success envelope with HTTP 200.
func WriteEnvelope(w http.ResponseWriter, r *http.Request, data any) {
	writeJSON(w, http.StatusOK, Envelope{Code: 0, Msg: "success", Data: data, RequestID: RequestID(r)})
}

// WriteEnvelopeStatus writes a success envelope with a custom status (201/204 handled by caller).
func WriteEnvelopeStatus(w http.ResponseWriter, r *http.Request, status int, data any) {
	writeJSON(w, status, Envelope{Code: 0, Msg: "success", Data: data, RequestID: RequestID(r)})
}

// WriteEnvelopeError writes an error envelope. code follows the segmented
// scheme: 400xx validation, 401xx auth, 404xx not found, 409xx conflict,
// 429xx rate limit, 500xx internal, 6xxxx upstream (body preserved).
func WriteEnvelopeError(w http.ResponseWriter, r *http.Request, status, code int, msg string) {
	writeJSON(w, status, Envelope{Code: code, Msg: msg, RequestID: RequestID(r)})
}

// MapStoreErr converts repository errors into envelope errors.
func MapStoreErr(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case err == nil:
		return
	case err == store.ErrNotFound:
		WriteEnvelopeError(w, r, http.StatusNotFound, 40401, err.Error())
	default:
		WriteEnvelopeError(w, r, http.StatusInternalServerError, 50001, err.Error())
	}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(body)
}
