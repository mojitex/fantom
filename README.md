# Fantom — AI Routing Control Plane

Fantom is a self-hosted AI gateway with a Go routing runtime and a Next.js control-plane dashboard. The dashboard and API ship together in one multi-architecture Docker image.

**Image:** `monsmain/fantom:latest`  
**Repository:** https://github.com/mojitex/fantom

## What is in this release

### Control plane
- A focused dark dashboard with four primary sections: Endpoint & Key, Providers, Combos and Usage; Settings stays out of the main navigation.
- Add, edit, test and remove OpenAI-compatible provider connections directly in the dashboard. Provider changes are applied to the live Go process without rebuilding the image.
- Provider credentials are persisted in a local state file with restrictive filesystem permissions and are never returned by the management API. Multiple API keys can be configured per provider, selected round-robin, and retried on retryable upstream failures (up to three keys per model candidate).
- Model aliases map a stable client-facing ID to an upstream provider and model. Ordered fallback aliases retry on authentication, rate-limit, timeout and transient 5xx responses.
- Generate and revoke client API keys. Generated keys are only displayed once and stored as SHA-256 hashes.
- Request history and provider/model/latency aggregates for the last 5,000 requests. Request history writes are batched off the inference path and flushed on graceful shutdown.
- Default provider, token saver and code-mode settings are stored persistently.

### Gateway runtime
- `GET /v1/models` and `POST /v1/chat/completions`.
- NVIDIA NIM and OpenCode Console are preconfigured; more OpenAI-compatible providers can be added from the dashboard.
- Provider-specific chat paths and model-catalog URLs.
- Multiple upstream NVIDIA keys, round-robin key selection, pooled HTTP connections, HTTP/2 and incremental SSE forwarding.
- 100-second default response-header timeout; configurable per provider. Streaming bodies are not cut off by a fixed total-response timer.
- Optional outbound HTTP/HTTPS proxy.
- Safe command-tool output normalization and optional compact code-mode instructions.
- Bearer-key authentication, request IDs, health endpoint and structured logs.

## Important scope and compatibility notes

Fantom is not yet a feature-for-feature clone of 9Router or OmniRoute. It currently supports OpenAI-compatible Chat Completions, model aliases, ordered fallback chains and retrying a provider's API-key pool. It does **not** yet include OAuth/device-code onboarding, quota-aware multi-account selection, native Claude/Gemini/OpenAI Responses translation, provider token-refresh workflows or price-based billing. Usage analytics records prompt, completion and total tokens when the upstream reports them; providers that omit usage data are shown without token counts. The dashboard retains the most recent 5,000 requests, so these totals are rolling history rather than lifetime billing records.

The initial provider catalog contains NVIDIA NIM and OpenCode Console. OpenCode free-tier restrictions are not bypassed; use supported Console/Go API access. Add compatible endpoints from the Providers page.

## Deploy on Railway

1. Deploy the `monsmain/fantom:latest` image (or connect this repository and use its Dockerfile).
2. Set the Railway service's listening port to **8080** (the container listens on `:8080`).
3. Add these variables:

   | Variable | Required | Purpose |
   |---|---|---|
   | `GATEWAY_API_KEY` | Yes | First dashboard/admin login and gateway access |
   | `NVIDIA_API_KEY` | Optional | NVIDIA NIM upstream credential |
   | `OPENCODE_API_KEY` | Optional | OpenCode Console upstream credential |
   | `FANTOM_DATA_DIR` | Recommended | Set to `/data` |
   | `FANTOM_TOKEN_SAVER` | Optional | `safe` or `off` |
   | `FANTOM_CODE_MODE` | Optional | `off`, `lite`, `full` or `ultra` |

4. Attach a Railway **Volume mounted at `/data`** and set `FANTOM_DATA_DIR=/data`. This preserves providers, secrets, aliases, generated key hashes and request logs across redeploys. Request-history writes are coalesced for up to about one second to keep disk I/O off the inference path; graceful shutdown flushes the pending batch. A hard process or host crash can lose the most recent unflushed request records. Without a persistent volume, the container filesystem can be reset by a redeploy.
5. Open the generated Railway domain. Sign in with `GATEWAY_API_KEY`.

For a self-hosted Docker deployment, `compose.yaml` creates a named `fantom-data` volume automatically.

## API usage

Base URL: `https://YOUR-DOMAIN/v1`

```bash
curl https://YOUR-DOMAIN/v1/chat/completions \
  -H "Authorization: Bearer YOUR_CLIENT_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "meta/your-model",
    "messages": [{"role":"user","content":"Hello"}],
    "stream": true
  }'
```

Use generated client keys for application traffic on `/v1/*`. Management routes under `/api/admin/*` require the environment-level `GATEWAY_API_KEY`; generated client keys cannot administer the gateway.

## Admin API

All endpoints below require `Authorization: Bearer YOUR_GATEWAY_KEY`.

| Method | Endpoint | Purpose |
|---|---|---|
| GET | `/api/admin/bootstrap` | Dashboard data and overview |
| GET / POST | `/api/admin/providers` | List or add providers |
| PUT / DELETE | `/api/admin/providers/{id}` | Update or remove a provider |
| POST | `/api/admin/providers/{id}/test` | Test provider model-catalog connectivity |
| GET / PUT | `/api/admin/routes` | Read or replace model aliases and ordered fallback chains |
| GET / POST | `/api/admin/keys` | List keys or create a one-time secret |
| DELETE | `/api/admin/keys/{id}` | Revoke a generated key |
| GET | `/api/admin/usage` | Aggregated request and provider-reported token metrics |
| GET | `/api/admin/logs` | Recent request history, including reported token usage when available |
| GET / PUT | `/api/admin/settings` | Default provider and optimization settings |

### Provider payload

```json
{
  "id": "my-openai-compatible",
  "base_url": "https://api.example.com/v1",
  "chat_path": "/chat/completions",
  "models_url": "https://api.example.com/v1/models",
  "auth_type": "bearer",
  "api_keys": ["provider-secret-1", "provider-secret-2"],
  "models": ["*"],
  "timeout_seconds": 100
}
```

Use `models: ["*"]` to expose all models returned by the provider's model endpoint, or list explicit model IDs. Credentials are saved to the protected state file and redacted from dashboard responses. Use `api_key` for a single credential or `api_keys` for a round-robin pool. When an upstream key receives a retryable HTTP status or a connection error, Fantom tries another key from the same pool (up to three key attempts) before advancing to the next configured fallback alias.

## Token saver

- `FANTOM_TOKEN_SAVER=safe` normalizes recognized command-tool results only: ANSI sequences, carriage-return progress redraws, consecutive repeated lines and valid JSON whitespace.
- `FANTOM_TOKEN_SAVER=off` leaves request content unchanged.
- `FANTOM_CODE_MODE=lite|full|ultra` adds a compact coding instruction; default is `off`.
- Per-request headers: `X-Fantom-Token-Saver` and `X-Fantom-Code-Mode`.

This is conservative output cleanup, not a universal tokenizer compressor. Savings vary with tool output and are not guaranteed. Usage reports only provider-returned token counts; it does not estimate billing cost.

## Local development

```bash
go test ./...
go vet ./...
GATEWAY_API_KEY=dev-secret FANTOM_DATA_DIR=./data go run ./cmd/gateway
```

Dashboard development:

```bash
cd dashboard
npm install
npm run dev
```

## Docker publishing

GitHub Actions builds `linux/amd64` and `linux/arm64` and publishes `monsmain/fantom:latest` on pushes to `main`. Configure repository secrets `DOCKER_USERNAME` (or `DOCKERHUB_USERNAME`) and `DOCKERHUB_TOKEN`.

## Security

- Use a long, unique `GATEWAY_API_KEY` and do not share the admin key with client applications.
- Generated client keys are hashed at rest; provider upstream secrets are stored in the local state file. Protect the mounted volume and its backups.
- Put public deployments behind HTTPS. Do not expose the container's port without a TLS-terminating proxy.
- Never commit provider credentials or the Fantom state file.
