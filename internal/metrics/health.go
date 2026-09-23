package metrics

import (
	"context"
	"encoding/json"
	"net/http"
)

// ContentTypeJSON is the canonical JSON response content type.
const ContentTypeJSON = "application/json; charset=utf-8"

// LivenessCheck evaluates whether the local process is alive.
type LivenessCheck func(ctx context.Context) (live bool, reason string)

// ReadinessDetails captures bounded operational readiness metadata.
type ReadinessDetails struct {
	Mode     string `json:"mode,omitempty"`
	Role     string `json:"role,omitempty"`
	Disk     string `json:"disk,omitempty"`
	Term     uint64 `json:"term,omitempty"`
	LeaderID uint64 `json:"leader_id,omitempty"`
	Reason   string `json:"reason,omitempty"`
}

// ReadinessCheck evaluates whether the node is ready to accept and serve traffic.
type ReadinessCheck func(ctx context.Context) (ready bool, details ReadinessDetails)

// LivenessResponse models the bounded JSON response for /live.
type LivenessResponse struct {
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
}

// ReadinessResponse models the bounded JSON response for /ready.
type ReadinessResponse struct {
	Status   string `json:"status"`
	Mode     string `json:"mode,omitempty"`
	Role     string `json:"role,omitempty"`
	Disk     string `json:"disk,omitempty"`
	Term     uint64 `json:"term,omitempty"`
	LeaderID uint64 `json:"leader_id,omitempty"`
	Reason   string `json:"reason,omitempty"`
}

// LivenessHandler returns an http.Handler serving the /live endpoint.
// Only HTTP GET is permitted; other methods are rejected with HTTP 405 Method Not Allowed.
func LivenessHandler(check LivenessCheck) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		w.Header().Set("Content-Type", ContentTypeJSON)
		w.Header().Set("X-Content-Type-Options", "nosniff")

		live := true
		reason := ""
		if check != nil {
			live, reason = check(r.Context())
		}

		resp := LivenessResponse{}
		if live {
			resp.Status = "UP"
			w.WriteHeader(http.StatusOK)
		} else {
			resp.Status = "DOWN"
			if reason == "" {
				reason = "terminating"
			}
			resp.Reason = reason
			w.WriteHeader(http.StatusServiceUnavailable)
		}

		_ = json.NewEncoder(w).Encode(resp)
	})
}

// ReadinessHandler returns an http.Handler serving the /ready endpoint.
// Only HTTP GET is permitted; other methods are rejected with HTTP 405 Method Not Allowed.
func ReadinessHandler(check ReadinessCheck) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		w.Header().Set("Content-Type", ContentTypeJSON)
		w.Header().Set("X-Content-Type-Options", "nosniff")

		ready := false
		var details ReadinessDetails
		if check != nil {
			ready, details = check(r.Context())
		}

		resp := ReadinessResponse{
			Mode:     details.Mode,
			Role:     details.Role,
			Disk:     details.Disk,
			Term:     details.Term,
			LeaderID: details.LeaderID,
		}

		if ready {
			resp.Status = "READY"
			w.WriteHeader(http.StatusOK)
		} else {
			resp.Status = "DOWN"
			if details.Reason == "" {
				resp.Reason = "not_ready"
			} else {
				resp.Reason = details.Reason
			}
			w.WriteHeader(http.StatusServiceUnavailable)
		}

		_ = json.NewEncoder(w).Encode(resp)
	})
}
