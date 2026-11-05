# syntax=docker/dockerfile:1.7
FROM --platform=$BUILDPLATFORM node:24-alpine AS dashboard-builder
WORKDIR /dashboard
COPY dashboard/package.json ./
RUN npm install --no-audit --no-fund
COPY dashboard/ ./
RUN npm run build

FROM --platform=$BUILDPLATFORM golang:1.26.10-alpine AS gateway-builder
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
COPY --from=dashboard-builder /dashboard/out ./dashboard/out
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} go build -trimpath -ldflags="-s -w" -o /out/fantom ./cmd/gateway

FROM alpine:3.21 AS runtime
RUN apk add --no-cache ca-certificates \
    && addgroup -S fantom \
    && adduser -S -G fantom fantom \
    && mkdir -p /data \
    && chown -R fantom:fantom /data
WORKDIR /app
COPY --from=gateway-builder /out/fantom /app/fantom
COPY configs/providers.json /app/configs/providers.json
COPY --from=dashboard-builder /dashboard/out /app/dashboard
ENV GATEWAY_LISTEN_ADDR=:8080 CONFIG_FILE=/app/configs/providers.json STATIC_DIR=/app/dashboard FANTOM_DATA_DIR=/data
VOLUME ["/data"]
EXPOSE 8080
USER fantom
ENTRYPOINT ["/app/fantom"]
