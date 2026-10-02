package admin

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"
)

// newSSETestServer starts a real listener-backed SSE server for connection-budget tests.
func newSSETestServer(t *testing.T, bus *EventBus, perIP int) (*httptest.Server, func()) {
	t.Helper()
	h := newSSEStreamHandler(bus)
	if perIP > 0 {
		h.perIP = newIPLimiter(perIP)
	}
	// A short heartbeat keeps the test fast while still exercising the ticker path.
	h.interval = 50 * time.Millisecond

	mux := http.NewServeMux()
	mux.Handle(RouteEvents, h)
	srv := httptest.NewServer(SecurityHeaders(NoStoreAPI(mux)))
	return srv, srv.Close
}

// sseTestClient dials without keep-alive.
//
// DisableKeepAlives matters for the goroutine-leak assertion: the default Transport keeps
// a persistent readLoop and writeLoop per connection, so N streams would add ~2N
// goroutines on the CLIENT side and make a correct server look like it leaks. Turning
// keep-alive off makes each stream's goroutines disappear when the body is closed, so the
// assertion measures the server.
var sseTestClient = &http.Client{
	Timeout: 5 * time.Second,
	Transport: &http.Transport{
		DisableKeepAlives:   true,
		MaxIdleConnsPerHost: 0,
	},
}

// openStream dials the SSE endpoint and returns the response plus a reader.
func openStream(t *testing.T, base string, ip string) (*http.Response, *bufio.Reader, func()) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, base+RouteEvents, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := sseTestClient.Do(req)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	return resp, bufio.NewReader(resp.Body), func() { _ = resp.Body.Close() }
}

// TestSSE_ContentTypeAndRetry verifies the stream preamble.
func TestSSE_ContentTypeAndRetry(t *testing.T) {
	bus := NewEventBus(BusOptions{MaxSubscribers: 4})
	srv, done := newSSETestServer(t, bus, 4)
	defer done()

	resp, rd, closeBody := openStream(t, srv.URL, "127.0.0.1")
	defer closeBody()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
	if hasAnyCORSHeader(resp.Header) {
		t.Error("SSE response carries a CORS header")
	}

	// The retry directive must be the first frame.
	line, err := rd.ReadString('\n')
	if err != nil {
		t.Fatalf("reading retry frame: %v", err)
	}
	if !strings.HasPrefix(line, "retry: ") {
		t.Errorf("first frame = %q, want a retry: directive", line)
	}
}

// TestSSE_DeliversPublishedEvents verifies end-to-end delivery.
func TestSSE_DeliversPublishedEvents(t *testing.T) {
	bus := NewEventBus(BusOptions{MaxSubscribers: 4})
	srv, done := newSSETestServer(t, bus, 4)
	defer done()

	resp, rd, closeBody := openStream(t, srv.URL, "127.0.0.1")
	defer closeBody()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	bus.Publish(NewEvent(EventRaftTransition, 1, map[string]string{"to": "leader"}))

	deadline := time.Now().Add(3 * time.Second)
	sawEvent := false
	for time.Now().Before(deadline) && !sawEvent {
		line, err := rd.ReadString('\n')
		if err != nil {
			break
		}
		if strings.HasPrefix(line, "event: ") {
			if !strings.Contains(line, EventRaftTransition) {
				t.Errorf("event line = %q, want %q", line, EventRaftTransition)
			}
			sawEvent = true
		}
	}
	if !sawEvent {
		t.Error("published event was never delivered to the stream")
	}
}

// TestSSE_Heartbeat verifies the idle keep-alive comment.
func TestSSE_Heartbeat(t *testing.T) {
	bus := NewEventBus(BusOptions{MaxSubscribers: 4})
	srv, done := newSSETestServer(t, bus, 4)
	defer done()

	resp, rd, closeBody := openStream(t, srv.URL, "127.0.0.1")
	defer closeBody()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	deadline := time.Now().Add(3 * time.Second)
	sawPing := false
	for time.Now().Before(deadline) && !sawPing {
		line, err := rd.ReadString('\n')
		if err != nil {
			break
		}
		if strings.HasPrefix(line, ": ping") {
			sawPing = true
		}
	}
	if !sawPing {
		t.Error("no heartbeat emitted on an idle stream")
	}
}

// TestSSE_GlobalCapacityRefusesExcess is the SEC-8 acceptance case: many concurrent
// connections, at most the cap succeed, and every refusal happens before allocation.
func TestSSE_GlobalCapacityRefusesExcess(t *testing.T) {
	bus := NewEventBus(BusOptions{MaxSubscribers: 3})
	srv, done := newSSETestServer(t, bus, 0) // perIP disabled so the global cap governs
	defer done()

	type result struct {
		status int
	}
	results := make(chan result, 20)
	bodies := make([]func(), 0, 20)

	for i := 0; i < 20; i++ {
		resp, _, closeBody := openStream(t, srv.URL, "127.0.0.1")
		bodies = append(bodies, closeBody)
		results <- result{status: resp.StatusCode}
	}
	for _, c := range bodies {
		defer c()
	}

	ok, refused := 0, 0
	for i := 0; i < 20; i++ {
		switch (<-results).status {
		case http.StatusOK:
			ok++
		case http.StatusServiceUnavailable:
			refused++
		default:
			t.Errorf("unexpected status")
		}
	}
	if ok > 3 {
		t.Errorf("%d streams accepted with a global cap of 3", ok)
	}
	if refused != 20-ok {
		t.Errorf("refused = %d, want %d", refused, 20-ok)
	}
	if ok == 0 {
		t.Error("no stream was accepted at all")
	}
	t.Logf("accepted=%d refused=%d (cap 3)", ok, refused)
}

// TestSSE_PerIPLimit verifies the per-IP budget.
func TestSSE_PerIPLimit(t *testing.T) {
	bus := NewEventBus(BusOptions{MaxSubscribers: 50})
	srv, done := newSSETestServer(t, bus, 2)
	defer done()

	var closes []func()
	okCount := 0
	for i := 0; i < 6; i++ {
		resp, _, closeBody := openStream(t, srv.URL, "127.0.0.1")
		closes = append(closes, closeBody)
		if resp.StatusCode == http.StatusOK {
			okCount++
		} else if resp.StatusCode != http.StatusServiceUnavailable {
			t.Errorf("unexpected status %d", resp.StatusCode)
		}
	}
	for _, c := range closes {
		c()
	}
	// httptest clients all present 127.0.0.1, so the per-IP cap must bind at 2.
	if okCount > 2 {
		t.Errorf("accepted %d streams from one IP with a per-IP cap of 2", okCount)
	}
	if okCount == 0 {
		t.Error("no stream accepted")
	}
	t.Logf("per-IP accepted=%d (cap 2)", okCount)
}

// TestSSE_DisconnectReleasesSlot verifies resources are returned on disconnect, which is
// what prevents a leak across many short-lived connections.
func TestSSE_DisconnectReleasesSlot(t *testing.T) {
	bus := NewEventBus(BusOptions{MaxSubscribers: 2})
	srv, done := newSSETestServer(t, bus, 0)
	defer done()

	for round := 0; round < 8; round++ {
		resp, _, closeBody := openStream(t, srv.URL, "127.0.0.1")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("round %d: status %d, want 200 (slot not released)", round, resp.StatusCode)
		}
		closeBody()
		// Give the server a moment to observe the disconnect.
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) && bus.Subscribers() > 0 {
			time.Sleep(10 * time.Millisecond)
		}
		if bus.Subscribers() != 0 {
			t.Fatalf("round %d: %d subscribers left after disconnect", round, bus.Subscribers())
		}
	}
}

// TestSSE_NoGoroutineLeak is the SEC-8 acceptance case for leak-freedom.
func TestSSE_NoGoroutineLeak(t *testing.T) {
	bus := NewEventBus(BusOptions{MaxSubscribers: 4})
	srv, done := newSSETestServer(t, bus, 0)

	// Warm up so one-off runtime goroutines are already counted.
	warm, _, closeWarm := openStream(t, srv.URL, "127.0.0.1")
	_ = warm
	time.Sleep(50 * time.Millisecond)
	before := runtime.NumGoroutine()
	closeWarm()

	for i := 0; i < 30; i++ {
		_, _, closeBody := openStream(t, srv.URL, "127.0.0.1")
		closeBody()
	}
	// Allow the server to observe each disconnect and unwind.
	time.Sleep(300 * time.Millisecond)
	after := runtime.NumGoroutine()
	done()

	// The authoritative invariant: the server holds no subscriptions and no per-IP slots.
	if bus.Subscribers() != 0 {
		t.Errorf("SECURITY/LEAK: %d subscribers still registered after all clients left", bus.Subscribers())
	}
	// Secondary signal: with keep-alives disabled, a leaking handler would still show up.
	if after-before > 10 {
		t.Errorf("goroutine leak suspected: before=%d after=%d", before, after)
	}
	t.Logf("goroutines before=%d after=%d; subscribers=%d", before, after, bus.Subscribers())
}

// TestSSE_ContextCancelStopsStream verifies the handler honours r.Context().
func TestSSE_ContextCancelStopsStream(t *testing.T) {
	bus := NewEventBus(BusOptions{MaxSubscribers: 4})
	h := newSSEStreamHandler(bus)
	h.interval = time.Hour

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, RouteEvents, nil).WithContext(ctx)
	req.RemoteAddr = "127.0.0.1:12345"

	done := make(chan struct{})
	go func() {
		h.ServeHTTP(httptest.NewRecorder(), req)
		close(done)
	}()

	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("handler did not return after context cancellation")
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && bus.Subscribers() > 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if bus.Subscribers() != 0 {
		t.Errorf("subscriber leaked after cancellation: %d", bus.Subscribers())
	}
}

// TestSSE_RejectsNonGET verifies method enforcement on the stream.
func TestSSE_RejectsNonGET(t *testing.T) {
	bus := NewEventBus(BusOptions{})
	h := newSSEStreamHandler(bus)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, RouteEvents, nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST = %d, want 405", rec.Code)
	}
	if allow := rec.Header().Get("Allow"); allow != http.MethodGet {
		t.Errorf("Allow = %q, want GET", allow)
	}
}

// TestRemoteIP_IgnoresForwardedHeader pins the anti-spoofing decision.
func TestRemoteIP_IgnoresForwardedHeader(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, RouteEvents, nil)
	req.RemoteAddr = "10.0.0.9:5555"
	req.Header.Set("X-Forwarded-For", "1.2.3.4")
	if got := remoteIP(req); got != "10.0.0.9" {
		t.Errorf("remoteIP = %q, want 10.0.0.9 (XFF must be ignored)", got)
	}
}

// TestIPLimiter covers the per-IP accounting.
func TestIPLimiter(t *testing.T) {
	l := newIPLimiter(2)
	if !l.acquire("a") || !l.acquire("a") {
		t.Fatal("first two acquires should succeed")
	}
	if l.acquire("a") {
		t.Error("third acquire should be refused")
	}
	if !l.acquire("b") {
		t.Error("a different IP should be unaffected")
	}
	if l.count("a") != 2 {
		t.Errorf("count(a) = %d, want 2", l.count("a"))
	}
	l.release("a")
	if l.count("a") != 1 {
		t.Errorf("count(a) after release = %d, want 1", l.count("a"))
	}
	l.release("a")
	if l.count("a") != 0 {
		t.Errorf("count(a) after full release = %d, want 0 (entry should be deleted)", l.count("a"))
	}
	// Releasing an unknown IP must be safe.
	l.release("zzz")
	var nilLimiter *ipLimiter
	if !nilLimiter.acquire("x") || nilLimiter.count("x") != 0 {
		t.Error("nil limiter should be permissive")
	}
}
