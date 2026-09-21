-- 0006: drop request_logs.channel_protocol — it duplicates protocol_out
-- (both record the serving channel's protocol), and with one endpoint per
-- (provider, protocol) the pair (provider_name, protocol_out) already
-- identifies the endpoint.
ALTER TABLE request_logs DROP COLUMN channel_protocol;
