# Multi-stage Dockerfile — builds all three binaries from a single context.
# Use --target to pick one per service: kvfs-edge, kvfs-dn, kvfs-cli.

FROM golang:1.26-alpine AS builder
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/kvfs-edge ./cmd/kvfs-edge
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/kvfs-dn   ./cmd/kvfs-dn
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/kvfs-cli  ./cmd/kvfs-cli

# ---- kvfs-edge image ----
FROM alpine:3.20 AS kvfs-edge
RUN adduser -D -u 10001 kvfs && \
    mkdir -p /var/lib/kvfs-edge && \
    chown -R kvfs:kvfs /var/lib/kvfs-edge
COPY --from=builder /out/kvfs-edge /usr/local/bin/kvfs-edge
USER kvfs
VOLUME /var/lib/kvfs-edge
EXPOSE 8000
ENTRYPOINT ["/usr/local/bin/kvfs-edge"]

# ---- kvfs-dn image ----
FROM alpine:3.20 AS kvfs-dn
RUN adduser -D -u 10001 kvfs && \
    mkdir -p /var/lib/kvfs-dn && \
    chown -R kvfs:kvfs /var/lib/kvfs-dn
COPY --from=builder /out/kvfs-dn /usr/local/bin/kvfs-dn
USER kvfs
VOLUME /var/lib/kvfs-dn
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/kvfs-dn"]

# ---- kvfs-cli image (for demos) ----
FROM alpine:3.20 AS kvfs-cli
RUN adduser -D -u 10001 kvfs && apk add --no-cache curl jq
COPY --from=builder /out/kvfs-cli /usr/local/bin/kvfs-cli
USER kvfs
ENTRYPOINT ["/usr/local/bin/kvfs-cli"]
