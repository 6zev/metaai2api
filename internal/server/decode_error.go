package server

import (
	"strings"
)

// decodeError is a client-facing description of why a config body was rejected.
//
// The decoder's own messages quote request text verbatim: encoding/json reports
// `unknown field "..."` with the caller's key, and a duration parse error quotes
// the caller's value. Both would echo a secret that exists only in the request
// being rejected, and no scrub list can help, because the value is not part of
// any configuration yet.
//
// So the detail is classified here and logged server-side (scrubbed), while the
// response carries only a fixed reason. The operator still learns what to fix;
// a client learns nothing about the payload.
type decodeError struct {
	// Reason is the stable, secret-free text returned to the caller.
	Reason string
	// Detail is the raw decoder error, for the server log only. It is scrubbed
	// before being written.
	Detail error
}

func (e *decodeError) Error() string { return e.Reason }
func (e *decodeError) Unwrap() error { return e.Detail }

// Classify turns a decoder failure into a reason that names the problem without
// reproducing the input.
func classifyDecodeError(err error) *decodeError {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "unknown field"):
		// Shape: json: unknown field "name". The name comes from the request.
		return &decodeError{Reason: "decode config: unknown field in the request body", Detail: err}
	case strings.Contains(msg, "invalid duration"):
		return &decodeError{Reason: "decode config: updater.interval is not a valid duration (for example \"24h\")", Detail: err}
	case strings.Contains(msg, "cannot unmarshal"):
		// Shape: json: cannot unmarshal string into Go struct field ... of type bool.
		// The type name is safe, but the whole message is logged rather than returned.
		return &decodeError{Reason: "decode config: a field has the wrong type", Detail: err}
	case strings.Contains(msg, "unexpected end of JSON input"):
		return &decodeError{Reason: "decode config: the request body is not complete JSON", Detail: err}
	default:
		return &decodeError{Reason: "decode config: the request body is not valid JSON", Detail: err}
	}
}

// errTrailingData is reported when a body decodes but has more content after it.
//
// It is a DECODE error, not an encode-provider profile, so it is built here and
// carries no request text.
func errTrailingData() *decodeError {
	return &decodeError{Reason: "decode config: unexpected trailing data"}
}
