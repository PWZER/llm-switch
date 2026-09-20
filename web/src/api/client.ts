// Thin fetch wrapper around the admin API envelope {code,msg,data,request_id}.

const TOKEN_KEY = 'lsw_admin_token';

export function getToken(): string {
  return localStorage.getItem(TOKEN_KEY) ?? '';
}

export function setToken(token: string) {
  localStorage.setItem(TOKEN_KEY, token);
}

export function clearToken() {
  localStorage.removeItem(TOKEN_KEY);
}

export class ApiError extends Error {
  code: number;
  constructor(code: number, msg: string) {
    super(msg);
    this.code = code;
  }
}

async function request<T>(method: string, path: string, body?: unknown): Promise<T> {
  const headers: Record<string, string> = {};
  const token = getToken();
  if (token) headers['Authorization'] = `Bearer ${token}`;
  if (body !== undefined) headers['Content-Type'] = 'application/json';

  const resp = await fetch(path, {
    method,
    headers,
    body: body !== undefined ? JSON.stringify(body) : undefined,
  });
  if (resp.status === 401) {
    clearToken();
    window.location.hash = '#/login';
    throw new ApiError(40101, 'unauthorized');
  }
  const env = (await resp.json()) as { code: number; msg: string; data: T };
  if (env.code !== 0) throw new ApiError(env.code, env.msg);
  return env.data;
}

export const api = {
  get: <T>(path: string) => request<T>('GET', path),
  post: <T>(path: string, body?: unknown) => request<T>('POST', path, body ?? {}),
  put: <T>(path: string, body?: unknown) => request<T>('PUT', path, body ?? {}),
  del: <T>(path: string) => request<T>('DELETE', path),
};

// — shared types -----------------------------------------------------------

export interface UsageProbe {
  type: 'balance' | 'plan';
  path: string;
  name?: string;
  auth_style?: 'bearer' | 'raw';
}

export interface Provider {
  id: number;
  name: string;
  /** Absolute models-list fetch URL; null = not configured. */
  models_url: string | null;
  enabled: boolean;
  created_at: number;
  updated_at: number;
}

/** One upstream credential set of a provider; the API key is an attribute. */
export interface Account {
  id: number;
  provider_id: number;
  label: string;
  api_key_mask: string;
  weight: number;
  enabled: boolean;
  usage_probes: UsageProbe[];
}

export interface Channel {
  id: number;
  provider_id: number;
  provider_name: string;
  name: string;
  protocol: 'openai' | 'anthropic';
  base_url: string;
  chat_path: string;
  auth_style: 'bearer' | 'x-api-key';
  responses_path: string | null;
  extra_headers: string;
  enabled: boolean;
  priority: number;
  weight: number;
  supports_embeddings: boolean;
  passthrough: boolean;
  force_upstream_stream: boolean;
}

// — usage / balance probes ---------------------------------------------------

export interface UsageWindow {
  name: string;
  window?: string;
  used_percent?: number;
  remaining_percent?: number;
  unlimited?: boolean;
  resets_at?: number; // unix seconds
  unit?: string; // quota unit, e.g. CREDIT
  used_amount?: number;
  limit_amount?: number;
  note?: string; // raw numbers when percent semantics were unclassifiable
}

export interface UsageBalance {
  currency?: string;
  total?: number;
  available?: number;
  granted?: number;
  voucher?: number;
  cash?: number;
}

export interface ProbeResult {
  probe: string;
  type: 'balance' | 'plan';
  path?: string;
  ok: boolean;
  status?: number;
  error?: string;
  plan?: string;
  balance?: UsageBalance;
  windows?: UsageWindow[];
  raw?: string;
}

export interface AccountUsageReport {
  account_id: number;
  provider_id: number;
  label: string;
  key_mask: string;
  queried_at: number; // unix seconds
  configured: boolean;
  results: ProbeResult[];
}

/** Two-tier account availability test result (quick = /models, deep = mini chat). */
export interface AccountTestResult {
  ok: boolean;
  class?: string; // ok|validation|auth|model|path|unreachable
  status?: number;
  error?: string;
  model?: string;
  key_mask?: string;
  depth: 'quick' | 'deep';
  model_count?: number;
  models?: string[];
  dns_ms?: number;
  connect_ms?: number;
  tls_ms?: number;
  first_byte_ms?: number;
  total_ms?: number;
}

/** One models-table row: a provider serving a client-facing model name. */
export interface Model {
  id: string;
  provider_id: number;
  /** Upstream alias; empty = identity (upstream gets `id`). */
  upstream_model: string;
  display_name: string;
  enabled: boolean;
  context_window: number | null;
  max_output_tokens: number | null;
  /** Enriched by the admin list endpoint. */
  provider_name?: string;
  provider_enabled?: boolean;
}

export interface ModelRouteTarget {
  /** The target's provider; always set. */
  provider_id: number;
  /** Pinned endpoint; null = auto-select among the provider's enabled endpoints. */
  channel_id: number | null;
  upstream_model: string;
  /** Pin the target to one account of the target's provider; 0/absent = pool rotation. */
  account_id?: number;
}

export interface ModelRoute {
  name: string;
  targets: ModelRouteTarget[];
  updated_at: number;
}

export interface ClientKey {
  id: number;
  name: string;
  prefix: string;
  enabled: boolean;
  token_limit: number | null;
  expires_at: number | null;
  created_at: number;
  last_used_at: number | null;
}

export interface RequestLog {
  id: number;
  ts: number;
  request_id: string;
  api_key_name: string;
  provider_name: string;
  account_id: number | null;
  account_name: string;
  channel_name: string;
  model: string;
  upstream_model: string;
  protocol_in: string;
  protocol_out: string;
  stream: boolean;
  status: number;
  success: boolean;
  error_type: string | null;
  attempts: number;
  prompt_tokens: number;
  completion_tokens: number;
  cache_read_tokens: number;
  cache_write_tokens: number;
  reasoning_tokens: number;
  latency_ms: number;
  first_token_ms: number | null;
}
