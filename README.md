# Fantom AI Gateway

Fantom is a standalone, OpenAI-compatible AI Gateway built with Go and a TypeScript/Next.js dashboard. The dashboard is exported as static assets and served by the Go process, so the complete product is published as one Docker service.

Docker Hub image: monsmain/fantom

## The two configured providers

1. **NVIDIA NIM** — uses `NVIDIA_API_KEY` or a comma-separated key pool in `NVIDIA_API_KEYS`.
2. **OpenCode Console** — uses an official personal or service-account API key in `OPENCODE_API_KEY`. The starter allowlist contains `kimi-k3`; add the exact model IDs enabled for your Console workspace to `configs/providers.json`.

OpenCode's free-tier chat models are restricted to requests originating from the OpenCode client. Fantom does not impersonate that client or bypass that restriction; use Console models or a Go subscription through the documented API.

## Features

- OpenAI-compatible `POST /v1/chat/completions` and `GET /v1/models`.
- Exactly two providers configured initially: NVIDIA NIM and OpenCode Console.
- Provider-specific chat and model-catalog endpoints.
- Multiple NVIDIA upstream keys with round-robin selection.
- Reusable HTTP connection pools, keep-alive, HTTP/2 negotiation and incremental SSE forwarding.
- Per-provider response-header timeout (100 seconds by default).
- Optional outbound HTTP/HTTPS proxy.
- Gateway API-key authentication, request IDs and structured JSON logs.
- Opt-in code-generation modes inspired by Ponytail's minimal-change principles.
- Safe command-tool result cleanup inspired by RTK's normalization approach.
- One multi-architecture Docker image containing both API and dashboard.

This is an initial foundation, not yet a feature-for-feature replacement for 8room. Persistent users, usage accounting, quotas, admin CRUD, advanced failover and database-backed settings remain later milestones.

## Run with Docker Compose

1. Copy the environment template:

   ```bash
   cp .env.example .env
   ```

   Windows PowerShell:

   ```powershell
   Copy-Item .env.example .env
   ```

2. Set a long random `GATEWAY_API_KEY`, your `NVIDIA_API_KEY`, and your official `OPENCODE_API_KEY`.

3. Start Fantom:

   ```bash
   docker compose up -d --build
   ```

4. Open the dashboard at http://localhost:8090.

The default bind is localhost-only (`127.0.0.1:8090`). To expose it, set `FANTOM_BIND=0.0.0.0:8090` only behind a firewall and HTTPS reverse proxy.

## Use the API

Base URL: `http://localhost:8090/v1`

```bash
curl http://localhost:8090/v1/chat/completions \
  -H "Authorization: Bearer YOUR_GATEWAY_KEY" \
  -H "Content-Type: application/json" \
  -H "X-Fantom-Token-Saver: safe" \
  -H "X-Fantom-Code-Mode: full" \
  -d '{
    "model": "meta/your-model",
    "messages": [{"role": "user", "content": "Hello"}],
    "stream": true
  }'
```

The model catalog and chat endpoint require `Authorization: Bearer YOUR_GATEWAY_KEY`. `/healthz` is public.

## Token saver

The default mode is configured by `FANTOM_TOKEN_SAVER=safe`. It only touches recognized command-tool results (`bash`, `shell`, `terminal`, `command`, or `exec`) and:

- removes terminal ANSI control sequences;
- keeps the final content of progress lines redrawn with carriage returns;
- collapses consecutive identical non-empty lines;
- minifies valid JSON without changing its data.

User prompts, assistant messages, and file-read tool results are left untouched. The optimizer does not discard large arbitrary blocks or replace file contents with unrecoverable placeholders.

Per request, override the default:

- `X-Fantom-Token-Saver: safe` — normalization and repeated-line cleanup.
- `X-Fantom-Token-Saver: off` — pass the request body through unchanged.
- `X-Fantom-Code-Mode: lite|full|ultra` — prepend a compact coding instruction that prioritizes minimal, complete changes. Default: off.

Responses report `X-Fantom-Tool-Results-Optimized`, `X-Fantom-Bytes-Saved` and `X-Fantom-Estimated-Tokens-Saved` when a command result was shortened. The estimate is bytes divided by four, not a provider tokenizer measurement.

RTK's own `rtk-proxy` is licensed BUSL-1.1 and its terms restrict offering the daemon or a derivative as a paid or hosted third-party service. Fantom therefore implements its own limited normalization logic rather than bundling or copying that daemon. Ponytail's ideas are represented by a small, opt-in code-mode directive rather than injecting its full skill into every request.

## Provider configuration

Edit `configs/providers.json`; keep API keys in environment variables, not JSON.

NVIDIA uses:

```json
{
  "id": "nvidia",
  "base_url": "https://integrate.api.nvidia.com/v1",
  "chat_path": "/chat/completions",
  "api_key_env": "NVIDIA_API_KEY",
  "api_keys_env": "NVIDIA_API_KEYS",
  "models": ["*"],
  "timeout_seconds": 100
}
```

OpenCode Console uses separate URLs for chat and model discovery:

```json
{
  "id": "opencode-console",
  "base_url": "https://opencode.ai/inference/openai/v1",
  "chat_path": "/chat/completions",
  "models_url": "https://opencode.ai/inference/v1/models",
  "api_key_env": "OPENCODE_API_KEY",
  "models": ["kimi-k3"],
  "timeout_seconds": 100
}
```

- `base_url`: upstream API base URL.
- `chat_path`: chat-completions path, defaults to `/chat/completions`.
- `models_url`: optional absolute URL for model discovery; if omitted, Fantom requests `base_url + /models`.
- `auth_type`: `bearer` or `none`.
- `api_key_env`: environment variable for one key.
- `api_keys_env`: comma-separated key pool; takes precedence when non-empty.
- `models`: exact model IDs or `*`.
- `timeout_seconds`: response-header timeout, default 100 and maximum 600.
- `headers`: optional provider-specific headers.

Public aliases can route a client-facing model name to a provider/model pair:

```json
{
  "routes": {
    "fast-model": {
      "provider": "nvidia",
      "upstream_model": "meta/actual-model-id"
    }
  }
}
```

After editing provider configuration, restart:

```bash
docker compose restart fantom
```

## Outbound proxy

Fantom uses Go's pooled `net/http` transport with `http.ProxyFromEnvironment`. Set `HTTPS_PROXY` (and optionally `HTTP_PROXY` / `NO_PROXY`) in `.env` for outbound provider traffic. This is separate from Fantom's own public API endpoint.

## Publish to Docker Hub

GitHub Actions builds `linux/amd64` and `linux/arm64` and publishes:

- `monsmain/fantom:latest` on pushes to `main`.
- `monsmain/fantom:vX.Y.Z` for version tags.
- `monsmain/fantom:sha-<commit>` for traceable builds.

Configure these GitHub Actions repository secrets:

- `DOCKER_USERNAME` or `DOCKERHUB_USERNAME`: Docker Hub username.
- `DOCKERHUB_TOKEN`: token with push permission to `monsmain/fantom`.

## Local development

```bash
go test ./...
go vet ./...
GATEWAY_API_KEY=dev-secret NVIDIA_API_KEY=your-key go run ./cmd/gateway
```

For the dashboard:

```bash
cd dashboard
npm install
npm run dev
```

## Security

- Replace the sample gateway key before starting.
- Do not commit `.env` or provider API keys.
- Keep the default localhost-only bind unless public exposure is intentional.
- Put TLS termination and a firewall in front of public deployments.
