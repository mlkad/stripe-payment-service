package handler

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
)

// maxAPIBytes caps ordinary JSON request bodies, which are small structs. The
// webhook route has its own, much larger limit; see stripe_handler.go.
const maxAPIBytes int64 = 32 << 10

// errorResponse is the single error envelope for the whole API. Handlers pass
// a message safe to show a caller; detail goes to the log instead.
type errorResponse struct {
	Error string `json:"error"`
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, errorResponse{Error: message})
}

// decodeJSON reads a size-limited body into dst and turns every failure mode
// into a message a caller can act on. Unknown fields are rejected so a typo in
// a client's payload surfaces as an error rather than a silently ignored value.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	if ct := r.Header.Get("Content-Type"); ct != "" && !strings.HasPrefix(ct, "application/json") {
		return errors.New("content-type must be application/json")
	}

	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxAPIBytes))
	dec.DisallowUnknownFields()

	if err := dec.Decode(dst); err != nil {
		return decodeError(err)
	}
	// A second value in the stream means the client sent something other than
	// the single object this endpoint accepts.
	if dec.More() {
		return errors.New("request body must contain a single JSON object")
	}
	return nil
}

// decodeError translates encoding/json's failures into messages that name the
// offending field where possible. The raw error is not returned: it embeds
// offsets and Go type names that mean nothing to an API consumer.
func decodeError(err error) error {
	var (
		syntaxErr   *json.SyntaxError
		typeErr     *json.UnmarshalTypeError
		tooLargeErr *http.MaxBytesError
	)
	switch {
	case errors.As(err, &tooLargeErr):
		return errors.New("request body too large")
	case errors.As(err, &syntaxErr):
		return errors.New("request body is not valid JSON")
	case errors.As(err, &typeErr):
		if typeErr.Field != "" {
			return errors.New("field " + typeErr.Field + " has the wrong type")
		}
		return errors.New("request body has a field of the wrong type")
	case errors.Is(err, io.EOF):
		return errors.New("request body is empty")
	case strings.HasPrefix(err.Error(), "json: unknown field "):
		return errors.New("unknown " + strings.TrimPrefix(err.Error(), "json: unknown "))
	default:
		return errors.New("request body is not valid JSON")
	}
}
