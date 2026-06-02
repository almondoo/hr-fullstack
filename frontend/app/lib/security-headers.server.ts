/**
 * security-headers.server.ts
 *
 * Returns a Headers object populated with recommended security headers.
 * Call this in the root loader and merge into the response.
 */

export function buildSecurityHeaders(): Record<string, string> {
  return {
    // Disallow embedding in frames (clickjacking protection)
    "X-Frame-Options": "DENY",
    // Prevent MIME-type sniffing
    "X-Content-Type-Options": "nosniff",
    // Restrict referrer information
    "Referrer-Policy": "strict-origin-when-cross-origin",
    // Permissions policy: deny sensitive APIs by default
    "Permissions-Policy": "camera=(), microphone=(), geolocation=()",
    // Content Security Policy
    // script-src: allow same-origin and inline scripts only from React Router
    // (nonce-based CSP is preferred in production, but requires server-side nonce injection)
    "Content-Security-Policy": [
      "default-src 'self'",
      "script-src 'self'",
      "style-src 'self' 'unsafe-inline'",
      "img-src 'self' data:",
      "font-src 'self'",
      "connect-src 'self'",
      "frame-ancestors 'none'",
      "base-uri 'self'",
      "form-action 'self'",
    ].join("; "),
  };
}
