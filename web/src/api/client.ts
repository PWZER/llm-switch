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

export interface Provider {
  id: number;
  name: string;
  enabled: boolean;
  created_at: number;
  updated_at: number;
}

export interface ProviderKey {
  id: number;
  provider_id: number;
  label: string;
  api_key_mask: string;
  weight: number;
  enabled: boolean;
}

export interface ChannelModel {
  channel_id?: number;
  model: string;
  upstream_model: string;
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
  models_url: string | null;
  extra_headers: string;
  enabled: boolean;
  priority: number;
  weight: number;
  auto_bind: boolean;
  supports_embeddings: boolean;
  passthrough: boolean;
  force_upstream_stream: boolean;
  models: ChannelModel[];
}

export interface Model {
  id: string;
  display_name: string;
  source: string;
  enabled: boolean;
  context_window: number | null;
  max_output_tokens: number | null;
}

export interface Alias {
  name: string;
  channel_id: number;
  upstream_model: string;
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
