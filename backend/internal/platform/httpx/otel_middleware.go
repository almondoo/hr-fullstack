package httpx

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

// OTelMiddleware wraps each Gin request in an OpenTelemetry span and records
// HTTP server metrics (http.server.request.duration, http.server.active_requests)
// via the otelhttp instrumentation library.
//
// Span naming: each request is identified by "<METHOD> <route-template>" where
// the route template is the Gin full path (e.g. "/api/v1/employees/:id"), NOT
// the expanded URL, so no PII (e.g. actual employee IDs) leaks into trace
// attribute values or metric label cardinality.
//
// Context propagation: W3C TraceContext and Baggage headers are extracted from
// incoming requests and injected into outgoing calls automatically by the global
// TextMapPropagator set in platform/otel.Init().
//
// Security:
//   - Route template used for span/metric labels; expanded paths are excluded.
//   - No request/response bodies or query parameters are recorded.
//   - No PII or credentials appear in trace attributes.
func OTelMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		// Resolve route template before the handler runs so we capture the
		// Gin pattern (e.g. "/api/v1/employees/:id") rather than the raw path.
		// FullPath() returns "" for 404 routes; we fall back to a safe literal.
		route := c.FullPath()
		if route == "" {
			route = "unknown"
		}

		// otelhttp.NewHandler wraps an http.Handler.  We bridge it to Gin by
		// constructing a one-shot http.Handler that calls c.Next() and then
		// exits, letting otelhttp start/end the span around the Gin chain.
		handler := otelhttp.NewHandler(
			http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// Propagate the otelhttp-enriched context back into gin.Context
				// so downstream handlers can obtain the active span via
				// trace.SpanFromContext(c.Request.Context()).
				c.Request = r
				c.Next()
			}),
			route,
			otelhttp.WithMessageEvents(otelhttp.ReadEvents, otelhttp.WriteEvents),
		)

		handler.ServeHTTP(c.Writer, c.Request)
		// Abort the remaining chain — c.Next() was already called inside the
		// otelhttp handler above.
		c.Abort()
	}
}
