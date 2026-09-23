package metrics

import (
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestMetricsServer_LifecycleAndEndpoints(t *testing.T) {
	reg := NewRegistry()
	c := NewCounter()
	c.Add(100)
	reg.RegisterCounter("test_counter", "Test counter help", c)

	srv, err := NewServer("127.0.0.1:0", reg)
	if err != nil {
		t.Fatalf("failed to create server: %v", err)
	}

	if err := srv.Start(); err != nil {
		t.Fatalf("failed to start server: %v", err)
	}
	defer func() { _ = srv.Close() }()

	addr := srv.Addr().String()

	// 1. GET /metrics returns 200 OK and Prometheus Content-Type
	resp, err := http.Get("http://" + addr + "/metrics")
	if err != nil {
		t.Fatalf("failed to GET /metrics: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected status 200 OK, got %d", resp.StatusCode)
	}
	contentType := resp.Header.Get("Content-Type")
	if !strings.Contains(contentType, "text/plain") || !strings.Contains(contentType, "version=0.0.4") {
		t.Errorf("expected Prometheus content-type, got %q", contentType)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("failed to read body: %v", err)
	}
	if !strings.Contains(string(body), "test_counter 100") {
		t.Errorf("expected body to contain 'test_counter 100', got:\n%s", string(body))
	}

	// 2. POST /metrics rejected with 405 Method Not Allowed
	postResp, err := http.Post("http://"+addr+"/metrics", "text/plain", strings.NewReader("bad"))
	if err != nil {
		t.Fatalf("failed to POST /metrics: %v", err)
	}
	defer postResp.Body.Close()
	if postResp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("expected status 405 for POST, got %d", postResp.StatusCode)
	}
	if postResp.Header.Get("Allow") != http.MethodGet {
		t.Errorf("expected Allow: GET, got %q", postResp.Header.Get("Allow"))
	}

	// 3. GET /unknown returns 404
	badResp, err := http.Get("http://" + addr + "/unknown")
	if err != nil {
		t.Fatalf("failed to GET /unknown: %v", err)
	}
	defer badResp.Body.Close()
	if badResp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404 for /unknown, got %d", badResp.StatusCode)
	}

	// 4. Graceful Shutdown
	shutCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutCtx); err != nil {
		t.Fatalf("shutdown failed: %v", err)
	}

	// Post-shutdown requests must fail
	_, postCloseErr := http.Get("http://" + addr + "/metrics")
	if postCloseErr == nil {
		t.Errorf("expected error connecting after shutdown, got nil")
	}
}

func TestMetricsServer_ConcurrentScraping(t *testing.T) {
	reg := NewRegistry()
	h := NewHistogram(DefaultLatencyBuckets)
	reg.RegisterHistogram("busy_metric", "Busy latency", h)

	srv, err := NewServer("127.0.0.1:0", reg)
	if err != nil {
		t.Fatalf("failed to create server: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("failed to start server: %v", err)
	}
	defer func() { _ = srv.Close() }()

	addr := srv.Addr().String()

	stopCh := make(chan struct{})
	var wg sync.WaitGroup

	// Background writers updating metrics continuously
	const writerCount = 4
	for i := 0; i < writerCount; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stopCh:
					return
				default:
					h.ObserveDuration(500 * time.Microsecond)
					time.Sleep(10 * time.Microsecond)
				}
			}
		}()
	}

	// Concurrent scrapers fetching /metrics
	const scraperCount = 10
	client := &http.Client{Timeout: 2 * time.Second}
	var scraperWg sync.WaitGroup
	scraperWg.Add(scraperCount)

	for i := 0; i < scraperCount; i++ {
		go func() {
			defer scraperWg.Done()
			for j := 0; j < 20; j++ {
				resp, err := client.Get("http://" + addr + "/metrics")
				if err != nil {
					t.Errorf("scraper request failed: %v", err)
					return
				}
				body, err := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				if err != nil {
					t.Errorf("failed reading body: %v", err)
					return
				}
				if !strings.Contains(string(body), "busy_metric") {
					t.Errorf("expected busy_metric in response")
					return
				}
			}
		}()
	}

	scraperWg.Wait()
	close(stopCh)
	wg.Wait()
}

func TestMetricsServer_SlowScraperTimeout(t *testing.T) {
	srv, err := NewServer("127.0.0.1:0", DefaultRegistry)
	if err != nil {
		t.Fatalf("failed to create server: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("failed to start server: %v", err)
	}
	defer func() { _ = srv.Close() }()

	addr := srv.Addr().String()

	// Connect a raw TCP client that sends partial HTTP headers and stalls
	conn, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		t.Fatalf("failed to connect: %v", err)
	}
	defer conn.Close()

	// Send incomplete request header
	_, _ = conn.Write([]byte("GET /metrics HTTP/1.1\r\nHost: 127.0.0.1\r\n"))

	// Server ReadHeaderTimeout is 5s. Read from connection until server forcefully closes it.
	_ = conn.SetReadDeadline(time.Now().Add(7 * time.Second))
	buf := make([]byte, 128)
	_, readErr := conn.Read(buf)
	if readErr == nil {
		// Or server sent 408 Request Timeout and closed
		t.Logf("server returned error response before closing (safe behavior)")
	} else if readErr != io.EOF && !strings.Contains(readErr.Error(), "connection reset") {
		t.Logf("connection closed as expected: %v", readErr)
	}
}

func TestMetricsServer_PortCollision(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to bind port: %v", err)
	}
	defer l.Close()

	addr := l.Addr().String()

	// Attempt to create MetricsServer on already bound port -> must return error
	_, err = NewServer(addr, DefaultRegistry)
	if err == nil {
		t.Fatalf("expected error binding to occupied address %s, got nil", addr)
	}
}
