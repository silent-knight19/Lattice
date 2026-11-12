#!/bin/bash
set -m

# Start Lattice storage engine with admin console on loopback
/app/lattice --data-dir=/data --port=9099 --admin-address=127.0.0.1:7070 &
LATTICE_PID=$!

# Start Caddy reverse proxy forwarding public $PORT to Lattice loopback
caddy run --config /etc/caddy/Caddyfile --adapter caddyfile &
CADDY_PID=$!

cleanup() {
    echo "lattice: shutting down services..."
    kill -TERM "$LATTICE_PID" "$CADDY_PID" 2>/dev/null || true
    wait
    exit 0
}

trap cleanup SIGINT SIGTERM

# Wait for either process to terminate
wait -n "$LATTICE_PID" "$CADDY_PID"
cleanup
