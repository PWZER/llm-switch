package gateway

import (
	"bufio"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/PWZER/llm-switch/internal/ir"
	"github.com/PWZER/llm-switch/internal/payload"
	"github.com/PWZER/llm-switch/internal/protocol"
)

// errBadRequest marks an unparseable client body: fail fast, never fail over.
type errBadRequest struct{ msg string }

func (e errBadRequest) Error() string { return e.msg }

// prepareUpstreamBody produces the body for one candidate. Responses
// passthrough: rewrite the model field only (stream_options is chat
// vocabulary — never injected into a Responses body). Same protocol:
// passthrough rewrite of the model field only (plus stream_options injection
// so OpenAI-shaped streams report usage). Cross protocol: client wire ->
// IR -> upstream wire (the full conversion path).
func prepareUpstreamBody(raw []byte, clientProto, upstreamProto, upstreamModel string, stream bool, passthrough bool) ([]byte, error) {
	if passthrough {
		return rewriteModel(raw, upstreamModel, false), nil
	}
	if upstreamProto == clientProto {
		// Usage injection only makes sense on the OpenAI wire shape.
		return rewriteModel(raw, upstreamModel, stream && upstreamProto == string(ir.OpenAI)), nil
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
	out, eerr := uc.EncodeRequest(req)
	if eerr != nil {
		// Encode failures mean the client asked for something the upstream
		// wire cannot express (e.g. Responses text.format on anthropic):
		// that is a client error, not a gateway failure.
		return nil, errBadRequest{eerr.Error()}
	}
	return out, nil
}

// relayConvertedStream pipes a cross-protocol upstream SSE stream to the
// client: upstream SSE -> IR events -> client SSE, flushing per event. sink,
// when non-nil, collects the raw upstream bytes (not the rendered client
// frames) for payload recording.
func relayConvertedStream(w http.ResponseWriter, r *http.Request, resp *http.Response,
	upstreamProto, clientProto, model string, idleTimeout time.Duration, sink *payload.Buffer) (usageInfo, *int64, error) {

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
			if sink != nil {
				sink.Write(line)
			}
			if data, ok := sseDataPayload(line); ok {
				for _, ev := range reader.Feed(data) {
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
				slog.Warn("converted stream idle timeout", "upstream", upstreamProto, "client", clientProto)
				return usage, ttft, errUpstreamIdleTimeout
			}
			if rerr == io.EOF {
				return usage, ttft, nil
			}
			slog.Warn("converted stream read failed", "upstream", upstreamProto, "client", clientProto, "err", rerr)
			return usage, ttft, rerr
		}
	}
}

// relayConvertedNonStream converts a complete upstream response body into the
// client's protocol shape. sink, when non-nil, collects the raw upstream body
// for payload recording.
func relayConvertedNonStream(w http.ResponseWriter, r *http.Request, resp *http.Response,
	upstreamProto, clientProto, model string, sink *payload.Buffer) (usageInfo, error) {

	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return usageInfo{}, err
	}
	if sink != nil {
		sink.Write(body)
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
