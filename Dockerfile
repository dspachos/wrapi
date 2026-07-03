# syntax=docker/dockerfile:1

# ---- build the wrapi gateway -------------------------------------------------
FROM golang:1.22-alpine AS build
WORKDIR /src/gateway

# Cache module downloads.
COPY gateway/go.mod gateway/go.sum ./
RUN go mod download

# Build a static binary.
COPY gateway/ ./
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /out/gateway .

# ---- minimal runtime image ---------------------------------------------------
# distroless/static ships CA certificates (needed for HTTPS upstreams) and nothing else.
FROM gcr.io/distroless/static-debian12

COPY --from=build /out/gateway /gateway
COPY gateway/config /config

WORKDIR /
EXPOSE 8080

# Config defaults resolve to /config/* (see gateway env vars). Override at runtime:
#   docker run -e JWT_SECRET=... -e LEGACY_API_URL=... -p 8080:8080 wrapi
ENV GATEWAY_CONFIG_PATH=/config/hitl_policy_map.json \
    AGENT_SPEC_PATH=/config/agent_openapi.json \
    RESPONSE_SHAPES_PATH=/config/response_shapes.json \
    COMPOSITES_PATH=/config/composites.json \
    LISTEN_ADDR=:8080

ENTRYPOINT ["/gateway"]
