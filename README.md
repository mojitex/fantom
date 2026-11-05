# Fantom — AI Routing Control Plane

Fantom is a self-hosted AI routing platform with a Go gateway, an embedded Go provider/OAuth/protocol runtime, and a TypeScript/Next.js control-plane dashboard. The dashboard and runtime ship together in one multi-architecture Docker image.

**Image:** `monsmain/fantom:latest`  
**Repository:** https://github.com/mojitex/fantom

## What is in this release

### Control plane
- A focused dashboard with four primary destinations: Endpoint & Key, Providers, Combos and Usage. Searchable model discovery lives inside Providers; the full request explorer lives inside Usage; Settings stays in the sidebar footer.
- Add, edit, test and remove OpenAI-compatible provider connections directly in the dashboard. Provider changes are applied to the live Go process without rebuilding the image.
- Provider credentials are persisted in a local state file with restrictive filesystem permissions and are never returned by the management API. Multiple API keys can be configured per provider; requests rotate through healthy credentials, temporarily cool down keys rejected or rate-limited by the upstream, honor a bounded `Retry-After` header and retry another credential before advancing to a model fallback. When every key is cooling down and no alternate route can serve the model, Fantom returns `503` with `Retry-After` instead of repeatedly hitting a throttled key.
- Model aliases map a stable client-facing ID to an upstream provider and model. Ordered fallback aliases are attempted only for classified transient/authentication/billing responses.
- Generate and revoke client API keys. Generated keys are only displayed once and stored as SHA-256 hashes.
- Request history and provider/model/latency aggregates (last 5,000 requests), including provider-reported prompt/completion/total tokens when returned by the upstream (JSON and SSE).
- Default provider, token saver and code-mode settings are stored persistently.

### Gateway runtime
- `GET /v1/models` and `POST /v1/chat/completions`.
- NVIDIA NIM and OpenCode Console are preconfigured; more OpenAI-compatible providers can be added from the dashboard.
- Provider-specific chat paths and model-catalog URLs.
- Multiple upstream NVIDIA keys, round-robin key selection, pooled HTTP connections, HTTP/2 and incremental SSE forwarding.
- 100-second default response-header timeout; configurable per provider.
- Provider model catalogs are cached for 45 seconds, serve the last known-good result during transient failures, and can be invalidated through the admin API.
- Optional outbound HTTP/HTTPS proxy.
- Safe command-tool output normalization and optional compact code-mode instructions.
- Bearer-key authentication, request IDs, health endpoint and structured logs.

### Embedded Go routing engine

Fantom now embeds the MIT-licensed [CLIProxyAPI Go SDK](https://github.com/router-for-me/CLIProxyAPI) as an in-process service with a private loopback listener. Fantom remains the public gateway: it validates application keys before proxying inference traffic, while the embedded engine owns provider executors, OAuth credential files, token refresh, account selection, protocol compatibility and native streaming.

The embedded runtime exposes its management API through Fantom at `/v0/management/*` and its advanced control panel at `/management.html`. The control-panel page is reachable so it can render its login form; management operations require the environment-level `GATEWAY_API_KEY`. A direct link is available as **Router Console** in the TypeScript dashboard. OAuth callbacks are forwarded through the public Fantom listener. OAuth state and the embedded runtime configuration are stored under `FANTOM_DATA_DIR/cli-proxy`; attach a persistent volume to retain accounts across restarts.

The two routing planes are intentionally staged: models already owned by Fantom's configured providers and explicit aliases continue through Fantom's existing provider path; models discovered from OAuth/account-backed providers are delegated to the embedded runtime. This avoids silently breaking existing NVIDIA NIM and OpenCode configurations while the provider-management UI is migrated.

## Important scope and compatibility notes

The embedded Go engine provides OAuth-backed provider execution, credential refresh, account selection and native OpenAI/Anthropic/Gemini-compatible API surfaces. Fantom's own TypeScript dashboard and its provider/usage control APIs are being migrated in stages; some advanced controls are currently available through the proxied `/v0/management/*` API and `/management.html` panel rather than the primary Fantom dashboard. Do not treat the existing legacy Usage page as a complete cross-runtime cost ledger yet.

The initial provider catalog contains NVIDIA NIM and OpenCode Console. OpenCode free-tier restrictions are not bypassed; use supported Console/Go API access. Add compatible endpoints from the Providers page.

Credential failover is bounded to the configured key pool for each route candidate. Token analytics reports only usage returned by the upstream; providers that omit usage are recorded without fabricated token or cost figures. Monetary cost calculation, OAuth refresh, quota-aware account scheduling and cross-protocol translation remain separate work and are not claimed by this release.

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
   | `FANTOM_PUBLIC_URL` | Recommended for OAuth | Public Fantom URL, including `https://` |
   | `FANTOM_ROUTER_INTERNAL_KEY` | Optional | Fixed private key for the loopback-only embedded runtime; leave empty for a derived key |
   | `FANTOM_TOKEN_SAVER` | Optional | `safe` or `off` |
   | `FANTOM_CODE_MODE` | Optional | `off`, `lite`, `full` or `ultra` |

4. Attach a Railway **Volume mounted at `/data`** and set `FANTOM_DATA_DIR=/data`. This preserves legacy providers, embedded OAuth credentials, router configuration, secrets, aliases, generated key hashes and request logs across redeploys. Request-history writes are coalesced for up to about one second and flushed on graceful shutdown, keeping routine log I/O off the inference path. A hard process/host crash can still lose the most recent unflushed request records. Without a persistent volume, the container filesystem can be reset by a redeploy.
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

Use generated client keys for application traffic on `/v1/*`. Management routes under `/api/admin/*` and the embedded `/v0/management/*` API require the environment-level `GATEWAY_API_KEY`; the `/management.html` page presents the embedded console login. Generated client keys cannot administer the gateway.

## Admin API

All endpoints below require `Authorization: Bearer YOUR_GATEWAY_KEY`.

| Method | Endpoint | Purpose |
|---|---|---|
| GET | `/api/admin/bootstrap` | Dashboard data and overview |
| GET / POST | `/api/admin/providers` | List or add providers |
| PUT / DELETE | `/api/admin/providers/{id}` | Update or remove a provider |
| POST | `/api/admin/providers/{id}/test` | Run a live chat-completions smoke test (also reports model-catalog status) |
| POST | `/api/admin/models/refresh` | Invalidate provider model-catalog caches |
| POST | `/api/admin/models/refresh` | Invalidate cached provider model catalogs |
| GET / PUT | `/api/admin/routes` | Read or replace model aliases and ordered fallback chains |
| GET / POST | `/api/admin/keys` | List keys or create a one-time secret |
| DELETE | `/api/admin/keys/{id}` | Revoke a generated key |
| GET | `/api/admin/usage` | Aggregated request metrics |
| GET | `/api/admin/logs` | Recent request history |
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

Use `models: ["*"]` to expose all models returned by the provider's model endpoint, or list explicit model IDs. Credentials are saved to the protected state file and redacted from dashboard responses. Use `api_key` for a single credential or `api_keys` for a round-robin pool.

## Token saver

- `FANTOM_TOKEN_SAVER=safe` normalizes recognized command-tool results only: ANSI sequences, carriage-return progress redraws, consecutive repeated lines and valid JSON whitespace.
- `FANTOM_TOKEN_SAVER=off` leaves request content unchanged.
- `FANTOM_CODE_MODE=lite|full|ultra` adds a compact coding instruction; default is `off`.
- Per-request headers: `X-Fantom-Token-Saver` and `X-Fantom-Code-Mode`.

This is conservative output cleanup, not a universal tokenizer compressor. Savings vary with tool output and are not guaranteed.

### Retry diagnostics

Final chat responses can include these gateway headers (including a gateway error after upstream attempts):

- `X-Fantom-Upstream-Attempts`: total upstream HTTP attempts made for the request.
- `X-Fantom-Credential-Retries`: additional credentials tried inside a provider key pool.
- `X-Fantom-Fallback-Attempts`: additional model/provider fallback candidates attempted after the primary candidate.

Credential retries and model fallback attempts are counted separately. The same three counters are stored in each request-history row and shown in the Request Logs table.

Request history is written in coalesced batches rather than synchronously on every inference request. Fantom flushes pending history during graceful shutdown. A hard process or host crash can still lose the most recent unflushed request records (normally up to about one second of traffic).

## Local development

```bash
go mod tidy
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
