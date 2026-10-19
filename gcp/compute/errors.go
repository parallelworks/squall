package compute

import (
	"cmp"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/parallelworks/squall/fault"
)

// apiError is an error in the shape Google APIs answer with.
type apiError struct {
	Status  int
	Reason  string
	Message string
	// opErrors, when set, are the errors an operation that ends in this one lists.
	opErrors []operationError
}

func (e *apiError) Error() string { return fmt.Sprintf("%d %s: %s", e.Status, e.Reason, e.Message) }

func notFound(resource string) *apiError {
	return &apiError{Status: http.StatusNotFound, Reason: "notFound", Message: fmt.Sprintf("The resource '%s' was not found", resource)}
}

func alreadyExists(resource string) *apiError {
	return &apiError{Status: http.StatusConflict, Reason: "alreadyExists", Message: fmt.Sprintf("The resource '%s' already exists", resource)}
}

// inUse is the error for deleting or claiming what another resource uses.
func inUse(kind, resource, user string) *apiError {
	return &apiError{Status: http.StatusBadRequest, Reason: "resourceInUseByAnotherResource", Message: fmt.Sprintf("The %s resource '%s' is already being used by '%s'", kind, resource, user)}
}

func fingerprintMismatch() *apiError {
	return &apiError{Status: http.StatusPreconditionFailed, Reason: "conditionNotMet", Message: "Labels fingerprint either invalid or resource labels have changed"}
}

func notReady(resource string) *apiError {
	return &apiError{Status: http.StatusBadRequest, Reason: "resourceNotReady", Message: fmt.Sprintf("The resource '%s' is not ready", resource)}
}

func invalid(format string, args ...any) *apiError {
	return &apiError{Status: http.StatusBadRequest, Reason: "invalid", Message: fmt.Sprintf(format, args...)}
}

func faultError(f fault.Fault) *apiError {
	status := cmp.Or(f.Status, http.StatusServiceUnavailable)
	return &apiError{Status: status, Reason: cmp.Or(f.Reason, "backendError"), Message: fmt.Sprintf("squall: injected fault (%s)", f)}
}

type errorBody struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Code    int           `json:"code"`
	Message string        `json:"message"`
	Errors  []errorReason `json:"errors"`
}

type errorReason struct {
	Message string `json:"message"`
	Domain  string `json:"domain"`
	Reason  string `json:"reason"`
}

func writeError(w http.ResponseWriter, e *apiError) {
	writeJSON(w, e.Status, errorBody{errorDetail{
		Code:    e.Status,
		Message: e.Message,
		Errors:  []errorReason{{Message: e.Message, Domain: "global", Reason: e.Reason}},
	}})
}

// writeJSON answers with body. A body that writes itself, such as a list
// whose items are already encoded, is not encoded again.
func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	// Once the status is sent, a client that went away is all a write error can mean.
	if wt, ok := body.(io.WriterTo); ok {
		w.WriteHeader(status)
		_, _ = wt.WriteTo(w)
		return
	}
	b, ok := body.(json.RawMessage)
	if !ok {
		b = encoded(body)
	}
	w.WriteHeader(status)
	_, _ = w.Write(b)
}
