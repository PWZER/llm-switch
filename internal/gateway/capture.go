package gateway

import (
	"net/http"

	"github.com/PWZER/llm-switch/internal/payload"
)

// capture collects the three payload segments for one recorded request.
// Client fields are set once; upstream/response fields are per-attempt and
// reset on failover, so only the final attempt survives. Recording is
// read-only with respect to the relay: it never touches socket writes.
type capture struct {
	clientHeaders map[string][]string
	clientBody    []byte

	upHeaders   map[string][]string
	upBody      []byte
	hasUpstream bool

	respStatus  int
	respHeaders map[string][]string
	respBody    *payload.Buffer
	hasResponse bool
}

// newCapture snapshots the client-facing request (headers redacted).
func newCapture(r *http.Request, body []byte) *capture {
	return &capture{
		clientHeaders: payload.RedactHeaders(r.Header),
		clientBody:    body,
	}
}

// resetAttempt clears per-attempt fields at the top of each failover
// iteration.
func (c *capture) resetAttempt() {
	c.upHeaders, c.upBody, c.hasUpstream = nil, nil, false
	c.respStatus, c.respHeaders, c.respBody, c.hasResponse = 0, nil, nil, false
}

// setUpstream records the exact request sent upstream: headers as finalized
// by setUpstreamHeaders (redacted — the channel secret rides them) and the
// prepared body.
func (c *capture) setUpstream(req *http.Request, body []byte) {
	c.upHeaders = payload.RedactHeaders(req.Header)
	c.upBody = body
	c.hasUpstream = true
}

// setResponse records the upstream status line and headers before relaying
// and allocates the body sink the relays append to.
func (c *capture) setResponse(resp *http.Response) {
	c.respStatus = resp.StatusCode
	c.respHeaders = payload.RedactHeaders(resp.Header)
	c.respBody = payload.NewBuffer(payload.MaxSegmentBytes)
	c.hasResponse = true
}

// sink returns the response body collector, nil-safe for relays.
func (c *capture) sink() *payload.Buffer {
	if c == nil {
		return nil
	}
	return c.respBody
}

// record builds the payload.Record for the writer.
func (c *capture) record(requestID string, ts int64) *payload.Record {
	rec := &payload.Record{
		RequestID: requestID,
		TS:        ts,
		ClientReq: payload.Segment{Headers: c.clientHeaders, Body: c.clientBody},
	}
	if c.hasUpstream {
		rec.UpstreamReq = &payload.Segment{Headers: c.upHeaders, Body: c.upBody}
	}
	if c.hasResponse {
		rec.RespStatus = c.respStatus
		rec.RespHeaders = c.respHeaders
		rec.RespBody = c.respBody.Bytes()
		rec.RespTrunc = c.respBody.Truncated()
	}
	return rec
}

// enqueuePayload hands the captured record to the async writer and returns
// the relative payload directory for the log row ("" when not recording).
func (g *Gateway) enqueuePayload(c *capture, requestID string, ts int64) string {
	if c == nil || g.Payloads == nil {
		return ""
	}
	rel, _ := g.Payloads.Enqueue(c.record(requestID, ts))
	return rel
}
