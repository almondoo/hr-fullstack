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
//   - When OTelEnabled is false OR OTLPEndpoint is empty, registers no-op providers
//     so the rest of the application remains instrumented without any I/O.
//   - Returns a Shutdown function the caller (main.go) must defer to flush and
//     cleanly shut down the exporters before the process exits.
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
// # Pending work (cloud-provider selection, GAP-01)
//
// Aggregation backend (Prometheus scrape endpoint vs push), SLO monitoring, and
// alert routing (PagerDuty / Slack) depend on the deployment target and are
// deferred until GAP-01 (deploy-target decision) is resolved.  This scaffold
// provides the SDK plumbing; exporter choice is a one-line swap in init().
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
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/metric"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
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
// Parameters:
//   - enabled: true to activate OTLP exporters; false for no-op providers.
//   - endpoint: OTLP/HTTP base URL (e.g. "https://collector:4318").
//   - serviceName: identifies this service in traces/metrics.
//   - logger: used for startup diagnostics only; never logs credentials or PII.
//
// Returns a ShutdownFunc that the caller must invoke on application exit.
// When Init returns an error, it also returns a best-effort ShutdownFunc (may
// be a no-op) so the caller can always defer safely.
func Init(ctx context.Context, enabled bool, endpoint, serviceName string, logger *slog.Logger) (ShutdownFunc, error) {
	if !enabled || endpoint == "" {
		// No-op providers: instrumentation code compiles and runs without I/O.
		// This is the safe default for development and when the endpoint is
		// not yet configured.
		otel.SetTracerProvider(tracenoop.NewTracerProvider())
		otel.SetMeterProvider(metricnoop.NewMeterProvider())
		otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
			propagation.TraceContext{},
			propagation.Baggage{},
		))
		logger.Info("otel: disabled (no-op providers registered)",
			"enabled", enabled,
			"endpoint_set", endpoint != "",
		)
		return func(_ context.Context) error { return nil }, nil
	}

	res, err := resource.New(ctx,
		resource.WithSchemaURL(semconv.SchemaURL),
		resource.WithAttributes(
			semconv.ServiceNameKey.String(serviceName),
		),
		resource.WithProcessPID(),
		resource.WithHost(),
	)
	if err != nil {
		// resource.New merges defaults; a partial error is non-fatal — continue
		// with whatever was built, but log the issue.
		logger.Warn("otel: resource build returned partial error; continuing", "error", err)
		if res == nil {
			res = resource.Default()
		}
	}

	// --- Trace exporter (OTLP/HTTP) ---
	traceExp, err := otlptracehttp.New(ctx,
		otlptracehttp.WithEndpointURL(endpoint),
	)
	if err != nil {
		return noopShutdown, err
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(traceExp,
			sdktrace.WithBatchTimeout(5*time.Second),
		),
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithResource(res),
	)

	// --- Metric exporter (OTLP/HTTP) ---
	metricExp, err := otlpmetrichttp.New(ctx,
		otlpmetrichttp.WithEndpointURL(endpoint),
	)
	if err != nil {
		// Shut down the already-started trace provider before returning.
		_ = tp.Shutdown(ctx)
		return noopShutdown, err
	}

	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(metricExp,
			sdkmetric.WithInterval(60*time.Second),
		)),
		sdkmetric.WithResource(res),
	)

	// --- Global propagator (W3C TraceContext + Baggage) ---
	otel.SetTracerProvider(tp)
	otel.SetMeterProvider(mp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	logger.Info("otel: providers initialised",
		"service", serviceName,
		// Intentionally log only that an endpoint was set, never its value,
		// to prevent leaking credentials embedded in the URL.
		"endpoint_configured", true,
	)

	return func(ctx context.Context) error {
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

// noopShutdown is a no-op ShutdownFunc used in error paths where a real
// provider was never registered.
func noopShutdown(_ context.Context) error { return nil }
