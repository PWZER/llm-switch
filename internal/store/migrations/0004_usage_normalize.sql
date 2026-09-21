-- Normalize historical usage rows to the total-input convention:
-- prompt_tokens now INCLUDES cache_read_tokens + cache_write_tokens
-- (OpenAI convention). Rows served by an anthropic upstream were logged with
-- the Anthropic wire semantics (input_tokens excludes cache); fold the cache
-- columns in. Rows from openai upstreams already follow the convention.
-- Rows whose cache fields were never captured stay unchanged (zeros).
UPDATE request_logs
SET prompt_tokens = prompt_tokens + cache_read_tokens + cache_write_tokens
WHERE protocol_out = 'anthropic';
