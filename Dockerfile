# Stage 1: Build static Lattice daemon binary with embedded console assets
FROM golang:1.23-alpine AS builder

WORKDIR /app

# Copy all repository source files (including embedded web/dist)
COPY . .

# Build statically linked Lattice daemon
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /app/bin/lattice ./cmd/lattice

# Stage 2: Minimal runtime image with Caddy reverse proxy
FROM caddy:2-alpine

WORKDIR /app

# Install bash and tini supervisor
RUN apk add --no-cache bash tini

# Copy the compiled Lattice binary
COPY --from=builder /app/bin/lattice /app/lattice

# Copy Caddyfile and startup entrypoint
COPY Caddyfile /etc/caddy/Caddyfile
COPY entrypoint.sh /app/entrypoint.sh
RUN chmod +x /app/entrypoint.sh

# Persistent database storage directory
RUN mkdir -p /data

EXPOSE 8080

ENTRYPOINT ["/sbin/tini", "--"]
CMD ["/app/entrypoint.sh"]
