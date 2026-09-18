package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestPprofServer_Lifecycle verifies that an isolated PprofServer can be started on
// loopback, serves all standard pprof diagnostic endpoints, and shuts down cleanly.
func TestPprofServer_Lifecycle(t *testing.T) {
	srv, err := NewPprofServer("127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to create PprofServer: %v", err)
	}

	addr := srv.Addr()
	if addr == nil {
		t.Fatalf("expected non-nil addr from bound listener")
	}
	tcpAddr, ok := addr.(*net.TCPAddr)
	if !ok || tcpAddr.Port <= 0 {
		t.Fatalf("expected valid TCPAddr with port > 0, got %v", addr)
	}

	if err := srv.Start(); err != nil {
		t.Fatalf("failed to start PprofServer: %v", err)
	}

	baseURL := fmt.Sprintf("http://127.0.0.1:%d", tcpAddr.Port)
	client := &http.Client{
		Timeout: 5 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse // Do not follow redirects automatically in tests
		},
	}
	defer client.CloseIdleConnections()

	// 1. Root and /debug/pprof redirects
	t.Run("Redirects", func(t *testing.T) {
		for _, path := range []string{"/", "/debug/pprof"} {
			resp, err := client.Get(baseURL + path)
			if err != nil {
				t.Fatalf("GET %s failed: %v", path, err)
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusMovedPermanently {
				t.Errorf("GET %s: expected %d MovedPermanently, got %d", path, http.StatusMovedPermanently, resp.StatusCode)
			}
			loc := resp.Header.Get("Location")
			if loc != "/debug/pprof/" {
				t.Errorf("GET %s: expected Location '/debug/pprof/', got %q", path, loc)
			}
		}
	})

	// 2. Index endpoint
	t.Run("Index", func(t *testing.T) {
		resp, err := client.Get(baseURL + "/debug/pprof/")
		if err != nil {
			t.Fatalf("GET /debug/pprof/ failed: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("expected 200 OK, got %d", resp.StatusCode)
		}
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("failed to read body: %v", err)
		}
		bodyStr := string(body)
		expectedProfiles := []string{
			"heap", "goroutine", "allocs", "block", "mutex", "threadcreate", "profile", "trace",
		}
		for _, p := range expectedProfiles {
			if !strings.Contains(bodyStr, p) {
				t.Errorf("expected index to contain profile link %q", p)
			}
		}
	})

	// 3. Cmdline endpoint
	t.Run("Cmdline", func(t *testing.T) {
		resp, err := client.Get(baseURL + "/debug/pprof/cmdline")
		if err != nil {
			t.Fatalf("GET /debug/pprof/cmdline failed: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("expected 200 OK, got %d", resp.StatusCode)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
	})

	// 4. Profiles that return payloads
	profileEndpoints := []string{
		"/debug/pprof/heap",
		"/debug/pprof/goroutine",
		"/debug/pprof/allocs",
		"/debug/pprof/threadcreate",
	}
	for _, ep := range profileEndpoints {
		t.Run(ep, func(t *testing.T) {
			resp, err := client.Get(baseURL + ep)
			if err != nil {
				t.Fatalf("GET %s failed: %v", ep, err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("GET %s: expected 200 OK, got %d", ep, resp.StatusCode)
			}
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("failed to read body for %s: %v", ep, err)
			}
			if len(body) == 0 {
				t.Errorf("GET %s: expected non-empty profile payload", ep)
			}
		})
	}

	// 5. CPU profile generation (1 second)
	t.Run("CPUProfile", func(t *testing.T) {
		cpuClient := &http.Client{Timeout: 10 * time.Second}
		defer cpuClient.CloseIdleConnections()
		resp, err := cpuClient.Get(baseURL + "/debug/pprof/profile?seconds=1")
		if err != nil {
			t.Fatalf("GET /debug/pprof/profile?seconds=1 failed: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("expected 200 OK, got %d", resp.StatusCode)
		}
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("failed to read cpu profile: %v", err)
		}
		if len(body) == 0 {
			t.Errorf("expected non-empty CPU profile payload")
		}
	})

	// 6. Unknown 404
	t.Run("NotFound", func(t *testing.T) {
		resp, err := client.Get(baseURL + "/invalid/endpoint")
		if err != nil {
			t.Fatalf("GET /invalid/endpoint failed: %v", err)
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, resp.Body)
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("expected 404 NotFound, got %d", resp.StatusCode)
		}
	})

	// 7. Graceful Shutdown & Idempotence
	t.Run("Shutdown", func(t *testing.T) {
		client.CloseIdleConnections()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		if err := srv.Shutdown(ctx); err != nil {
			t.Fatalf("shutdown failed: %v", err)
		}

		// Second call must be idempotent and succeed without error
		if err := srv.Shutdown(ctx); err != nil {
			t.Fatalf("second shutdown call failed: %v", err)
		}

		// Verify server refuses new connections
		conn, err := net.DialTimeout("tcp", addr.String(), 200*time.Millisecond)
		if err == nil {
			conn.Close()
			t.Fatalf("expected connection to be refused after shutdown")
		}
	})
}

// TestPprofServer_LoopbackRejection verifies strict loopback-only binding enforcement.
func TestPprofServer_LoopbackRejection(t *testing.T) {
	invalidAddresses := []string{
		"0.0.0.0:6060",
		"::0:6060",
		"192.168.1.100:6060",
		"10.0.0.1:6060",
		"example.com:6060",
		"127.0.0.1:-1",
		"127.0.0.1:65536",
		"127.0.0.1:abc",
		"no-port-spec",
	}

	for _, addr := range invalidAddresses {
		t.Run(addr, func(t *testing.T) {
			srv, err := NewPprofServer(addr)
			if err == nil {
				if srv != nil {
					_ = srv.Shutdown(context.Background())
				}
				t.Fatalf("expected error creating PprofServer on %q, got nil", addr)
			}
		})
	}
}

// TestPprofServer_ShutdownWithoutStart verifies clean resource release when
// Shutdown is invoked prior to calling Start.
func TestPprofServer_ShutdownWithoutStart(t *testing.T) {
	srv, err := NewPprofServer("127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to create PprofServer: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown without start failed: %v", err)
	}

	// Double shutdown
	if err := srv.Shutdown(ctx); err != nil {
		t.Fatalf("second shutdown failed: %v", err)
	}
}

// TestPprofServer_DoubleStart verifies that calling Start twice returns an error.
func TestPprofServer_DoubleStart(t *testing.T) {
	srv, err := NewPprofServer("127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to create PprofServer: %v", err)
	}
	defer func() { _ = srv.Shutdown(context.Background()) }()

	if err := srv.Start(); err != nil {
		t.Fatalf("initial start failed: %v", err)
	}
	if err := srv.Start(); err == nil {
		t.Fatalf("expected error on second Start() call, got nil")
	}
}

// TestPprofServer_ShutdownTimeoutFallback verifies forceful close on context expiration.
func TestPprofServer_ShutdownTimeoutFallback(t *testing.T) {
	srv, err := NewPprofServer("127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to create PprofServer: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("start failed: %v", err)
	}

	// Already expired context
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()

	err = srv.Shutdown(ctx)
	if err == nil {
		t.Fatalf("expected context deadline error, got nil")
	}
}

// TestPprofServer_ConcurrentRequests tests that concurrent profile retrieval functions
// cleanly and without race conditions.
func TestPprofServer_ConcurrentRequests(t *testing.T) {
	srv, err := NewPprofServer("127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to create PprofServer: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("failed to start: %v", err)
	}
	defer func() { _ = srv.Shutdown(context.Background()) }()

	port := srv.Addr().(*net.TCPAddr).Port
	endpoints := []string{
		fmt.Sprintf("http://127.0.0.1:%d/debug/pprof/heap", port),
		fmt.Sprintf("http://127.0.0.1:%d/debug/pprof/goroutine", port),
		fmt.Sprintf("http://127.0.0.1:%d/debug/pprof/allocs", port),
		fmt.Sprintf("http://127.0.0.1:%d/debug/pprof/", port),
	}

	const numWorkers = 8
	const requestsPerWorker = 5
	var wg sync.WaitGroup
	wg.Add(numWorkers)

	client := &http.Client{Timeout: 3 * time.Second}
	defer client.CloseIdleConnections()

	for i := 0; i < numWorkers; i++ {
		go func(workerID int) {
			defer wg.Done()
			for j := 0; j < requestsPerWorker; j++ {
				ep := endpoints[(workerID+j)%len(endpoints)]
				resp, err := client.Get(ep)
				if err != nil {
					t.Errorf("worker %d: GET %s failed: %v", workerID, ep, err)
					return
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				if resp.StatusCode != http.StatusOK {
					t.Errorf("worker %d: GET %s status = %d", workerID, ep, resp.StatusCode)
				}
			}
		}(i)
	}

	wg.Wait()
}
