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
