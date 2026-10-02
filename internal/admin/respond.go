package admin

import "net/http"

// This file holds the shared error-response writers for the admin API.
//
// Security intent (SEC-9): every rejection is a CONSTANT body that discloses nothing
// about the server's configuration, its allowlists, or the secret value the caller
// supplied. Distinguishing "wrong Host" from "wrong Origin" from "wrong token" in the
// response would turn the admin API into an oracle, so all rejections share one shape.
//
// Responses also never carry Access-Control-* headers (SEC-1.3), which keeps the API
// unreadable from a foreign origin, and are always no-store so operational data is not
// cached by intermediaries.

// errorBody is the single rejection envelope used across the admin API.
type errorBody struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// writeError emits a rejection with the given HTTP status and opaque code/message.
//
// The caller must pass a STATIC message. Passing anything derived from the request or
// from internal error values would defeat the purpose of this helper.
func writeError(w http.ResponseWriter, status int, code, message string) {
	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	body := `{"error":{"code":"` + code + `","message":"` + message + `"}}` + "\n"
	_, _ = w.Write([]byte(body))
}

// writeMethodNotAllowed rejects a request that used an unsupported method on a route.
func writeMethodNotAllowed(w http.ResponseWriter) {
	writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed for this resource")
}

// writeInternalError rejects a request that failed unexpectedly.
//
// It deliberately does NOT include the underlying error: internal errors can carry
// filesystem paths, key material, or other details that must not reach a client.
func writeInternalError(w http.ResponseWriter) {
	writeError(w, http.StatusInternalServerError, "internal_error", "internal error")
}
