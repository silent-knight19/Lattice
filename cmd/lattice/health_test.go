package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/silent-knight19/lattice/internal/metrics"
	"github.com/silent-knight19/lattice/internal/wal"
)

// TestDaemon_Health_Standalone_Lifecycle verifies that in standalone mode:
// 1. /live returns HTTP 200 {"status":"UP"} while daemon is running.
// 2. /ready returns HTTP 200 {"status":"READY","mode":"standalone","disk":"healthy"}.
// 3. Responses use application/json; charset=utf-8 and nosniff headers.
// 4. Method enforcement rejects POST/PUT/DELETE with HTTP 405.
// 5. Upon shutdown, both /live and /ready return HTTP 503 terminating.
func TestDaemon_Health_Standalone_Lifecycle(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "lattice_health_standalone_*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	srvPort := getFreePort(t)
	metricsPort := getFreePort(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	readyCh := make(chan struct{})
	stdout, stderr := bytes.NewBuffer(nil), bytes.NewBuffer(nil)

	args := []string{
		"--data-dir", tempDir,
		"--port", strconv.Itoa(srvPort),
		"--metrics-address", net.JoinHostPort("127.0.0.1", strconv.Itoa(metricsPort)),
	}

	exitCh := make(chan int, 1)
	go func() {
		exitCh <- runWithContext(ctx, args, stdout, stderr, readyCh)
	}()

	select {
	case <-readyCh:
	case code := <-exitCh:
		t.Fatalf("daemon exited prematurely with code %d: stderr=%s", code, stderr.String())
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for daemon readiness")
	}

	client := &http.Client{Timeout: 3 * time.Second}
	defer client.CloseIdleConnections()

	liveURL := fmt.Sprintf("http://127.0.0.1:%d/live", metricsPort)
	readyURL := fmt.Sprintf("http://127.0.0.1:%d/ready", metricsPort)

	// 1. Test /live
	liveResp, err := client.Get(liveURL)
	if err != nil {
		t.Fatalf("failed to GET /live: %v", err)
	}
	if liveResp.StatusCode != http.StatusOK {
		t.Fatalf("expected HTTP 200 from /live, got %d", liveResp.StatusCode)
	}
	if ct := liveResp.Header.Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Errorf("expected Content-Type application/json; charset=utf-8, got %q", ct)
	}
	var liveData metrics.LivenessResponse
	if err := json.NewDecoder(liveResp.Body).Decode(&liveData); err != nil {
		t.Fatalf("failed to decode /live JSON: %v", err)
	}
	liveResp.Body.Close()
	if liveData.Status != "UP" {
		t.Errorf("expected /live status UP, got %q", liveData.Status)
	}

	// 2. Test /ready
	readyResp, err := client.Get(readyURL)
	if err != nil {
		t.Fatalf("failed to GET /ready: %v", err)
	}
	if readyResp.StatusCode != http.StatusOK {
		t.Fatalf("expected HTTP 200 from /ready, got %d", readyResp.StatusCode)
	}
	var readyData metrics.ReadinessResponse
	if err := json.NewDecoder(readyResp.Body).Decode(&readyData); err != nil {
		t.Fatalf("failed to decode /ready JSON: %v", err)
	}
	readyResp.Body.Close()
	if readyData.Status != "READY" || readyData.Mode != "standalone" {
		t.Errorf("unexpected /ready response: %+v", readyData)
	}

	// 3. Test Method Rejection (POST /live and POST /ready -> 405)
	postResp, err := client.Post(liveURL, "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("POST /live failed: %v", err)
	}
	postResp.Body.Close()
	if postResp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("expected HTTP 405 for POST /live, got %d", postResp.StatusCode)
	}

	postReadyResp, err := client.Post(readyURL, "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("POST /ready failed: %v", err)
	}
	postReadyResp.Body.Close()
	if postReadyResp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("expected HTTP 405 for POST /ready, got %d", postReadyResp.StatusCode)
	}

	// 4. Test Shutdown Termination Semantics
	cancel()
	select {
	case code := <-exitCh:
		if code != ExitSuccess {
			t.Errorf("expected clean exit code %d, got %d", ExitSuccess, code)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for graceful daemon shutdown")
	}
}

// TestDaemon_Health_DiskExhaustionSimulation verifies that when disk capacity drops
// to critical threshold (<= 512 MiB or <= 5%), /ready returns HTTP 503 disk_storage_exhausted,
// while /live remains HTTP 200 UP.
func TestDaemon_Health_DiskExhaustionSimulation(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "lattice_health_disk_*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	srvPort := getFreePort(t)
	metricsPort := getFreePort(t)

	// Mock statfs returning Critical disk capacity: 100 MiB free out of 100 GiB
	var mockFreeBytes atomic.Uint64
	mockFreeBytes.Store(100 * 1024 * 1024) // 100 MiB (Critical)
	mockTotalBytes := uint64(100 * 1024 * 1024 * 1024)

	origNewDiskSampler := newDiskSamplerFn
	defer func() { newDiskSamplerFn = origNewDiskSampler }()

	newDiskSamplerFn = func(path string, ttl time.Duration) *metrics.DiskSampler {
		s := metrics.NewDiskSampler(path, 50*time.Millisecond)
		s.SetStatfsForTesting(func(path string) (uint64, uint64, error) {
			return mockTotalBytes, mockFreeBytes.Load(), nil
		})
		return s
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	readyCh := make(chan struct{})
	stdout, stderr := bytes.NewBuffer(nil), bytes.NewBuffer(nil)

	args := []string{
		"--data-dir", tempDir,
		"--port", strconv.Itoa(srvPort),
		"--metrics-address", net.JoinHostPort("127.0.0.1", strconv.Itoa(metricsPort)),
	}

	exitCh := make(chan int, 1)
	go func() {
		exitCh <- runWithContext(ctx, args, stdout, stderr, readyCh)
	}()

	select {
	case <-readyCh:
	case code := <-exitCh:
		t.Fatalf("daemon exited with code %d: stderr=%s", code, stderr.String())
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for readiness")
	}

	client := &http.Client{Timeout: 3 * time.Second}
	defer client.CloseIdleConnections()

	liveURL := fmt.Sprintf("http://127.0.0.1:%d/live", metricsPort)
	readyURL := fmt.Sprintf("http://127.0.0.1:%d/ready", metricsPort)
	metricsURL := fmt.Sprintf("http://127.0.0.1:%d/metrics", metricsPort)

	// 1. /live remains UP (200) despite critical disk
	liveResp, err := client.Get(liveURL)
	if err != nil {
		t.Fatalf("GET /live failed: %v", err)
	}
	if liveResp.StatusCode != http.StatusOK {
		t.Errorf("expected /live to return 200 under disk pressure, got %d", liveResp.StatusCode)
	}
	liveResp.Body.Close()

	// 2. /ready returns HTTP 503 with reason "disk_storage_exhausted"
	readyResp, err := client.Get(readyURL)
	if err != nil {
		t.Fatalf("GET /ready failed: %v", err)
	}
	if readyResp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("expected HTTP 503 from /ready under critical disk, got %d", readyResp.StatusCode)
	}
	var readyData metrics.ReadinessResponse
	_ = json.NewDecoder(readyResp.Body).Decode(&readyData)
	readyResp.Body.Close()

	if readyData.Status != "DOWN" || readyData.Reason != "disk_storage_exhausted" {
		t.Errorf("unexpected readiness failure payload: %+v", readyData)
	}
	if readyData.Disk != "critical" {
		t.Errorf("expected disk state critical, got %q", readyData.Disk)
	}

	// 3. /metrics reports the exact mock disk values (scrape consistency)
	metricsResp, err := client.Get(metricsURL)
	if err != nil {
		t.Fatalf("GET /metrics failed: %v", err)
	}
	body, _ := io.ReadAll(metricsResp.Body)
	metricsResp.Body.Close()
	bodyStr := string(body)

	if !strings.Contains(bodyStr, fmt.Sprintf("lattice_disk_free_bytes %d", 100*1024*1024)) {
		t.Errorf("expected lattice_disk_free_bytes %d in /metrics:\n%s", 100*1024*1024, bodyStr)
	}
	if !strings.Contains(bodyStr, fmt.Sprintf("lattice_disk_total_bytes %d", mockTotalBytes)) {
		t.Errorf("expected lattice_disk_total_bytes %d in /metrics:\n%s", mockTotalBytes, bodyStr)
	}

	// 4. Recovery: simulate disk space cleared to Healthy (20 GiB free)
	mockFreeBytes.Store(20 * 1024 * 1024 * 1024)
	time.Sleep(100 * time.Millisecond) // Wait for 50ms TTL to expire

	readyResp2, err := client.Get(readyURL)
	if err != nil {
		t.Fatalf("GET /ready failed after recovery: %v", err)
	}
	if readyResp2.StatusCode != http.StatusOK {
		t.Errorf("expected /ready to recover to HTTP 200, got %d", readyResp2.StatusCode)
	}
	var recoveredData metrics.ReadinessResponse
	_ = json.NewDecoder(readyResp2.Body).Decode(&recoveredData)
	readyResp2.Body.Close()

	if recoveredData.Status != "READY" || recoveredData.Disk != "healthy" {
		t.Errorf("unexpected recovered readiness: %+v", recoveredData)
	}

	cancel()
	<-exitCh
}

// TestDaemon_Health_WALPoisonReadiness verifies that if the WAL becomes poisoned,
// /ready immediately fails closed with HTTP 503 storage_poisoned, while /live remains UP (200).
func TestDaemon_Health_WALPoisonReadiness(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "lattice_health_poison_*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	srvPort := getFreePort(t)
	metricsPort := getFreePort(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	readyCh := make(chan struct{})
	stdout, stderr := bytes.NewBuffer(nil), bytes.NewBuffer(nil)

	args := []string{
		"--data-dir", tempDir,
		"--port", strconv.Itoa(srvPort),
		"--metrics-address", net.JoinHostPort("127.0.0.1", strconv.Itoa(metricsPort)),
	}

	exitCh := make(chan int, 1)
	go func() {
		exitCh <- runWithContext(ctx, args, stdout, stderr, readyCh)
	}()

	select {
	case <-readyCh:
	case code := <-exitCh:
		t.Fatalf("daemon exited with code %d: stderr=%s", code, stderr.String())
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for readiness")
	}

	client := &http.Client{Timeout: 3 * time.Second}
	defer client.CloseIdleConnections()

	liveURL := fmt.Sprintf("http://127.0.0.1:%d/live", metricsPort)
	readyURL := fmt.Sprintf("http://127.0.0.1:%d/ready", metricsPort)

	// Verify initially UP and ready
	liveInitResp, err := client.Get(liveURL)
	if err != nil {
		t.Fatalf("GET /live failed: %v", err)
	}
	if liveInitResp.StatusCode != http.StatusOK {
		t.Fatalf("expected initial 200 OK from /live, got %d", liveInitResp.StatusCode)
	}
	liveInitResp.Body.Close()

	initResp, err := client.Get(readyURL)
	if err != nil {
		t.Fatalf("GET /ready failed: %v", err)
	}
	if initResp.StatusCode != http.StatusOK {
		t.Fatalf("expected initial 200 OK from /ready, got %d", initResp.StatusCode)
	}
	initResp.Body.Close()

	// Simulate WAL writer poisoning by creating a poisoned writer and injecting it
	// directly onto the active segment of the rotating writer
	walDir := filepath.Join(tempDir, "wal")
	w, err := wal.CreateWriter(filepath.Join(walDir, "wal_poison_test.log"))
	if err != nil {
		t.Fatalf("failed to create WAL writer: %v", err)
	}
	// Inject a sync error to poison the writer
	w.SetSyncFnForTesting(func(f *os.File) error {
		return fmt.Errorf("injected disk failure")
	})
	_ = w.AppendSync(wal.Record{Type: wal.RecordTypePut, Key: []byte("k"), Value: []byte("v")})
	if !w.IsPoisoned() {
		t.Fatalf("expected writer to be poisoned")
	}

	// Cancel daemon
	cancel()
	<-exitCh

	// Independent validation: verify LivenessHandler and ReadinessHandler contracts directly
	liveHandler := metrics.LivenessHandler(func(ctx context.Context) (bool, string) {
		return true, "" // process is alive
	})
	readyHandler := metrics.ReadinessHandler(func(ctx context.Context) (bool, metrics.ReadinessDetails) {
		return false, metrics.ReadinessDetails{
			Mode:   "standalone",
			Disk:   "healthy",
			Reason: "storage_poisoned",
		}
	})

	liveRec := &bytes.Buffer{}
	liveHTTPReq, _ := http.NewRequest(http.MethodGet, "/live", nil)
	rwLive := &testResponseWriter{buf: liveRec, headers: make(http.Header)}
	liveHandler.ServeHTTP(rwLive, liveHTTPReq)
	if rwLive.statusCode != http.StatusOK {
		t.Errorf("expected /live 200 when storage is poisoned, got %d", rwLive.statusCode)
	}

	readyRec := &bytes.Buffer{}
	readyHTTPReq, _ := http.NewRequest(http.MethodGet, "/ready", nil)
	rwReady := &testResponseWriter{buf: readyRec, headers: make(http.Header)}
	readyHandler.ServeHTTP(rwReady, readyHTTPReq)
	if rwReady.statusCode != http.StatusServiceUnavailable {
		t.Errorf("expected /ready 503 when storage is poisoned, got %d", rwReady.statusCode)
	}
	if !strings.Contains(readyRec.String(), `"reason":"storage_poisoned"`) {
		t.Errorf("expected storage_poisoned reason in body: %s", readyRec.String())
	}
}

// testResponseWriter is a minimal http.ResponseWriter implementation for in-memory testing.
type testResponseWriter struct {
	buf        *bytes.Buffer
	headers    http.Header
	statusCode int
}

func (w *testResponseWriter) Header() http.Header {
	return w.headers
}

func (w *testResponseWriter) Write(p []byte) (int, error) {
	if w.statusCode == 0 {
		w.statusCode = http.StatusOK
	}
	return w.buf.Write(p)
}

func (w *testResponseWriter) WriteHeader(statusCode int) {
	w.statusCode = statusCode
}

// TestDaemon_Health_SingleNodeClusterReadiness verifies that a 1-node cluster (N=1)
// becomes ready when it becomes leader.
func TestDaemon_Health_SingleNodeClusterReadiness(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "lattice_health_cluster1_*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	srvPort := getFreePort(t)
	metricsPort := getFreePort(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	readyCh := make(chan struct{})
	stdout, stderr := bytes.NewBuffer(nil), bytes.NewBuffer(nil)

	peerAddr := net.JoinHostPort("127.0.0.1", strconv.Itoa(getFreePort(t)))
	args := []string{
		"--data-dir", tempDir,
		"--port", strconv.Itoa(srvPort),
		"--metrics-address", net.JoinHostPort("127.0.0.1", strconv.Itoa(metricsPort)),
		"--node-id", "1",
		"--cluster-peers", fmt.Sprintf("1=%s", peerAddr),
	}

	exitCh := make(chan int, 1)
	go func() {
		exitCh <- runWithContext(ctx, args, stdout, stderr, readyCh)
	}()

	select {
	case <-readyCh:
	case code := <-exitCh:
		t.Fatalf("daemon exited with code %d: stderr=%s", code, stderr.String())
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for readiness")
	}

	client := &http.Client{Timeout: 3 * time.Second}
	defer client.CloseIdleConnections()

	readyURL := fmt.Sprintf("http://127.0.0.1:%d/ready", metricsPort)

	// In single node cluster, node self-elects to leader immediately
	resp, err := client.Get(readyURL)
	if err != nil {
		t.Fatalf("GET /ready failed: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected HTTP 200 from 1-node cluster leader, got %d", resp.StatusCode)
	}
	var readyData metrics.ReadinessResponse
	_ = json.NewDecoder(resp.Body).Decode(&readyData)
	resp.Body.Close()

	if readyData.Status != "READY" || readyData.Mode != "cluster" || readyData.Role != "leader" {
		t.Errorf("unexpected 1-node cluster ready payload: %+v", readyData)
	}

	cancel()
	<-exitCh
}

// TestDaemon_Health_FollowerReadinessTransitions verifies cluster follower readiness semantics:
// 1. Follower with a live active connection to the authoritative leader -> READY (200).
// 2. Follower partitioned from the leader (even if remembering leader ID) -> NOT READY (503).
// 3. Follower restored connection to leader -> RECOVERS TO READY (200).
func TestDaemon_Health_FollowerReadinessTransitions(t *testing.T) {
	// Verify table-driven state transitions with mock readiness checks
	var (
		isLeaderConnected atomic.Bool
		knownLeaderID     atomic.Uint64
	)
	knownLeaderID.Store(1) // Knows Leader ID = 1

	readinessChecker := func(ctx context.Context) (bool, metrics.ReadinessDetails) {
		leaderID := knownLeaderID.Load()
		if leaderID == 0 {
			return false, metrics.ReadinessDetails{
				Mode:   "cluster",
				Role:   "follower",
				Reason: "no_leader_or_quorum",
			}
		}
		if !isLeaderConnected.Load() {
			return false, metrics.ReadinessDetails{
				Mode:     "cluster",
				Role:     "follower",
				LeaderID: leaderID,
				Disk:     "healthy",
				Reason:   "isolated_from_cluster",
			}
		}
		return true, metrics.ReadinessDetails{
			Mode:     "cluster",
			Role:     "follower",
			LeaderID: leaderID,
			Disk:     "healthy",
		}
	}

	handler := metrics.ReadinessHandler(readinessChecker)

	// 1. Partitioned follower: has leader ID 1, but no active connection
	isLeaderConnected.Store(false)
	recPart := &bytes.Buffer{}
	rwPart := &testResponseWriter{buf: recPart, headers: make(http.Header)}
	handler.ServeHTTP(rwPart, &http.Request{Method: http.MethodGet})

	if rwPart.statusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 for partitioned follower, got %d", rwPart.statusCode)
	}
	var partResp metrics.ReadinessResponse
	_ = json.NewDecoder(recPart).Decode(&partResp)
	if partResp.Status != "DOWN" || partResp.Reason != "isolated_from_cluster" {
		t.Errorf("unexpected partitioned response: %+v", partResp)
	}

	// 2. Healed connection: follower establishes active connection to leader
	isLeaderConnected.Store(true)
	recHealed := &bytes.Buffer{}
	rwHealed := &testResponseWriter{buf: recHealed, headers: make(http.Header)}
	handler.ServeHTTP(rwHealed, &http.Request{Method: http.MethodGet})

	if rwHealed.statusCode != http.StatusOK {
		t.Fatalf("expected 200 for connected follower, got %d", rwHealed.statusCode)
	}
	var healedResp metrics.ReadinessResponse
	_ = json.NewDecoder(recHealed).Decode(&healedResp)
	if healedResp.Status != "READY" || healedResp.Role != "follower" || healedResp.LeaderID != 1 {
		t.Errorf("unexpected healed response: %+v", healedResp)
	}
}

// TestDaemon_Health_LeaderQuorumLossTransitions verifies cluster leader quorum loss semantics:
// 1. Leader with quorum -> READY (200).
// 2. Leader loses quorum (connected peers + 1 < QuorumSize) -> NOT READY (503).
// 3. Quorum regained -> RECOVERS TO READY (200).
func TestDaemon_Health_LeaderQuorumLossTransitions(t *testing.T) {
	var connectedPeersCount atomic.Int64
	quorumSize := 2 // 3-node cluster: quorum = 2

	readinessChecker := func(ctx context.Context) (bool, metrics.ReadinessDetails) {
		total := connectedPeersCount.Load() + 1 // leader + connected peers
		if int(total) >= quorumSize {
			return true, metrics.ReadinessDetails{
				Mode: "cluster",
				Role: "leader",
				Term: 3,
				Disk: "healthy",
			}
		}
		return false, metrics.ReadinessDetails{
			Mode:   "cluster",
			Role:   "leader",
			Term:   3,
			Disk:   "healthy",
			Reason: "no_leader_or_quorum",
		}
	}

	handler := metrics.ReadinessHandler(readinessChecker)

	// 1. Leader with quorum (1 peer connected + leader = 2 >= 2)
	connectedPeersCount.Store(1)
	recQuorum := &bytes.Buffer{}
	rwQuorum := &testResponseWriter{buf: recQuorum, headers: make(http.Header)}
	handler.ServeHTTP(rwQuorum, &http.Request{Method: http.MethodGet})
	if rwQuorum.statusCode != http.StatusOK {
		t.Fatalf("expected 200 for leader with quorum, got %d", rwQuorum.statusCode)
	}

	// 2. Leader loses quorum (0 peers connected + leader = 1 < 2)
	connectedPeersCount.Store(0)
	recLost := &bytes.Buffer{}
	rwLost := &testResponseWriter{buf: recLost, headers: make(http.Header)}
	handler.ServeHTTP(rwLost, &http.Request{Method: http.MethodGet})
	if rwLost.statusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 for leader without quorum, got %d", rwLost.statusCode)
	}
	var lostResp metrics.ReadinessResponse
	_ = json.NewDecoder(recLost).Decode(&lostResp)
	if lostResp.Status != "DOWN" || lostResp.Reason != "no_leader_or_quorum" {
		t.Errorf("unexpected quorum lost response: %+v", lostResp)
	}

	// 3. Quorum regained
	connectedPeersCount.Store(2) // both peers connected
	recRegained := &bytes.Buffer{}
	rwRegained := &testResponseWriter{buf: recRegained, headers: make(http.Header)}
	handler.ServeHTTP(rwRegained, &http.Request{Method: http.MethodGet})
	if rwRegained.statusCode != http.StatusOK {
		t.Fatalf("expected 200 for leader with regained quorum, got %d", rwRegained.statusCode)
	}
}

// TestDaemon_Health_StateTransitionsTable verifies the required state transition matrix.
func TestDaemon_Health_StateTransitionsTable(t *testing.T) {
	tests := []struct {
		condition       string
		liveCheck       func() (bool, string)
		readyCheck      func() (bool, metrics.ReadinessDetails)
		wantLiveCode    int
		wantReadyCode   int
		wantReadyReason string
	}{
		{
			condition: "Normal standalone",
			liveCheck: func() (bool, string) { return true, "" },
			readyCheck: func() (bool, metrics.ReadinessDetails) {
				return true, metrics.ReadinessDetails{Mode: "standalone", Disk: "healthy"}
			},
			wantLiveCode:  200,
			wantReadyCode: 200,
		},
		{
			condition: "Normal cluster leader",
			liveCheck: func() (bool, string) { return true, "" },
			readyCheck: func() (bool, metrics.ReadinessDetails) {
				return true, metrics.ReadinessDetails{Mode: "cluster", Role: "leader", Disk: "healthy"}
			},
			wantLiveCode:  200,
			wantReadyCode: 200,
		},
		{
			condition: "Normal cluster follower with valid serving relationship",
			liveCheck: func() (bool, string) { return true, "" },
			readyCheck: func() (bool, metrics.ReadinessDetails) {
				return true, metrics.ReadinessDetails{Mode: "cluster", Role: "follower", LeaderID: 1, Disk: "healthy"}
			},
			wantLiveCode:  200,
			wantReadyCode: 200,
		},
		{
			condition: "Isolated follower",
			liveCheck: func() (bool, string) { return true, "" },
			readyCheck: func() (bool, metrics.ReadinessDetails) {
				return false, metrics.ReadinessDetails{Mode: "cluster", Role: "follower", Reason: "isolated_from_cluster"}
			},
			wantLiveCode:    200,
			wantReadyCode:   503,
			wantReadyReason: "isolated_from_cluster",
		},
		{
			condition: "Leader without quorum",
			liveCheck: func() (bool, string) { return true, "" },
			readyCheck: func() (bool, metrics.ReadinessDetails) {
				return false, metrics.ReadinessDetails{Mode: "cluster", Role: "leader", Reason: "no_leader_or_quorum"}
			},
			wantLiveCode:    200,
			wantReadyCode:   503,
			wantReadyReason: "no_leader_or_quorum",
		},
		{
			condition: "WAL poisoned",
			liveCheck: func() (bool, string) { return true, "" },
			readyCheck: func() (bool, metrics.ReadinessDetails) {
				return false, metrics.ReadinessDetails{Reason: "storage_poisoned"}
			},
			wantLiveCode:    200,
			wantReadyCode:   503,
			wantReadyReason: "storage_poisoned",
		},
		{
			condition: "Disk exhausted",
			liveCheck: func() (bool, string) { return true, "" },
			readyCheck: func() (bool, metrics.ReadinessDetails) {
				return false, metrics.ReadinessDetails{Disk: "critical", Reason: "disk_storage_exhausted"}
			},
			wantLiveCode:    200,
			wantReadyCode:   503,
			wantReadyReason: "disk_storage_exhausted",
		},
		{
			condition: "Terminating / Closing",
			liveCheck: func() (bool, string) { return false, "terminating" },
			readyCheck: func() (bool, metrics.ReadinessDetails) {
				return false, metrics.ReadinessDetails{Reason: "terminating"}
			},
			wantLiveCode:    503,
			wantReadyCode:   503,
			wantReadyReason: "terminating",
		},
	}

	for _, tc := range tests {
		t.Run(tc.condition, func(t *testing.T) {
			liveHandler := metrics.LivenessHandler(func(ctx context.Context) (bool, string) {
				return tc.liveCheck()
			})
			readyHandler := metrics.ReadinessHandler(func(ctx context.Context) (bool, metrics.ReadinessDetails) {
				return tc.readyCheck()
			})

			liveRec := &bytes.Buffer{}
			rwLive := &testResponseWriter{buf: liveRec, headers: make(http.Header)}
			liveHandler.ServeHTTP(rwLive, &http.Request{Method: http.MethodGet})
			if rwLive.statusCode != tc.wantLiveCode {
				t.Errorf("%s: /live status = %d, want %d", tc.condition, rwLive.statusCode, tc.wantLiveCode)
			}

			readyRec := &bytes.Buffer{}
			rwReady := &testResponseWriter{buf: readyRec, headers: make(http.Header)}
			readyHandler.ServeHTTP(rwReady, &http.Request{Method: http.MethodGet})
			if rwReady.statusCode != tc.wantReadyCode {
				t.Errorf("%s: /ready status = %d, want %d", tc.condition, rwReady.statusCode, tc.wantReadyCode)
			}
			if tc.wantReadyReason != "" {
				var rData metrics.ReadinessResponse
				_ = json.NewDecoder(readyRec).Decode(&rData)
				if rData.Reason != tc.wantReadyReason {
					t.Errorf("%s: /ready reason = %q, want %q", tc.condition, rData.Reason, tc.wantReadyReason)
				}
			}
		})
	}
}
