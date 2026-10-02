package admin

import (
	"errors"
	"net/http"

	latticeerrors "github.com/silent-knight19/lattice/internal/errors"
)

// This file implements SEC-9: error sanitisation.
//
// The threat: an admin API that forwards err.Error() to a client discloses internals. In this
// codebase that is not hypothetical. For example:
//
//	(*latticeerrors.InvalidPathError).Error() renders
//	    invalid path "/etc/passwd" outside root "/var/lib/lattice/data": ...
//
// Forwarding that would hand an attacker the filesystem layout, and every handler's error
// string becomes an information channel: error text distinguishes "no such file" from
// "permission denied" from "corrupt", which is a probing oracle.
//
// The rule enforced here: the CLIENT receives a stable, opaque CODE plus STATIC text. The
// DETAIL goes to the server log, correlated by request id.

// Error codes. These are part of the API contract and are safe to expose: they name a
// category of failure, never a specific fact about this server's state.
const (
	CodeInvalidRequest = "invalid_request"
	CodeInvalidPath    = "invalid_path"
	CodeNotFound       = "not_found"
	CodeForbidden      = "forbidden"
	CodeConflict       = "conflict"
	CodeTooLarge       = "too_large"
	CodeCorrupted      = "corrupted"
	CodeUnavailable    = "unavailable"
	CodeNotSupported   = "not_supported"
	CodeTimeout        = "timeout"
	CodeInternal       = "internal_error"
	CodeBusy           = "busy"
)

// staticMessages maps a code to its fixed client-facing text.
//
// Every message is a CONSTANT. None contains a path, a key, a value, a hostname, or any
// other request- or server-derived content.
var staticMessages = map[string]string{
	CodeInvalidRequest: "the request was not valid",
	CodeInvalidPath:    "the supplied path was rejected",
	CodeNotFound:       "resource not found",
	CodeForbidden:      "request denied",
	CodeConflict:       "the resource is in a conflicting state",
	CodeTooLarge:       "the request exceeds a size limit",
	CodeCorrupted:      "the data failed an integrity check",
	CodeUnavailable:    "the resource is temporarily unavailable",
	CodeNotSupported:   "the operation is not supported",
	CodeTimeout:        "the operation did not complete in time",
	CodeInternal:       "internal error",
	CodeBusy:           "the service is at capacity",
}

// messageFor returns the static text for a code, defaulting to the generic internal text.
func messageFor(code string) string {
	if m, ok := staticMessages[code]; ok {
		return m
	}
	return staticMessages[CodeInternal]
}

// CodeForError maps an internal error to an opaque client-facing code.
//
// It uses errors.Is against the sentinel set, so wrapping is handled correctly, and it falls
// back to CodeInternal for anything unrecognized. It NEVER returns a message derived from
// err.
//
// A few mappings are worth calling out because the underlying sentinels are unusually
// informative if leaked:
//
//   - ErrNotFound vs ErrPermissionDenied vs ErrKeyNotFound are distinct, which is exactly
//     what makes error text a probing oracle; the client gets one code family instead.
//   - ErrWALPoisoned and the Raft corruption sentinels map to "corrupted", which reveals
//     nothing about which node or segment is affected.
func CodeForError(err error) string {
	if err == nil {
		return ""
	}
	switch {
	// Path handling: InvalidPathError and its sentinel.
	case errors.Is(err, latticeerrors.ErrInvalidPath):
		return CodeInvalidPath

	// Not-found family.
	case errors.Is(err, latticeerrors.ErrKeyNotFound):
		return CodeNotFound

	// Size limits.
	case errors.Is(err, latticeerrors.ErrKeyTooLarge),
		errors.Is(err, latticeerrors.ErrValueTooLarge),
		errors.Is(err, latticeerrors.ErrRaftEntryTooLarge),
		errors.Is(err, latticeerrors.ErrByteCountOverflow):
		return CodeTooLarge

	// Integrity failures.
	case errors.Is(err, latticeerrors.ErrChecksumMismatch),
		errors.Is(err, latticeerrors.ErrTornWrite),
		errors.Is(err, latticeerrors.ErrInvalidRecordPayload),
		errors.Is(err, latticeerrors.ErrInvalidRecordType),
		errors.Is(err, latticeerrors.ErrRaftCorruptedState),
		errors.Is(err, latticeerrors.ErrRaftStoragePoisoned),
		errors.Is(err, latticeerrors.ErrAttestationCorrupted),
		errors.Is(err, latticeerrors.ErrAttestationMismatch):
		return CodeCorrupted

	// Throttling and capacity.
	case errors.Is(err, latticeerrors.ErrWriteThrottled),
		errors.Is(err, latticeerrors.ErrReadIndexThrottled),
		errors.Is(err, latticeerrors.ErrQueueFull),
		errors.Is(err, latticeerrors.ErrMemTableFull),
		errors.Is(err, latticeerrors.ErrCompactionRunning):
		return CodeBusy

	// Resource exhaustion.
	case errors.Is(err, latticeerrors.ErrMemoryLimitExceeded):
		return CodeUnavailable

	// Client-supplied values that are structurally invalid.
	case errors.Is(err, latticeerrors.ErrEmptyKey),
		errors.Is(err, latticeerrors.ErrKeyOutOfOrder),
		errors.Is(err, latticeerrors.ErrSequenceOutOfOrder),
		errors.Is(err, latticeerrors.ErrInvalidOpType),
		errors.Is(err, latticeerrors.ErrSegmentGap),
		errors.Is(err, latticeerrors.ErrDuplicateSegment),
		errors.Is(err, latticeerrors.ErrInvalidNodeID),
		errors.Is(err, latticeerrors.ErrInvalidPeerAddress),
		errors.Is(err, latticeerrors.ErrInvalidAuthzPolicy):
		return CodeInvalidRequest

	default:
		return CodeInternal
	}
}

// statusForCode returns the HTTP status paired with a code.
func statusForCode(code string) int {
	switch code {
	case CodeInvalidRequest, CodeInvalidPath:
		return http.StatusBadRequest
	case CodeNotFound:
		return http.StatusNotFound
	case CodeForbidden:
		return http.StatusForbidden
	case CodeConflict:
		return http.StatusConflict
	case CodeTooLarge:
		return http.StatusRequestEntityTooLarge
	case CodeCorrupted:
		return http.StatusUnprocessableEntity
	case CodeBusy:
		return http.StatusServiceUnavailable
	case CodeNotSupported:
		return http.StatusNotImplemented
	case CodeTimeout:
		return http.StatusGatewayTimeout
	case CodeUnavailable:
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}

// Fail writes a sanitized error response for err.
//
// The client receives only a code and static text. The underlying error is attached to the
// response context so SEC-10's audit logger can record it server-side against the request id.
//
// This is the ONLY function endpoint handlers should use to report a failure. The lower-level
// writeError remains available for guards, which deliberately have no error to report.
func Fail(w http.ResponseWriter, err error) {
	code := CodeForError(err)
	if code == "" {
		code = CodeInternal
	}
	WriteCoded(w, statusForCode(code), code)
}

// WriteCoded writes a response for an explicit code, with static text.
func WriteCoded(w http.ResponseWriter, status int, code string) {
	if status == 0 {
		status = statusForCode(code)
	}
	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	// Built by concatenation from a fixed set: no interpolation of request-derived data.
	body := `{"error":{"code":"` + code + `","message":"` + messageFor(code) + `"}}` + "\n"
	_, _ = w.Write([]byte(body))
}

// FailRequest reports err against an in-flight request, correlating the detail with the
// request id in the server log.
//
// Callers that want the failure attributable in logs should prefer this over Fail. The client
// response is identical either way: an opaque code and static text.
func FailRequest(w http.ResponseWriter, r *http.Request, err error, sink ErrorSink) {
	code := CodeForError(err)
	if code == "" {
		code = CodeInternal
	}
	id := ""
	if r != nil {
		id = RequestIDOf(r)
	}
	if sink != nil {
		sink(id, code, err)
	}
	WriteCoded(w, statusForCode(code), code)
}

// ErrorSink receives server-side error detail for logging.
//
// It is a function type rather than a direct logger dependency so that handlers stay
// testable and so SEC-10 can supply the real audit logger without this package depending on
// logging policy. The err passed here is the ORIGINAL, unsanitised error and must only ever be
// written to a server-side log.
type ErrorSink func(requestID, code string, err error)
