// Package otel initialises OpenTelemetry SDK providers (TracerProvider and
// MeterProvider) for the application.
//
// # Design
//
// The package exposes a single entry-point, Init(), that:
//   - Reads exporter configuration (OTLP_ENDPOINT) from the application Config.
//   - When OTelEnabled is true AND OTLPEndpoint is non-empty, configures OTLP/HTTP
//     exporters targeting that endpoint (e.g. an OpenTelemetry Collector or a
//     vendor-hosted endpoint such as Grafana Cloud, Google Cloud Trace, Datadog, …).
//   - When OTelEnabled is false OR OTLPEndpoint is empty, the TracerProvider is
//     no-op and only the Prometheus scrape exporter is active (no OTLP push I/O).
//   - Returns a Shutdown function the caller (main.go) must defer to flush and
//     cleanly shut down the exporters before the process exits.
//
// A Prometheus scrape exporter is ALWAYS registered on the MeterProvider
// (regardless of OTelEnabled / OTLP endpoint) so that the /metrics endpoint
// is available for local Prometheus or AWS CloudWatch agent scraping.
//
// # OTLP endpoint configuration
//
//	OTEL_ENABLED=true              # default false
//	OTEL_SERVICE_NAME=hr-saas      # default "hr-saas"
//	OTEL_EXPORTER_OTLP_ENDPOINT=https://collector:4318   # OTLP/HTTP endpoint
//
// The endpoint value is a placeholder; inject the real value from your secret
// manager / environment in non-development deployments.  Never hard-code it.
//
// # Prometheus scrape endpoint
//
// Init returns a non-nil http.Handler regardless of OTelEnabled.
// The caller (server.go) must mount it at GET /metrics.
// The handler collects all SDK metrics including otelhttp HTTP server metrics.
//
// # Security / PII policy
//
//   - No PII or credentials are written to trace/metric attributes.
//   - Span names and metric labels use only HTTP method, route template (not
//     expanded paths), and numeric status codes.
//   - OTLP_ENDPOINT must be injected from a secrets manager; it is never logged.
package otel

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	promclient "github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	promexporter "go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

// ShutdownFunc must be called (typically via defer) to flush pending telemetry
// and cleanly shut down providers before the process exits.
type ShutdownFunc func(context.Context) error

// Init sets up global OTel TracerProvider and MeterProvider.
//
// A Prometheus scrape exporter is always registered on the MeterProvider.
// The returned http.Handler must be mounted by the caller at GET /metrics.
//
// Parameters:
//   - enabled: true to activate OTLP push exporters in addition to Prometheus.
//   - endpoint: OTLP/HTTP base URL (e.g. "https://collector:4318").
//   - serviceName: identifies this service in traces/metrics.
//   - logger: used for startup diagnostics only; never logs credentials or PII.
//
// Returns (prometheusHandler, shutdownFunc, error).
// prometheusHandler is always non-nil.
// When Init returns an error, it also returns a best-effort ShutdownFunc (may
// be a no-op) so the caller can always defer safely.
func Init(ctx context.Context, enabled bool, endpoint, serviceName string, logger *slog.Logger) (http.Handler, ShutdownFunc, error) {
	// Build a dedicated Prometheus registry so we do not pollute the global
	// prometheus.DefaultRegisterer with collector conflicts on test re-runs.
	promRegistry := promclient.NewRegistry()
	promRegistry.MustRegister(
		promclient.NewGoCollector(),
		promclient.NewProcessCollector(promclient.ProcessCollectorOpts{}),
	)

	// Prometheus pull exporter — always enabled (scrape-based, zero I/O unless
	// a scraper connects).
	promExp, err := promexporter.New(
		promexporter.WithRegisterer(promRegistry),
	)
	if err != nil {
		return http.NotFoundHandler(), noopShutdown, err
	}

	metricsHandler := promhttp.HandlerFor(promRegistry, promhttp.HandlerOpts{
		EnableOpenMetrics: true,
	})

	if !enabled || endpoint == "" {
		// No OTLP push: register a MeterProvider backed only by the Prometheus
		// exporter so /metrics still works without I/O to an external collector.
		res, _ := buildResource(ctx, serviceName, logger)
		mp := sdkmetric.NewMeterProvider(
			sdkmetric.WithReader(promExp),
			sdkmetric.WithResource(res),
		)
		otel.SetTracerProvider(tracenoop.NewTracerProvider())
		otel.SetMeterProvider(mp)
		otel.SetTextMapPropagator(w3cPropagator())
		logger.Info("otel: OTLP push disabled; Prometheus scrape exporter active",
			"enabled", enabled,
			"endpoint_set", endpoint != "",
		)
		return metricsHandler, func(ctx context.Context) error {
			return mp.Shutdown(ctx)
		}, nil
	}

	res, _ := buildResource(ctx, serviceName, logger)

	// --- Trace exporter (OTLP/HTTP push) ---
	traceExp, err := otlptracehttp.New(ctx,
		otlptracehttp.WithEndpointURL(endpoint),
	)
	if err != nil {
		return metricsHandler, noopShutdown, err
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(traceExp,
			sdktrace.WithBatchTimeout(5*time.Second),
		),
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithResource(res),
	)

	// --- Metric exporters: OTLP/HTTP push + Prometheus pull ---
	metricExp, err := otlpmetrichttp.New(ctx,
		otlpmetrichttp.WithEndpointURL(endpoint),
	)
	if err != nil {
		// Shut down the already-started trace provider before returning.
		_ = tp.Shutdown(ctx)
		return metricsHandler, noopShutdown, err
	}

	mp := sdkmetric.NewMeterProvider(
		// OTLP push reader — periodic export to the configured collector.
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(metricExp,
			sdkmetric.WithInterval(60*time.Second),
		)),
		// Prometheus pull reader — exposes /metrics for scraping.
		sdkmetric.WithReader(promExp),
		sdkmetric.WithResource(res),
	)

	// --- Global propagator (W3C TraceContext + Baggage) ---
	otel.SetTracerProvider(tp)
	otel.SetMeterProvider(mp)
	otel.SetTextMapPropagator(w3cPropagator())

	logger.Info("otel: providers initialised (OTLP push + Prometheus scrape)",
		"service", serviceName,
		// Intentionally log only that an endpoint was set, never its value,
		// to prevent leaking credentials embedded in the URL.
		"endpoint_configured", true,
	)

	return metricsHandler, func(ctx context.Context) error {
		var errs []error
		if err := tp.Shutdown(ctx); err != nil {
			errs = append(errs, err)
		}
		if err := mp.Shutdown(ctx); err != nil {
			errs = append(errs, err)
		}
		return errors.Join(errs...)
	}, nil
}

// Tracer returns a named tracer from the global TracerProvider.
// Convenience wrapper so callers do not need to import go.opentelemetry.io/otel
// directly when they only need a tracer.
func Tracer(name string) trace.Tracer {
	return otel.Tracer(name)
}

// Meter returns a named meter from the global MeterProvider.
// Convenience wrapper so callers do not need to import the metric package.
func Meter(name string) metric.Meter {
	return otel.Meter(name)
}

// buildResource constructs an OTel resource with service name, PID, and host.
// Partial errors from resource.New are non-fatal (defaults are merged in); the
// function always returns a usable resource.
func buildResource(ctx context.Context, serviceName string, logger *slog.Logger) (*resource.Resource, error) {
	res, err := resource.New(ctx,
		resource.WithSchemaURL(semconv.SchemaURL),
		resource.WithAttributes(
			semconv.ServiceNameKey.String(serviceName),
		),
		resource.WithProcessPID(),
		resource.WithHost(),
	)
	if err != nil {
		logger.Warn("otel: resource build returned partial error; continuing", "error", err)
		if res == nil {
			res = resource.Default()
		}
	}
	return res, err
}

// w3cPropagator returns the W3C TraceContext + Baggage composite propagator
// used as the global TextMapPropagator.
func w3cPropagator() propagation.TextMapPropagator {
	return propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	)
}

// noopShutdown is a no-op ShutdownFunc used in error paths where a real
// provider was never registered.
func noopShutdown(_ context.Context) error { return nil }
