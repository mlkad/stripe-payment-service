# syntax=docker/dockerfile:1.7
# =============================================================================
# Multi-target build.
#   --target api       -> distroless, non-root, static API binary (~15 MB)
#   --target migrator  -> goose + the migrations directory
# =============================================================================

ARG GO_VERSION=1.25
ARG GOOSE_VERSION=v3.24.2

# --- shared build base -------------------------------------------------------
FROM golang:${GO_VERSION}-alpine AS base
RUN apk add --no-cache ca-certificates git tzdata
WORKDIR /src
# Dependency layer: only invalidated when go.mod/go.sum change.
COPY go.mod go.sum* ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

# --- API binary --------------------------------------------------------------
FROM base AS builder
ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE=unknown
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux go build \
        -trimpath \
        -ldflags="-s -w \
            -X main.version=${VERSION} \
            -X main.commit=${COMMIT} \
            -X main.buildDate=${BUILD_DATE}" \
        -o /out/api ./cmd/api

# --- goose binary ------------------------------------------------------------
FROM base AS goose-build
ARG GOOSE_VERSION
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go install github.com/pressly/goose/v3/cmd/goose@${GOOSE_VERSION}

# --- migrator runtime --------------------------------------------------------
FROM alpine:3.20 AS migrator
RUN apk add --no-cache ca-certificates postgresql16-client \
    && adduser -D -u 65532 goose
COPY --from=goose-build /go/bin/goose /usr/local/bin/goose
COPY --chown=goose:goose migrations /migrations
USER goose
WORKDIR /migrations
ENTRYPOINT ["goose"]
CMD ["status"]

# --- API runtime -------------------------------------------------------------
FROM gcr.io/distroless/static-debian12:nonroot AS api
COPY --from=builder /usr/share/zoneinfo /usr/share/zoneinfo
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=builder /out/api /app/api
USER nonroot:nonroot
EXPOSE 8080
ENV TZ=UTC
ENTRYPOINT ["/app/api"]
