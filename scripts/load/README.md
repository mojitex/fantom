# Fantom load testing

This test drives the real Chat Completions route through a configured provider.
It does **not** use a mock model: every successful request may incur provider
charges and consume provider quota. Begin with a small VU count and a test model.

## Requirements

- k6 installed on the machine running the test.
- A reachable Fantom base URL.
- A valid application bearer key (not the admin key).
- A model alias/provider route that is known to work.
- Monitoring of the Fantom container (CPU, memory, restarts) and, ideally,
  Prometheus scraping of the administrator-key-protected `/metrics` endpoint.

## Example

PowerShell:

```powershell
$env:BASE_URL = "https://fantom.example.com"
$env:GATEWAY_API_KEY = "your-application-key"
$env:MODEL = "your-model-alias"
$env:MAX_VUS = "10"
$env:RAMP_SECONDS = "30"
$env:HOLD_SECONDS = "60"
$env:SLEEP_SECONDS = "0.2"
k6 run scripts/load/fantom.js
```

Start with 5–10 VUs, then repeat at 25, 50, 100, and higher only when the
container and provider quotas can support it. Keep the model, prompt, response
limit, instance size, and deployment configuration identical between runs.

## Signals to record

- k6: `http_reqs`, request rate, p95/p99 latency, `http_req_failed`, and
  threshold failures.
- Fantom `/metrics`: `fantom_inference_in_flight`,
  `fantom_inference_rejections_total`, `fantom_upstream_in_flight`,
  `fantom_upstream_rejections_total`, `fantom_provider_limit_rejections_total`,
  and `fantom_request_body_bytes_in_flight`.
- Container: peak RSS/memory, CPU, network, restarts, and file-system latency.
- Provider: rate-limit responses, quota, latency, and error rates.

A rise in 503s with `X-Fantom-Admission: rejected` means the gateway's global
inference limit is saturated. `X-Fantom-Admission: body-budget` means the
weighted request-body budget is saturated. Provider and global upstream
rejections are also exposed as Prometheus counters. These are deliberate
backpressure signals, not queue-growth indicators.

Do not increase limits merely to make 503s disappear. First verify that memory,
CPU, upstream connection capacity, and provider quotas have headroom. The
configured values are per process; multiple replicas each enforce their own
limits. A shared ingress rate limit and distributed quota accounting are
required if you need one global budget across replicas.
