package gateway

import (
	"bufio"
	"io"
	"net/http"
	"time"

	"github.com/PWZER/llm-switch/internal/ir"
	"github.com/PWZER/llm-switch/internal/protocol"
)

// errBadRequest marks an unparseable client body: fail fast, never fail over.
type errBadRequest struct{ msg string }

func (e errBadRequest) Error() string { return e.msg }

// prepareUpstreamBody produces the body for one candidate. Same protocol:
// passthrough rewrite of the model field only. Cross protocol: client wire ->
// IR -> upstream wire (the full conversion path).
func prepareUpstreamBody(raw []byte, clientProto, upstreamProto, upstreamModel string) ([]byte, error) {
	if upstreamProto == clientProto {
		return rewriteModel(raw, upstreamModel), nil
	}
	cc, err := protocol.For(ir.Protocol(clientProto))
	if err != nil {
		return nil, errBadRequest{err.Error()}
	}
	uc, err := protocol.For(ir.Protocol(upstreamProto))
	if err != nil {
		return nil, errBadRequest{err.Error()}
	}
	req, derr := cc.DecodeRequest(raw)
	if derr != nil {
		return nil, errBadRequest{derr.Error()}
	}
	req.Model = upstreamModel
	return uc.EncodeRequest(req)
}

// relayConvertedStream pipes a cross-protocol upstream SSE stream to the
// client: upstream SSE -> IR events -> client SSE, flushing per event.
func relayConvertedStream(w http.ResponseWriter, r *http.Request, resp *http.Response,
	upstreamProto, clientProto, model string, idleTimeout time.Duration) (usageInfo, *int64, error) {

	defer resp.Body.Close()
	uc, err := protocol.For(ir.Protocol(upstreamProto))
	if err != nil {
		return usageInfo{}, nil, err
	}
	cr, err := protocol.For(ir.Protocol(clientProto))
	if err != nil {
		return usageInfo{}, nil, err
	}

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	write := func(b []byte) bool {
		if len(b) == 0 {
			return true
		}
		if _, werr := w.Write(b); werr != nil {
			return false
		}
		if flusher != nil {
			flusher.Flush()
		}
		return true
	}

	if pre, _ := cr.NewRenderer(model).Start(); !write(pre) {
		return usageInfo{}, nil, errClientCanceled
	}

	reader := uc.NewStreamReader()
	renderer := cr.NewRenderer(model)
	var usage usageInfo
	var ttft *int64
	start := time.Now()

	wdCtx, wdReset, wdStop := idleWatchdog(r.Context(), idleTimeout)
	defer wdStop()
	buffered := bufio.NewReaderSize(resp.Body, 64*1024)

	for {
		wdReset()
		line, rerr := buffered.ReadBytes('\n')
		if len(line) > 0 {
			if payload, ok := sseDataPayload(line); ok {
				for _, ev := range reader.Feed(payload) {
					if ev.Kind == ir.EvFinish {
						usage = usageInfo{
							Prompt:     ev.Usage.Input,
							Completion: ev.Usage.Output,
							CacheRead:  ev.Usage.CacheRead,
							CacheWrite: ev.Usage.CacheWrite,
							Reasoning:  ev.Usage.Reasoning,
						}
					}
					frame, _ := renderer.Frame(ev)
					if ttft == nil && len(frame) > 0 {
						ms := time.Since(start).Milliseconds()
						ttft = &ms
					}
					if !write(frame) {
						return usage, ttft, errClientCanceled
					}
				}
			}
		}
		if rerr != nil {
			// Flush any terminal events the upstream protocol can still produce.
			for _, ev := range reader.Finish() {
				frame, _ := renderer.Frame(ev)
				write(frame)
			}
			if done := renderer.Done(); len(done) > 0 {
				write(done)
			}
			if r.Context().Err() != nil {
				return usage, ttft, errClientCanceled
			}
			if wdCtx.Err() != nil {
				return usage, ttft, errUpstreamIdleTimeout
			}
			if rerr == io.EOF {
				return usage, ttft, nil
			}
			return usage, ttft, rerr
		}
	}
}

// relayConvertedNonStream converts a complete upstream response body into the
// client's protocol shape.
func relayConvertedNonStream(w http.ResponseWriter, r *http.Request, resp *http.Response,
	upstreamProto, clientProto, model string) (usageInfo, error) {

	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return usageInfo{}, err
	}
	uc, err := protocol.For(ir.Protocol(upstreamProto))
	if err != nil {
		return usageInfo{}, err
	}
	cr, err := protocol.For(ir.Protocol(clientProto))
	if err != nil {
		return usageInfo{}, err
	}
	irResp, derr := uc.DecodeResponse(body)
	if derr != nil {
		return usageInfo{}, derr
	}
	irResp.Model = orDefault(irResp.Model, model)
	out, cerr := cr.EncodeResponse(irResp)
	if cerr != nil {
		return usageInfo{}, cerr
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	if _, werr := w.Write(out); werr != nil {
		return usageFromIR(irResp.Usage), werr
	}
	return usageFromIR(irResp.Usage), nil
}

func usageFromIR(u ir.Usage) usageInfo {
	return usageInfo{
		Prompt: u.Input, Completion: u.Output,
		CacheRead: u.CacheRead, CacheWrite: u.CacheWrite, Reasoning: u.Reasoning,
	}
}
