// Minimal client-side token store. The admin API is protected by a bearer
// token; the operator pastes it once and it is kept in localStorage so the
// dashboard survives reloads. It never leaves the browser except as the
// Authorization header on same-origin API calls.

const TOKEN_KEY = "fs-admin-token";

const listeners = new Set<() => void>();

export function getToken(): string {
  return localStorage.getItem(TOKEN_KEY) ?? "";
}

export function setToken(token: string): void {
  if (token) {
    localStorage.setItem(TOKEN_KEY, token);
  } else {
    localStorage.removeItem(TOKEN_KEY);
  }
  listeners.forEach((fn) => fn());
}

export function clearToken(): void {
  setToken("");
}

// subscribe registers a callback fired whenever the token changes, so React can
// re-render the auth gate.
export function subscribe(fn: () => void): () => void {
  listeners.add(fn);
  return () => listeners.delete(fn);
}
