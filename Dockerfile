# ── Build frontend ──
FROM oven/bun:1 AS frontend
WORKDIR /app/web
COPY web/package.json web/bun.lock* ./
RUN bun install --frozen-lockfile
COPY web/ .
RUN bun run build

# ── Build backend ──
FROM golang:1.25-alpine AS backend
RUN apk add --no-cache git
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=frontend /app/web/dist ./web/dist
ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE=unknown
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w \
      -X github.com/tabloy/keygate/internal/version.Version=${VERSION} \
      -X github.com/tabloy/keygate/internal/version.Commit=${COMMIT} \
      -X github.com/tabloy/keygate/internal/version.BuildDate=${BUILD_DATE}" \
    -o /keygate ./cmd/server

# ── Runtime ──
FROM alpine:3.21
RUN apk add --no-cache ca-certificates tzdata curl
WORKDIR /app
COPY --from=backend /keygate /usr/local/bin/keygate
COPY --from=backend /app/db/migrations /app/db/migrations
COPY --from=backend /app/web/dist /app/web/dist
COPY --from=backend /app/docs /app/docs

LABEL org.opencontainers.image.title="Keygate" \
      org.opencontainers.image.description="Open source license management platform" \
      org.opencontainers.image.vendor="Tabloy" \
      org.opencontainers.image.url="https://keygate.app" \
      org.opencontainers.image.source="https://github.com/tabloy/keygate" \
      org.opencontainers.image.licenses="AGPL-3.0"

EXPOSE 9000
ENV PORT=9000
ENTRYPOINT ["keygate"]
