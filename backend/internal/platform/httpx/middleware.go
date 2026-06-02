// Package httpx provides shared HTTP middleware and response helpers for the
// Gin router.
package httpx

import (
	"log/slog"
	"net/http"
	"regexp"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// contextKey is the unexported type for values stored in gin.Context.
type contextKey string

const (
	// RequestIDKey is the key used to store/retrieve the request ID in
	// gin.Context (Keys map) and response header.
	RequestIDKey contextKey = "request_id"

	// RequestIDHeader is the HTTP header name for request correlation.
	RequestIDHeader = "X-Request-ID"

	// requestIDMaxLen is the maximum accepted length of a client-supplied
	// X-Request-ID value. Values longer than this are rejected and replaced
	// with a freshly-generated UUID to prevent log injection / header bloat.
	requestIDMaxLen = 128
)

// requestIDSafeRe matches strings composed entirely of alphanumeric characters,
// hyphens, underscores, and dots — the safe subset for correlation IDs.
// Characters outside this set (spaces, control chars, slashes, …) are rejected.
var requestIDSafeRe = regexp.MustCompile(`^[A-Za-z0-9\-_.]+$`)

// validateRequestID returns id if it is non-empty, within requestIDMaxLen,
// and matches requestIDSafeRe.  Otherwise it returns a newly-generated UUID.
func validateRequestID(id string) string {
	if id != "" && len(id) <= requestIDMaxLen && requestIDSafeRe.MatchString(id) {
		return id
	}
	return uuid.New().String()
}

// RequestID is a Gin middleware that:
//  1. Reads X-Request-ID from the incoming request header if present.
//  2. Validates the value (length ≤ 128, safe character set only).
//  3. Generates a new UUID v4 when the header is absent or fails validation.
//  4. Stores the validated/generated ID in gin.Context under RequestIDKey so
//     downstream handlers can retrieve it via GetRequestID.
//  5. Echoes the validated/generated ID back to the client in the response header.
//
// Client-supplied values that fail validation are silently replaced — the client
// receives the generated UUID in the response header.
// The forwarded/generated ID is safe to log (it contains no PII).
func RequestID() gin.HandlerFunc {
	return func(c *gin.Context) {
		id := validateRequestID(c.GetHeader(RequestIDHeader))
		c.Set(string(RequestIDKey), id)
		c.Header(RequestIDHeader, id)
		c.Next()
	}
}

// GetRequestID retrieves the request ID stored by the RequestID middleware.
// Returns an empty string when the middleware was not applied.
func GetRequestID(c *gin.Context) string {
	if v, ok := c.Get(string(RequestIDKey)); ok {
		if id, ok := v.(string); ok {
			return id
		}
	}
	return ""
}

// RequestLogger returns a Gin middleware that logs one structured line per
// request.
//
// Logged fields: method, path, status, duration_ms, request_id.
// Fields intentionally omitted to prevent PII / secret leakage:
//   - Query string parameters (may contain tokens, user-supplied data)
//   - Request / response body
//   - Authorization / Cookie headers
//   - Remote IP address (PII in some jurisdictions)
func RequestLogger(logger *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()

		logger.Info("request",
			"method", c.Request.Method,
			"path", c.Request.URL.Path,
			"status", c.Writer.Status(),
			"duration_ms", time.Since(start).Milliseconds(),
			"request_id", GetRequestID(c),
		)
	}
}

// SecurityHeaders sets supplemental HTTP security headers that are NOT covered
// by gin-contrib/secure (which owns X-Frame-Options, X-Content-Type-Options,
// HSTS, X-XSS-Protection, and CSP).
//
// Currently sets:
//   - Cross-Origin-Opener-Policy: same-origin — isolates the browsing context
//     group to prevent cross-origin window references (Spectre mitigation).
//
// Headers already managed by gin-contrib/secure are intentionally absent here
// to avoid duplicate / conflicting values in the response.
func SecurityHeaders() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("Cross-Origin-Opener-Policy", "same-origin")
		c.Next()
	}
}

// --- Unified JSON error response ---

// ErrorResponse is the canonical JSON envelope for all API errors.
// Clients should key on `code` (a stable machine-readable string) rather
// than `message` (human-readable, may be localised in a later slice).
type ErrorResponse struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// RespondError writes a JSON error response with the given HTTP status.
// Example:
//
//	httpx.RespondError(c, http.StatusNotFound, "NOT_FOUND", "resource not found")
func RespondError(c *gin.Context, status int, code, message string) {
	c.JSON(status, ErrorResponse{Code: code, Message: message})
}

// RespondInternalError writes a generic 500 response. The real error is NOT
// exposed to the client to prevent information leakage; log it server-side.
func RespondInternalError(c *gin.Context) {
	RespondError(c, http.StatusInternalServerError, "INTERNAL_ERROR", "an internal error occurred")
}
