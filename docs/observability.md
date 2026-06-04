# Observability: Metrics / Traces / Logs / Alerting

## Overview

The HR SaaS backend is instrumented with OpenTelemetry SDK (Go).  All three
signal types — traces, metrics, and structured logs — are wired at startup
and operate in no-op mode by default so that local development requires no
external services.

| Signal   | Library                              | Default state         |
|----------|--------------------------------------|-----------------------|
| Traces   | `go.opentelemetry.io/otel/sdk/trace` | No-op (OTEL_ENABLED=false) |
| Metrics  | `go.opentelemetry.io/otel/sdk/metric`| Prometheus pull always active |
| Logs     | `log/slog` (structured JSON)         | Always active         |

---

## Environment variables

| Variable                       | Default       | Purpose                                                  |
|--------------------------------|---------------|----------------------------------------------------------|
| `OTEL_ENABLED`                 | `false`       | Activates OTLP push exporters (traces + metrics)         |
| `OTEL_SERVICE_NAME`            | `hr-saas`     | `service.name` resource attribute on all spans/metrics   |
| `OTEL_EXPORTER_OTLP_ENDPOINT`  | _(empty)_     | OTLP/HTTP base URL — inject from secrets manager         |

When `OTEL_ENABLED=false` **or** `OTEL_EXPORTER_OTLP_ENDPOINT` is empty, the
trace provider is no-op and no OTLP push I/O occurs.  The Prometheus scrape
exporter remains active regardless.

**Security**: never hard-code the endpoint URL or any credentials.  Inject
from AWS Secrets Manager / Parameter Store or equivalent.

---

## Prometheus scrape endpoint (`/metrics`)

`GET /metrics` is always mounted on the server and exposes metrics in
OpenMetrics format.  It includes:

- `http_server_request_duration_seconds` — HTTP request latency histogram
  (labels: `http_request_method`, `http_route`, `http_response_status_code`)
- `http_server_active_requests` — in-flight request gauge
- `go_*` — Go runtime metrics (GC, goroutines, memory)
- `process_*` — process-level metrics (CPU, file descriptors)

**Access control**: restrict `/metrics` at the infrastructure layer.
Example ALB listener rule (do not expose publicly):

```yaml
# aws_lb_listener_rule (Terraform) — block /metrics from public listener
condition:
  path_pattern:
    values: ["/metrics"]
action:
  type: fixed-response
  fixed_response:
    content_type: "text/plain"
    message_body: "Forbidden"
    status_code: "403"
```

For CloudWatch agent scraping (ECS/EC2), add a scrape target pointing to the
container's internal port (not the public ALB):

```yaml
# /opt/aws/amazon-cloudwatch-agent/etc/amazon-cloudwatch-agent.json (excerpt)
{
  "metrics": {
    "metrics_collected": {
      "prometheus": {
        "prometheus_config_path": "/etc/prometheus/prometheus.yaml",
        "emf_processor": {
          "metric_namespace": "HRSaaS/App"
        }
      }
    }
  }
}
```

```yaml
# /etc/prometheus/prometheus.yaml (on the CloudWatch agent host / sidecar)
scrape_configs:
  - job_name: hr-saas
    scrape_interval: 60s
    static_configs:
      - targets: ["localhost:8080"]  # replace with actual container address
    metrics_path: /metrics
```

---

## OTLP push (traces + metrics) — staging/production

Set these environment variables in the task definition / pod spec.
**Values are placeholders — never commit real endpoints or keys.**

```bash
OTEL_ENABLED=true
OTEL_SERVICE_NAME=hr-saas
# Inject from AWS Secrets Manager / SSM Parameter Store:
OTEL_EXPORTER_OTLP_ENDPOINT=https://<collector-or-vendor-endpoint>:4318
```

### Supported backends (exporter is OTLP/HTTP — swap config, not code)

| Backend                  | Endpoint format                                      |
|--------------------------|------------------------------------------------------|
| Self-hosted OTel Collector | `http://collector:4318`                            |
| Grafana Cloud            | `https://<stack>.grafana.net/otlp` + headers        |
| AWS Distro for OTel (ADOT) | `http://localhost:4318` (sidecar pattern)          |
| Datadog                  | `https://trace.agent.datadoghq.com` + API key header|

For backends requiring authentication headers, run a local OTel Collector
sidecar that injects credentials and forwards to the vendor.  Example
Collector config skeleton (values from environment; never commit credentials):

```yaml
# otel-collector-config.yaml (sidecar, not committed — reference only)
receivers:
  otlp:
    protocols:
      http:
        endpoint: "0.0.0.0:4318"

exporters:
  otlphttp/vendor:
    endpoint: "${VENDOR_OTLP_ENDPOINT}"
    headers:
      Authorization: "Bearer ${VENDOR_API_KEY}"

service:
  pipelines:
    traces:
      receivers: [otlp]
      exporters: [otlphttp/vendor]
    metrics:
      receivers: [otlp]
      exporters: [otlphttp/vendor]
```

---

## AWS CloudWatch — metrics and logs

### Metrics via Prometheus scrape (CloudWatch agent)

See the scrape config in the section above.  The CloudWatch agent converts
Prometheus metrics to CloudWatch EMF (Embedded Metric Format) so they appear
as CloudWatch custom metrics under the `HRSaaS/App` namespace.

### Logs

The backend emits structured JSON via `log/slog`.  In ECS, ship logs to
CloudWatch Logs with the `awslogs` log driver:

```json
{
  "logDriver": "awslogs",
  "options": {
    "awslogs-group": "/ecs/hr-saas",
    "awslogs-region": "ap-northeast-1",
    "awslogs-stream-prefix": "backend"
  }
}
```

Use CloudWatch Logs Insights for ad-hoc queries:

```
fields @timestamp, @message
| filter level = "ERROR"
| sort @timestamp desc
| limit 100
```

---

## SLO definitions (indicative — confirm with stakeholders)

| SLO                          | Target   | Measurement                                               |
|------------------------------|----------|-----------------------------------------------------------|
| Availability                 | 99.9%    | `http_server_request_duration_seconds` success rate       |
| P99 latency (API)            | < 500 ms | `http_server_request_duration_seconds` p99 histogram      |
| Error rate (5xx)             | < 0.1%   | HTTP status 5xx / total requests                          |
| Login latency P99            | < 1 s    | `auth.Login` span duration (domain trace)                 |

---

## Alerting — PagerDuty / Slack

Deploy alert rules once the metrics backend is selected.  The examples below
use Prometheus Alertmanager syntax as a backend-agnostic reference.

### Example alert rules (Prometheus / Grafana — reference only)

```yaml
# alerts/hr-saas.yaml  (NOT committed to this repo — provision in ops repo)
groups:
  - name: hr-saas
    interval: 1m
    rules:
      # High error rate alert
      - alert: HighErrorRate
        expr: |
          sum(rate(http_server_request_duration_seconds_count{http_response_status_code=~"5.."}[5m]))
          /
          sum(rate(http_server_request_duration_seconds_count[5m])) > 0.001
        for: 5m
        labels:
          severity: critical
          service: hr-saas
        annotations:
          summary: "HR SaaS API error rate > 0.1%"
          runbook_url: "https://wiki.example.com/runbooks/hr-saas-error-rate"

      # High P99 latency alert
      - alert: HighLatencyP99
        expr: |
          histogram_quantile(0.99,
            sum(rate(http_server_request_duration_seconds_bucket[5m])) by (le, http_route)
          ) > 0.5
        for: 5m
        labels:
          severity: warning
          service: hr-saas
        annotations:
          summary: "HR SaaS P99 latency > 500 ms"

      # Service down (no metrics received)
      - alert: ServiceDown
        expr: up{job="hr-saas"} == 0
        for: 1m
        labels:
          severity: critical
          service: hr-saas
        annotations:
          summary: "HR SaaS backend is unreachable"
```

### Alertmanager routing (reference skeleton)

```yaml
# alertmanager.yaml (provision in ops repo — never commit credentials)
route:
  receiver: pagerduty-critical
  group_by: [alertname, service]
  group_wait: 30s
  group_interval: 5m
  repeat_interval: 4h
  routes:
    - match:
        severity: critical
      receiver: pagerduty-critical
    - match:
        severity: warning
      receiver: slack-warning

receivers:
  - name: pagerduty-critical
    pagerduty_configs:
      - service_key: "${PAGERDUTY_SERVICE_KEY}"   # inject from secrets manager
        description: '{{ .CommonAnnotations.summary }}'

  - name: slack-warning
    slack_configs:
      - api_url: "${SLACK_WEBHOOK_URL}"            # inject from secrets manager
        channel: "#alerts-hr-saas"
        title: '{{ .CommonAnnotations.summary }}'
        text: '{{ range .Alerts }}{{ .Annotations.summary }}{{ end }}'
```

---

## Manual spans — instrumented domain flows

The following domain service entry points emit manual OTel spans in addition
to the automatic HTTP server spans from `otelhttp`:

| Span name                | Package              | Attributes recorded           |
|--------------------------|----------------------|-------------------------------|
| `auth.Login`             | `internal/auth`      | `auth.success` (bool)         |
| `auth.Signup`            | `internal/auth`      | _(none beyond status/error)_  |
| `employee.CreateEmployee`| `internal/employee`  | _(none beyond status/error)_  |

**PII policy**: span names, attributes, and error messages must not contain
user emails, passwords, names, employee codes, or any other PII.  The span
attribute `auth.success=false` is safe (boolean only).

---

## Pending work (blocked on deploy-target decision — issue #9)

- Final exporter target selection (Grafana Cloud vs ADOT vs self-hosted Collector).
- ECS task definition / Terraform updates for `OTEL_EXPORTER_OTLP_ENDPOINT`.
- Alert rule deployment to the chosen metrics backend.
- CloudWatch Logs Insights saved queries and dashboards.
- Sampling strategy tuning (currently `AlwaysSample` — consider tail-based
  sampling via the OTel Collector for production traffic volumes).
