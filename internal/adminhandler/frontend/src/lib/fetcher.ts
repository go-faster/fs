// customFetch is the Orval mutator: every generated hook routes its request
// through here. It adapts Orval's axios-shaped request config onto the native
// fetch API, keeps requests same-origin, attaches the admin bearer token, and
// surfaces the admin API's structured `{ error_message }` body as a thrown
// Error.

import { clearToken, getToken } from "./auth";

export interface RequestConfig {
  url: string;
  // Orval emits upper-case verbs (e.g. "GET"); accept any casing.
  method: string;
  params?: Record<string, unknown>;
  data?: unknown;
  headers?: Record<string, string>;
  responseType?: string;
  signal?: AbortSignal;
}

export class ApiError extends Error {
  readonly status: number;
  constructor(message: string, status: number) {
    super(message);
    this.name = "ApiError";
    this.status = status;
  }
}

function buildUrl(url: string, params?: Record<string, unknown>): string {
  if (!params) return url;
  const usp = new URLSearchParams();
  for (const [k, v] of Object.entries(params)) {
    if (v === undefined || v === null) continue;
    usp.append(k, String(v));
  }
  const qs = usp.toString();
  return qs ? `${url}?${qs}` : url;
}

export const customFetch = async <T>(config: RequestConfig): Promise<T> => {
  const { url, method, params, data, headers, signal } = config;

  const token = getToken();

  const init: RequestInit = {
    method: method.toUpperCase(),
    signal,
    headers: {
      Accept: "application/json",
      ...(data !== undefined ? { "Content-Type": "application/json" } : {}),
      ...(token ? { Authorization: `Bearer ${token}` } : {}),
      ...headers,
    },
  };
  if (data !== undefined) {
    init.body = typeof data === "string" ? data : JSON.stringify(data);
  }

  const res = await fetch(buildUrl(url, params), init);

  let body: unknown = null;
  const text = await res.text();
  if (text) {
    try {
      body = JSON.parse(text);
    } catch {
      body = text;
    }
  }

  if (!res.ok) {
    // A rejected token is stale/wrong: drop it so the login gate reappears.
    if (res.status === 401) {
      clearToken();
    }
    const err = body as { error_message?: string } | null;
    throw new ApiError(
      err?.error_message || res.statusText || `HTTP ${res.status}`,
      res.status,
    );
  }

  return body as T;
};

export default customFetch;
