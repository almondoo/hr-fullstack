/**
 * api.server.ts — Server-only helper for calling the Go API.
 *
 * Responsibilities:
 * 1. Forward the browser's session Cookie to the Go API (BFF pattern).
 * 2. Forward the Go API's Set-Cookie header back to the browser.
 * 3. Fetch a CSRF token before state-changing requests and attach it
 *    as X-CSRF-Token.
 *
 * This file MUST NOT be imported by client-side code.
 * The ".server" suffix causes React Router to exclude it from client bundles.
 */

const API_URL = process.env["API_URL"] ?? "http://localhost:8080";

// ---------------------------------------------------------------------------
// Types
// ---------------------------------------------------------------------------

export interface ApiResponse<T> {
  data: T | null;
  /** HTTP status code returned by Go API */
  status: number;
  /** Set-Cookie headers to forward to the browser */
  setCookieHeaders: string[];
  ok: boolean;
}

export interface MeResponse {
  user: {
    id: string;
    email: string;
    displayName: string;
  };
  tenant: {
    id: string;
    name: string;
    slug: string;
  };
  role: string;
  permissions: string[];
}

export interface CsrfResponse {
  token: string;
}

// ---------------------------------------------------------------------------
// Internal helpers
// ---------------------------------------------------------------------------

/**
 * Build common headers forwarding the browser Cookie to the Go API.
 */
function buildHeaders(
  incomingCookie: string | null,
  extra: Record<string, string> = {},
): Headers {
  const headers = new Headers({
    "Content-Type": "application/json",
    Accept: "application/json",
  });
  if (incomingCookie) {
    headers.set("Cookie", incomingCookie);
  }
  for (const [key, value] of Object.entries(extra)) {
    headers.set(key, value);
  }
  return headers;
}

/**
 * Extract all Set-Cookie header values from a Response so they can be
 * forwarded to the browser via React Router's action/loader response headers.
 */
function extractSetCookies(res: Response): string[] {
  // getSetCookie() is available in Node 18+
  const raw = res.headers.getSetCookie?.();
  if (raw) return raw;
  // Fallback: headers.get("set-cookie") may return a comma-joined string
  const fallback = res.headers.get("set-cookie");
  return fallback ? [fallback] : [];
}

// ---------------------------------------------------------------------------
// CSRF
// ---------------------------------------------------------------------------

/**
 * Fetch a fresh CSRF token from Go API.
 * The CSRF cookie is set by Go API; we capture it to forward alongside
 * the token value.
 */
export async function fetchCsrfToken(
  incomingCookie: string | null,
): Promise<{ token: string; setCookieHeaders: string[] }> {
  const res = await fetch(`${API_URL}/api/v1/csrf`, {
    method: "GET",
    headers: buildHeaders(incomingCookie),
  });

  if (!res.ok) {
    throw new Error(`CSRF fetch failed: ${res.status}`);
  }

  const body = (await res.json()) as CsrfResponse;
  return {
    token: body.token,
    setCookieHeaders: extractSetCookies(res),
  };
}

// ---------------------------------------------------------------------------
// Auth endpoints
// ---------------------------------------------------------------------------

export async function apiLogin(
  incomingCookie: string | null,
  payload: { slug: string; email: string; password: string },
): Promise<ApiResponse<null>> {
  const { token, setCookieHeaders: csrfCookies } =
    await fetchCsrfToken(incomingCookie);

  const res = await fetch(`${API_URL}/api/v1/auth/login`, {
    method: "POST",
    headers: buildHeaders(incomingCookie, { "X-CSRF-Token": token }),
    body: JSON.stringify(payload),
  });

  const allSetCookies = [...csrfCookies, ...extractSetCookies(res)];

  return {
    data: null,
    status: res.status,
    setCookieHeaders: allSetCookies,
    ok: res.ok,
  };
}

export async function apiLogout(
  incomingCookie: string | null,
): Promise<ApiResponse<null>> {
  const { token, setCookieHeaders: csrfCookies } =
    await fetchCsrfToken(incomingCookie);

  const res = await fetch(`${API_URL}/api/v1/auth/logout`, {
    method: "POST",
    headers: buildHeaders(incomingCookie, { "X-CSRF-Token": token }),
  });

  const allSetCookies = [...csrfCookies, ...extractSetCookies(res)];

  return {
    data: null,
    status: res.status,
    setCookieHeaders: allSetCookies,
    ok: res.ok,
  };
}

export async function apiMe(
  incomingCookie: string | null,
): Promise<ApiResponse<MeResponse>> {
  const res = await fetch(`${API_URL}/api/v1/auth/me`, {
    method: "GET",
    headers: buildHeaders(incomingCookie),
  });

  if (!res.ok) {
    return {
      data: null,
      status: res.status,
      setCookieHeaders: extractSetCookies(res),
      ok: false,
    };
  }

  const data = (await res.json()) as MeResponse;
  return {
    data,
    status: res.status,
    setCookieHeaders: extractSetCookies(res),
    ok: true,
  };
}
