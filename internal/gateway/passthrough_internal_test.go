package gateway

import "testing"

// The usage tap normalizes every upstream protocol to the total-input
// convention: Prompt INCLUDES cache read/write tokens (OpenAI convention).

func TestTapNonStreamAnthropicBody(t *testing.T) {
	body := []byte(`{"id":"msg_1","type":"message","role":"assistant","model":"claude-x",` +
		`"content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn",` +
		`"usage":{"input_tokens":618,"output_tokens":8,"cache_read_input_tokens":70912}}`)
	u := tapNonStreamBody(protocolAnthropic, body)
	if u.Prompt != 618+70912 || u.Completion != 8 || u.CacheRead != 70912 || u.CacheWrite != 0 {
		t.Fatalf("anthropic non-stream tap wrong: %+v", u)
	}
}

func TestTapAnthropicStreamFullUsageInMessageDelta(t *testing.T) {
	// Zhipu/DeepSeek style: message_start carries no usage, message_delta
	// carries the FULL usage object including cache fields.
	var u usageInfo
	tapStreamLine(protocolAnthropic, []byte(`data: {"type":"message_start","message":{"id":"m","model":"x"}}`), &u)
	tapStreamLine(protocolAnthropic, []byte(`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},`+
		`"usage":{"input_tokens":100,"output_tokens":5,"cache_read_input_tokens":80,"cache_creation_input_tokens":10}}`), &u)
	if u.Prompt != 100+80+10 || u.Completion != 5 || u.CacheRead != 80 || u.CacheWrite != 10 {
		t.Fatalf("message_delta full-usage tap wrong: %+v", u)
	}
}

func TestTapAnthropicStreamOfficialSplit(t *testing.T) {
	// Official split: input + cache in message_start, output in message_delta.
	var u usageInfo
	tapStreamLine(protocolAnthropic, []byte(`data: {"type":"message_start","message":{"id":"m","model":"x",`+
		`"usage":{"input_tokens":618,"cache_read_input_tokens":70912}}}`), &u)
	tapStreamLine(protocolAnthropic, []byte(`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":8}}`), &u)
	if u.Prompt != 618+70912 || u.Completion != 8 || u.CacheRead != 70912 {
		t.Fatalf("official split tap wrong: %+v", u)
	}
}

func TestTapOpenAIStreamUsage(t *testing.T) {
	// OpenAI prompt_tokens already includes cached tokens.
	var u usageInfo
	tapStreamLine(protocolOpenAI, []byte(`data: {"choices":[],"usage":{"prompt_tokens":100,"completion_tokens":7,`+
		`"prompt_tokens_details":{"cached_tokens":20}}}`), &u)
	if u.Prompt != 100 || u.Completion != 7 || u.CacheRead != 20 {
		t.Fatalf("openai tap wrong: %+v", u)
	}
}

func TestTapDeepSeekFlatCacheFields(t *testing.T) {
	// DeepSeek flat cache fields override prompt_tokens_details; prompt_tokens
	// already includes hit + miss.
	var u usageInfo
	tapStreamLine(protocolOpenAI, []byte(`data: {"choices":[],"usage":{"prompt_tokens":100,"completion_tokens":7,`+
		`"prompt_cache_hit_tokens":80,"prompt_cache_miss_tokens":20}}`), &u)
	if u.Prompt != 100 || u.Completion != 7 || u.CacheRead != 80 {
		t.Fatalf("deepseek tap wrong: %+v", u)
	}
}

func TestTapResponsesNonStreamBareObject(t *testing.T) {
	body := []byte(`{"id":"resp_1","object":"response","status":"completed","model":"x","output":[],` +
		`"usage":{"input_tokens":12,"output_tokens":6,"total_tokens":18,` +
		`"input_tokens_details":{"cached_tokens":4}}}`)
	u := tapNonStreamBody(protocolOpenAIResponses, body)
	if u.Prompt != 12 || u.Completion != 6 || u.CacheRead != 4 {
		t.Fatalf("responses non-stream tap wrong: %+v", u)
	}
}

func TestTapResponsesStreamCompleted(t *testing.T) {
	var u usageInfo
	tapStreamLine(protocolOpenAIResponses, []byte(`data: {"type":"response.completed","response":{"id":"r","status":"completed",`+
		`"usage":{"input_tokens":12,"output_tokens":6,"input_tokens_details":{"cached_tokens":4}}}}`), &u)
	if u.Prompt != 12 || u.Completion != 6 || u.CacheRead != 4 {
		t.Fatalf("responses stream tap wrong: %+v", u)
	}
}

// hasContentDelta gates the TTFT mark on the first frame carrying generated
// content; handshake frames (role chunk, message_start, ping, response.created)
// must not trip it.

func TestHasContentDeltaOpenAI(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		want    bool
	}{
		{"role-only chunk", `{"choices":[{"delta":{"role":"assistant"},"index":0}]}`, false},
		{"empty content", `{"choices":[{"delta":{"role":"assistant","content":""},"index":0}]}`, false},
		{"null content", `{"choices":[{"delta":{"content":null},"index":0}]}`, false},
		{"text delta", `{"choices":[{"delta":{"content":"Hi"},"index":0}]}`, true},
		{"reasoning delta", `{"choices":[{"delta":{"reasoning_content":"let me"},"index":0}]}`, true},
		{"tool calls", `{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1"}]},"index":0}]}`, true},
		{"no choices usage frame", `{"choices":[],"usage":{"prompt_tokens":1}}`, false},
		{"malformed falls back to content", `{not json`, true},
	}
	for _, tc := range cases {
		if got := hasContentDelta(protocolOpenAI, []byte(tc.payload)); got != tc.want {
			t.Errorf("%s: hasContentDelta(openai) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestHasContentDeltaAnthropic(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		want    bool
	}{
		{"message_start", `{"type":"message_start","message":{"id":"m"}}`, false},
		{"ping", `{"type":"ping"}`, false},
		{"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`, false},
		{"text delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hi"}}`, true},
		{"thinking delta", `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"hmm"}}`, true},
		{"input json delta", `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{}"}}`, true},
		{"message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"}}`, false},
		{"malformed falls back to content", `{not json`, true},
	}
	for _, tc := range cases {
		if got := hasContentDelta(protocolAnthropic, []byte(tc.payload)); got != tc.want {
			t.Errorf("%s: hasContentDelta(anthropic) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestHasContentDeltaResponses(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		want    bool
	}{
		{"response.created", `{"type":"response.created","response":{"id":"r"}}`, false},
		{"response.in_progress", `{"type":"response.in_progress","response":{"id":"r"}}`, false},
		{"output item added", `{"type":"response.output_item.added","output_index":0,"item":{"type":"message"}}`, false},
		{"output text delta", `{"type":"response.output_text.delta","delta":"Hi"}`, true},
		{"reasoning summary delta", `{"type":"response.reasoning_summary_text.delta","delta":"hmm"}`, true},
		{"function args delta", `{"type":"response.function_call_arguments.delta","delta":"{}"}`, true},
		{"malformed falls back to content", `{not json`, true},
	}
	for _, tc := range cases {
		if got := hasContentDelta(protocolResponses, []byte(tc.payload)); got != tc.want {
			t.Errorf("%s: hasContentDelta(responses) = %v, want %v", tc.name, got, tc.want)
		}
	}
}
