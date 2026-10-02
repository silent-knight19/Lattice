package admin

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// SSE governance constants (SEC-8).
const (
	// DefaultMaxSubscribers is the global cap on concurrent event-stream clients.
	//
	// This is deliberately far lower than the metrics server's 256 scraper cap
	// (internal/metrics/server.go:20). The two workloads are opposite: a Prometheus scrape
	// is a brief connection that closes, whereas an SSE client holds a goroutine, a
	// buffered channel, and a TCP connection open for its entire session. A handful of
	// console tabs is a normal deployment; hundreds is an attack.
	DefaultMaxSubscribers = 8

	// DefaultMaxSubscribersPerIP blunts a single-host reconnect storm, where one machine
	// (or one page reloading in a loop) opens many streams.
	DefaultMaxSubscribersPerIP = 4

	// SSEHeartbeatInterval keeps intermediaries from reaping an idle stream.
	//
	// Without it, a load balancer or NAT in the path may silently close a connection that
	// carries no bytes, and the console would silently stop updating with no error.
	SSEHeartbeatInterval = 15 * time.Second

	// SSERetryMillis is the reconnection delay advertised to the browser. Bounded so a
	// large console fleet cannot turn a daemon restart into a reconnect storm.
	SSERetryMillis = 3000
)

// sseStreamHandler serves GET /api/v1/events.
//
// Guarantees, each covered by a test:
//   - A connection is refused with 503 BEFORE a goroutine or channel is allocated when the
//     global or per-IP budget is exhausted.
//   - Every accepted connection releases its slot on disconnect (r.Context().Done()).
//   - A heartbeat is emitted so idle streams survive intermediaries.
//   - No secret or unbounded string is ever written; every field is sanitized at
//     construction time in NewEvent.
type sseStreamHandler struct {
	bus         *EventBus
	perIP       *ipLimiter
	interval    time.Duration
	retryMillis int
}

func newSSEStreamHandler(bus *EventBus) *sseStreamHandler {
	return &sseStreamHandler{
		bus:         bus,
		perIP:       newIPLimiter(DefaultMaxSubscribersPerIP),
		interval:    SSEHeartbeatInterval,
		retryMillis: SSERetryMillis,
	}
}

// ServeHTTP implements the SSE endpoint.
func (h *sseStreamHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeMethodNotAllowed(w)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		// Without flushing this would be a hanging response, not a stream.
		writeError(w, http.StatusInternalServerError, "internal_error", "internal error")
		return
	}

	// Refuse BEFORE subscribing: Subscribe() allocates the channel, so checking the
	// budget afterwards would already have spent the memory we are trying to protect.
	if h.bus.Subscribers() >= h.bus.MaxSubscribers() {
		h.bus.DroppedCounter().Inc()
		w.Header().Set("Retry-After", strconv.Itoa(h.retryMillis/1000))
		writeError(w, http.StatusServiceUnavailable, "busy", "event stream capacity reached")
		return
	}
	ip := remoteIP(r)
	if !h.perIP.acquire(ip) {
		h.bus.DroppedCounter().Inc()
		w.Header().Set("Retry-After", strconv.Itoa(h.retryMillis/1000))
		writeError(w, http.StatusServiceUnavailable, "busy", "too many event streams from this client")
		return
	}

	sub, err := h.bus.Subscribe()
	if err != nil {
		// Lost a race against another connect between the check and the subscribe.
		h.perIP.release(ip)
		w.Header().Set("Retry-After", strconv.Itoa(h.retryMillis/1000))
		writeError(w, http.StatusServiceUnavailable, "busy", "event stream capacity reached")
		return
	}

	// Everything acquired above is released on every exit path.
	defer func() {
		sub.Close()
		h.perIP.release(ip)
	}()

	hdr := w.Header()
	hdr.Set("Content-Type", "text/event-stream; charset=utf-8")
	hdr.Set("Cache-Control", "no-store")
	hdr.Set("Connection", "keep-alive")
	// Ask intermediaries not to buffer; without it a proxy can hold the stream and the
	// heartbeat trick below stops working.
	hdr.Set("X-Accel-Buffering", "no")

	w.WriteHeader(http.StatusOK)
	// Advertise the reconnection delay.
	fmt.Fprintf(w, "retry: %d\n\n", h.retryMillis)
	// An initial comment flushes headers so the client sees the stream open immediately.
	fmt.Fprint(w, ": connected\n\n")
	flusher.Flush()

	ticker := time.NewTicker(h.interval)
	defer ticker.Stop()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			// Client disconnected or the server is shutting down.
			return

		case ev, ok := <-sub.C():
			if !ok {
				return
			}
			if err := writeSSEEvent(w, ev); err != nil {
				return
			}
			flusher.Flush()

		case <-ticker.C:
			// Heartbeat comment: keeps the connection alive across idle proxies.
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// writeSSEEvent marshals one event as an SSE data frame.
func writeSSEEvent(w http.ResponseWriter, ev Event) error {
	payload, err := json.Marshal(ev)
	if err != nil {
		// Never fail the stream over one unencodable event.
		return nil
	}
	if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Kind, payload); err != nil {
		return err
	}
	return nil
}

// remoteIP extracts the client IP for per-IP limiting.
//
// It reads r.RemoteAddr, which for a loopback admin listener is the TCP peer. X-Forwarded-For
// is deliberately IGNORED: it is client-supplied and trivially spoofed, so trusting it would
// let an attacker bypass the per-IP cap by varying a header.
func remoteIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// ipLimiter is a small fixed-capacity per-IP reference counter.
type ipLimiter struct {
	max  int
	mu   sync.Mutex
	held map[string]int
}

func newIPLimiter(max int) *ipLimiter {
	if max <= 0 {
		max = DefaultMaxSubscribersPerIP
	}
	return &ipLimiter{max: max, held: make(map[string]int)}
}

// acquire reserves a slot for ip, reporting false when the cap is reached.
func (l *ipLimiter) acquire(ip string) bool {
	if l == nil {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.held[ip] >= l.max {
		return false
	}
	l.held[ip]++
	return true
}

// release returns a slot for ip.
func (l *ipLimiter) release(ip string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if n := l.held[ip]; n <= 1 {
		delete(l.held, ip)
	} else {
		l.held[ip] = n - 1
	}
}

// count reports the current holders for ip (test helper).
func (l *ipLimiter) count(ip string) int {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.held[ip]
}

// sseConnLimiter bounds concurrent connections at the listener level.
//
// Mirrors connLimiterListener in internal/metrics/server.go:63-98. Rejecting at Accept()
// means a flood is refused before the server allocates any per-connection state.
type sseConnLimiter struct {
	net.Listener
	active *atomic.Int64
	max    int64
}

func newSSEConnLimiter(ln net.Listener, max int64) net.Listener {
	a := &atomic.Int64{}
	return &sseConnLimiter{Listener: ln, active: a, max: max}
}

func (l *sseConnLimiter) Accept() (net.Conn, error) {
	for {
		conn, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		if l.max > 0 && l.active.Load() >= l.max {
			_ = conn.Close()
			continue
		}
		l.active.Add(1)
		return &sseTrackedConn{Conn: conn, active: l.active}, nil
	}
}

// Active reports the current connection count.
func (l *sseConnLimiter) Active() int64 { return l.active.Load() }

type sseTrackedConn struct {
	net.Conn
	active *atomic.Int64
	once   atomic.Bool
}

func (c *sseTrackedConn) Close() error {
	if c.once.CompareAndSwap(false, true) {
		c.active.Add(-1)
	}
	return c.Conn.Close()
}
