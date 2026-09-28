// Package problem implements application/problem+json (RFC 9457) errors
// with the OBX error codes.
package problem

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
)

// FieldError is one entry of Problem.errors.
type FieldError struct {
	Pointer  *string `json:"pointer"`
	ObxError string  `json:"obx_error"`
	Detail   string  `json:"detail"`
}

// Problem is the error document.
type Problem struct {
	Type       string       `json:"type"`
	Title      string       `json:"title"`
	Status     int          `json:"status"`
	Detail     *string      `json:"detail"`
	Instance   *string      `json:"instance,omitempty"`
	ObxError   string       `json:"obx_error"`
	Errors     []FieldError `json:"errors,omitempty"`
	RetryAfter *int         `json:"retry_after,omitempty"`
}

func (p *Problem) Error() string {
	if p.Detail != nil {
		return p.ObxError + ": " + *p.Detail
	}
	return p.ObxError
}

var statuses = map[string]int{
	"invalid_request":        400,
	"validation_failed":      422,
	"unauthorized":           401,
	"invalid_token":          401,
	"access_revoked":         401,
	"insufficient_scope":     403,
	"forbidden":              403,
	"not_found":              404,
	"method_not_allowed":     405,
	"conflict":               409,
	"precondition_failed":    412,
	"payload_too_large":      413,
	"unsupported_media_type": 415,
	"rate_limited":           429,
	"limit_exceeded":         400,
	"cursor_invalid":         400,
	"cursor_expired":         410,
	"export_expired":         410,
	"original_not_allowed":   403,
	"invalid_gtin":           422,
	"gtin_conflict":          409,
	"position_conflict":      409,
	"checksum_mismatch":      422,
	"blob_missing":           422,
	"upload_incomplete":      409,
	"unknown_parent":         422,
	"files_missing":          422,
	"import_invalid":         422,
	"mass_unpublish_refused": 409,
	"unsafe_url":             422,
	"csrf_failed":            403,
	"invalid_credentials":    401,
	"account_blocked":        403,
	"setup_required":         409,
	"setup_completed":        409,
	"invite_invalid":         400,
	"not_implemented":        501,
	"internal_error":         500,
	"service_unavailable":    503,
}

var titles = map[string]string{
	"invalid_request":        "Invalid request",
	"validation_failed":      "Validation failed",
	"unauthorized":           "Authentication required",
	"invalid_token":          "Invalid or expired token",
	"access_revoked":         "Access revoked",
	"insufficient_scope":     "Insufficient scope",
	"forbidden":              "Forbidden",
	"not_found":              "Not found",
	"method_not_allowed":     "Method not allowed",
	"conflict":               "Conflict",
	"precondition_failed":    "Precondition failed",
	"payload_too_large":      "Payload too large",
	"unsupported_media_type": "Unsupported media type",
	"rate_limited":           "Too many requests",
	"limit_exceeded":         "Limit exceeded",
	"cursor_invalid":         "Invalid cursor",
	"cursor_expired":         "Cursor expired",
	"export_expired":         "Export expired",
	"original_not_allowed":   "Original not allowed",
	"invalid_gtin":           "Invalid GTIN",
	"gtin_conflict":          "GTIN already in use",
	"position_conflict":      "Position already in use",
	"checksum_mismatch":      "Checksum mismatch",
	"blob_missing":           "File not uploaded",
	"upload_incomplete":      "Upload incomplete",
	"unknown_parent":         "Unknown parent",
	"files_missing":          "Files missing",
	"import_invalid":         "Import file invalid",
	"mass_unpublish_refused": "Mass unpublish refused",
	"unsafe_url":             "Unsafe URL",
	"csrf_failed":            "CSRF check failed",
	"invalid_credentials":    "Invalid e-mail or password",
	"account_blocked":        "Account blocked",
	"setup_required":         "Setup required",
	"setup_completed":        "Setup already completed",
	"invite_invalid":         "Invitation or link invalid",
	"not_implemented":        "Not implemented",
	"internal_error":         "Internal error",
	"service_unavailable":    "Service unavailable",
}

// Status returns the HTTP status for an OBX error code.
func Status(code string) int {
	if s, ok := statuses[code]; ok {
		return s
	}
	return 500
}

// New creates a problem for code with a detail message.
func New(code, format string, args ...any) *Problem {
	d := format
	if len(args) > 0 {
		d = fmt.Sprintf(format, args...)
	}
	p := &Problem{
		Type:     "https://openbrandexchange.com/errors/" + code,
		Title:    titles[code],
		Status:   Status(code),
		ObxError: code,
	}
	if d != "" {
		p.Detail = &d
	}
	return p
}

// WithStatus overrides the HTTP status (setup_required is 409 or 503).
func (p *Problem) WithStatus(s int) *Problem { p.Status = s; return p }

// Field adds a field error (pointer may be "").
func (p *Problem) Field(pointer, code, detail string) *Problem {
	fe := FieldError{ObxError: code, Detail: detail}
	if pointer != "" || code != "" {
		ptr := pointer
		fe.Pointer = &ptr
	}
	p.Errors = append(p.Errors, fe)
	return p
}

// Write sends the problem as the response.
func Write(w http.ResponseWriter, p *Problem) {
	if p.RetryAfter != nil {
		w.Header().Set("Retry-After", strconv.Itoa(*p.RetryAfter))
	}
	if p.Status == 401 && w.Header().Get("WWW-Authenticate") == "" {
		w.Header().Set("WWW-Authenticate", `Bearer realm="obx"`)
	}
	w.Header().Set("Content-Type", "application/problem+json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(p.Status)
	_ = json.NewEncoder(w).Encode(p)
}

// As converts any error to a problem (unknown errors become internal_error
// without leaking details).
func As(err error) *Problem {
	if err == nil {
		return nil
	}
	if p, ok := err.(*Problem); ok {
		return p
	}
	return New("internal_error", "An unexpected error occurred.")
}
