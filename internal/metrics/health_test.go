package metrics

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHealth_LivenessHandler_HTTPContract(t *testing.T) {
	// 1. Healthy (live = true)
	handlerHealthy := LivenessHandler(func(ctx context.Context) (bool, string) {
		return true, ""
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/live", nil)
	handlerHealthy.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected HTTP 200, got %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != ContentTypeJSON {
		t.Fatalf("expected Content-Type %q, got %q", ContentTypeJSON, ct)
	}
	if nosniff := rec.Header().Get("X-Content-Type-Options"); nosniff != "nosniff" {
		t.Fatalf("expected X-Content-Type-Options nosniff, got %q", nosniff)
	}

	var liveResp LivenessResponse
	if err := json.NewDecoder(rec.Body).Decode(&liveResp); err != nil {
		t.Fatalf("failed to decode JSON response: %v", err)
	}
	if liveResp.Status != "UP" {
		t.Fatalf("expected status UP, got %q", liveResp.Status)
	}
	if liveResp.Reason != "" {
		t.Fatalf("expected empty reason on UP, got %q", liveResp.Reason)
	}

	// 2. Unhealthy (live = false, reason = terminating)
	handlerDown := LivenessHandler(func(ctx context.Context) (bool, string) {
		return false, "terminating"
	})

	recDown := httptest.NewRecorder()
	reqDown := httptest.NewRequest(http.MethodGet, "/live", nil)
	handlerDown.ServeHTTP(recDown, reqDown)

	if recDown.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected HTTP 503, got %d", recDown.Code)
	}
	var downResp LivenessResponse
	if err := json.NewDecoder(recDown.Body).Decode(&downResp); err != nil {
		t.Fatalf("failed to decode JSON response: %v", err)
	}
	if downResp.Status != "DOWN" {
		t.Fatalf("expected status DOWN, got %q", downResp.Status)
	}
	if downResp.Reason != "terminating" {
		t.Fatalf("expected reason terminating, got %q", downResp.Reason)
	}

	// 3. Method Not Allowed (POST, PUT, DELETE)
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		recMethod := httptest.NewRecorder()
		reqMethod := httptest.NewRequest(method, "/live", nil)
		handlerHealthy.ServeHTTP(recMethod, reqMethod)

		if recMethod.Code != http.StatusMethodNotAllowed {
			t.Fatalf("expected HTTP 405 for %s, got %d", method, recMethod.Code)
		}
		if allow := recMethod.Header().Get("Allow"); allow != http.MethodGet {
			t.Fatalf("expected Allow: GET, got %q", allow)
		}
	}
}

func TestHealth_ReadinessHandler_HTTPContract(t *testing.T) {
	// 1. Ready standalone
	handlerReady := ReadinessHandler(func(ctx context.Context) (bool, ReadinessDetails) {
		return true, ReadinessDetails{
			Mode: "standalone",
			Disk: "healthy",
		}
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/ready", nil)
	handlerReady.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected HTTP 200, got %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != ContentTypeJSON {
		t.Fatalf("expected Content-Type %q, got %q", ContentTypeJSON, ct)
	}

	var readyResp ReadinessResponse
	if err := json.NewDecoder(rec.Body).Decode(&readyResp); err != nil {
		t.Fatalf("failed to decode JSON response: %v", err)
	}
	if readyResp.Status != "READY" || readyResp.Mode != "standalone" || readyResp.Disk != "healthy" {
		t.Fatalf("unexpected ready response: %+v", readyResp)
	}

	// 2. Ready cluster leader
	handlerLeader := ReadinessHandler(func(ctx context.Context) (bool, ReadinessDetails) {
		return true, ReadinessDetails{
			Mode: "cluster",
			Role: "leader",
			Term: 5,
			Disk: "healthy",
		}
	})
	recLeader := httptest.NewRecorder()
	handlerLeader.ServeHTTP(recLeader, httptest.NewRequest(http.MethodGet, "/ready", nil))
	if recLeader.Code != http.StatusOK {
		t.Fatalf("expected HTTP 200, got %d", recLeader.Code)
	}
	var leaderResp ReadinessResponse
	_ = json.NewDecoder(recLeader.Body).Decode(&leaderResp)
	if leaderResp.Role != "leader" || leaderResp.Term != 5 {
		t.Fatalf("unexpected leader ready response: %+v", leaderResp)
	}

	// 3. Not Ready failure states
	failureCases := []struct {
		name       string
		details    ReadinessDetails
		wantReason string
	}{
		{
			name: "engine recovering",
			details: ReadinessDetails{
				Reason: "engine_recovering",
				Disk:   "healthy",
			},
			wantReason: "engine_recovering",
		},
		{
			name: "storage poisoned",
			details: ReadinessDetails{
				Reason: "storage_poisoned",
				Disk:   "healthy",
			},
			wantReason: "storage_poisoned",
		},
		{
			name: "disk storage exhausted",
			details: ReadinessDetails{
				Reason: "disk_storage_exhausted",
				Disk:   "critical",
			},
			wantReason: "disk_storage_exhausted",
		},
		{
			name: "no leader or quorum",
			details: ReadinessDetails{
				Mode:   "cluster",
				Role:   "leader",
				Reason: "no_leader_or_quorum",
				Disk:   "healthy",
			},
			wantReason: "no_leader_or_quorum",
		},
	}

	for _, fc := range failureCases {
		t.Run(fc.name, func(t *testing.T) {
			handler := ReadinessHandler(func(ctx context.Context) (bool, ReadinessDetails) {
				return false, fc.details
			})
			recFail := httptest.NewRecorder()
			handler.ServeHTTP(recFail, httptest.NewRequest(http.MethodGet, "/ready", nil))

			if recFail.Code != http.StatusServiceUnavailable {
				t.Fatalf("expected HTTP 503, got %d", recFail.Code)
			}
			var failResp ReadinessResponse
			if err := json.NewDecoder(recFail.Body).Decode(&failResp); err != nil {
				t.Fatalf("failed to decode JSON response: %v", err)
			}
			if failResp.Status != "DOWN" {
				t.Fatalf("expected status DOWN, got %q", failResp.Status)
			}
			if failResp.Reason != fc.wantReason {
				t.Fatalf("expected reason %q, got %q", fc.wantReason, failResp.Reason)
			}
		})
	}
}

func TestHealth_InformationDisclosure_NoSensitiveData(t *testing.T) {
	// Adversarial test: verify health handlers never leak database paths, error strings, or memory addresses
	handler := ReadinessHandler(func(ctx context.Context) (bool, ReadinessDetails) {
		return false, ReadinessDetails{
			Mode:   "standalone",
			Disk:   "critical",
			Reason: "disk_storage_exhausted",
		}
	})

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ready", nil))
	body, _ := io.ReadAll(rec.Body)
	bodyStr := string(body)

	// Check forbidden substrings
	forbidden := []string{
		"/var/", "/Users/", "/home/", "0x", "panic", "goroutine",
		".wal", "MANIFEST", "CURRENT", "nil pointer", "runtime error",
	}
	for _, f := range forbidden {
		if strings.Contains(bodyStr, f) {
			t.Fatalf("body leaked sensitive information %q: %s", f, bodyStr)
		}
	}
}

func TestServer_HealthEndpointsIntegrated(t *testing.T) {
	srv, err := NewServer("127.0.0.1:0", NewRegistry())
	if err != nil {
		t.Fatalf("failed to create server: %v", err)
	}
	defer srv.Close()

	if err := srv.Start(); err != nil {
		t.Fatalf("failed to start server: %v", err)
	}

	addr := srv.Addr().String()
	client := &http.Client{}

	// Default unconfigured liveness returns UP (200)
	resp, err := client.Get("http://" + addr + "/live")
	if err != nil {
		t.Fatalf("GET /live failed: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for /live, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Default unconfigured readiness returns DOWN (503)
	resp, err = client.Get("http://" + addr + "/ready")
	if err != nil {
		t.Fatalf("GET /ready failed: %v", err)
	}
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 for unconfigured /ready, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Configure dynamic readiness
	srv.SetReadinessCheck(func(ctx context.Context) (bool, ReadinessDetails) {
		return true, ReadinessDetails{
			Mode: "standalone",
			Disk: "healthy",
		}
	})

	resp, err = client.Get("http://" + addr + "/ready")
	if err != nil {
		t.Fatalf("GET /ready failed: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for ready /ready, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}
