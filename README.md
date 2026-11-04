# Fantom AI Gateway

Fantom is a standalone, OpenAI-compatible AI Gateway built with Go and a TypeScript/Next.js dashboard. The dashboard is exported as static assets and served by the Go process, so the complete product is published as one Docker service.

Docker Hub image: monsmain/fantom

## Features in the first implementation

- OpenAI-compatible POST /v1/chat/completions endpoint.
- OpenAI-compatible GET /v1/models catalog aggregation.
- NVIDIA NIM configured out of the box.
- Provider/model routing and public model aliases.
- Multiple upstream API keys per provider with round-robin key selection.
- Reusable HTTP connection pools, keep-alive and HTTP/2 negotiation.
- SSE streaming forwarded incrementally without buffering the full generation.
- Per-provider response-header timeout (100 seconds by default).
- Optional outbound HTTP/HTTPS proxy through standard proxy environment variables.
- Client API-key authentication, request IDs and JSON logs.
- Go API and Next.js dashboard in one multi-architecture Docker image.

This is a clean foundation, not yet a feature-for-feature replacement for 8room. Persistent users, usage accounting, quotas, admin CRUD, advanced failover and database-backed settings are later milestones.

## Run with Docker Compose

1. Copy the environment template:

   ```bash
   cp .env.example .env
   ```

   On Windows PowerShell:

   ```powershell
   Copy-Item .env.example .env
   ```

2. Edit .env. Set a long random GATEWAY_API_KEY and your NVIDIA_API_KEY.

3. Start Fantom:

   ```bash
   docker compose up -d --build
   ```

4. Open the dashboard at http://localhost:8090.

The default bind is 127.0.0.1:8090, so the service is not exposed to the public network. To change it, set FANTOM_BIND in .env, for example 0.0.0.0:8090. Only do this behind a firewall and HTTPS reverse proxy.

## Use the API

Base URL: http://localhost:8090/v1

```bash
curl http://localhost:8090/v1/chat/completions \
  -H "Authorization: Bearer YOUR_GATEWAY_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "meta/your-model",
    "messages": [{"role": "user", "content": "Hello"}],
    "stream": true
  }'
```

Health check is public:

```bash
curl http://localhost:8090/healthz
```

The model catalog and chat endpoint require Authorization: Bearer YOUR_GATEWAY_KEY.

## Configure providers

Edit configs/providers.json. Provider secrets must remain in environment variables, not in this JSON file.

Example NVIDIA provider:

```json
{
  "id": "nvidia",
  "base_url": "https://integrate.api.nvidia.com/v1",
  "auth_type": "bearer",
  "api_key_env": "NVIDIA_API_KEY",
  "api_keys_env": "NVIDIA_API_KEYS",
  "models": ["*"],
  "timeout_seconds": 100
}
```

- base_url: OpenAI-compatible API base URL, without the final /chat/completions or /models path.
- auth_type: bearer or none.
- api_key_env: environment variable holding one upstream key.
- api_keys_env: environment variable holding comma-separated upstream keys. When both are set, api_keys_env takes precedence when non-empty.
- models: exact upstream model IDs or *.
- timeout_seconds: time allowed to receive upstream response headers; defaults to 100, valid maximum 600.
- headers: optional provider-specific headers.

Example public alias:

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

A route maps public model ID fast-model to the upstream model. The upstream model must be included in that provider's model allowlist (or the provider must use *).

After changing provider configuration, restart the container:

```bash
docker compose restart fantom
```

## Outbound proxy

Fantom uses Go's pooled net/http transport with http.ProxyFromEnvironment. Set HTTPS_PROXY (and optionally HTTP_PROXY / NO_PROXY) in .env when upstream providers must be reached through an outbound proxy. This is separate from Fantom's own public API endpoint.

## Publish to Docker Hub

The GitHub Actions workflow builds linux/amd64 and linux/arm64 and publishes:

- monsmain/fantom:latest for pushes to main.
- monsmain/fantom:vX.Y.Z for Git tags such as v0.1.0.
- monsmain/fantom:sha-<commit> for traceable builds.

Add these GitHub Actions repository secrets in Settings → Secrets and variables → Actions:

- DOCKERHUB_USERNAME: Docker Hub username.
- DOCKERHUB_TOKEN: Docker Hub access token with permission to push to monsmain/fantom.

Pull the published image:

```bash
docker pull monsmain/fantom:latest
```

For configuration and persistence, use the included compose.yaml rather than running the bare image.

## Local development

Gateway:

```bash
go test ./...
go vet ./...
GATEWAY_API_KEY=dev-secret NVIDIA_API_KEY=your-key go run ./cmd/gateway
```

Dashboard:

```bash
cd dashboard
npm install
npm run dev
```

The supported Docker deployment exports the dashboard to static assets and serves both UI and API on the same port.

## Security notes

- Replace the sample client key before starting the service.
- Do not commit .env or provider API keys.
- Keep the default localhost-only bind unless public exposure is intentional.
- Put a TLS reverse proxy and firewall in front of any public deployment.
- The dashboard does not store the API key entered to load models; it is held in browser memory only.
